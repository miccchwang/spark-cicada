package tenant

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ───────────────────────────── 契约版本一致性 ─────────────────────────────

func TestContractVersionMatchesTS(t *testing.T) {
	// ⚠️ 若 contracts/tenant.ts 的 TENANT_CONTRACT_VERSION 变了，这里必须同步。
	// 真源比对由 web/test/contracts.test.mjs 负责（它读得到 TS 文件）；
	// 本测试负责在**没有前端构建**的纯 Go 环境里也钉住版本号，
	// 避免「后端跑起来了、前端还是老版本」这种半上线状态。
	if TenantContractVersion != "1.0" {
		t.Fatalf("契约版本漂移：Go 侧 %q，期望 1.0（同步改 contracts/tenant.ts）", TenantContractVersion)
	}
}

// ───────────────────────────── 档位值域（fail-open 入口） ─────────────────────────────

func TestIsolationTier_值域外的档位必须无效(t *testing.T) {
	valid := []IsolationTier{TierShared, TierDedicated}
	for _, tier := range valid {
		if !tier.Valid() {
			t.Errorf("档位 %q 应当有效", tier)
		}
	}
	// ★ 这些值都必须被判为无效。理由：若代码写成
	//     if tier == TierShared { 加 tenant 条件 }
	//   那么一个拼错/未迁移的档位值会**两个分支都不进**，
	//   从而静默跳过隔离 —— 这是 fail-open 的经典入口。
	for _, bad := range []IsolationTier{"", "Shared", "SHARED", "dedicatd", "independent", "shared ", "__proto__"} {
		if bad.Valid() {
			t.Errorf("档位 %q 不应被判为有效（否则会静默跳过隔离）", bad)
		}
	}
}

func TestHintPrecedence_claim必须高于host(t *testing.T) {
	// ★ 这条断言的是**安全属性**，不是实现细节：
	//   host 是用户可控输入，claim 由服务端签发。
	//   若有人把 host 排到 claim 前面，租户边界就交给用户决定了。
	idx := map[string]int{}
	for i, k := range HintPrecedence {
		idx[k] = i
	}
	if idx["claim"] > idx["header"] {
		t.Error("claim 必须优先于 header（已签名输入应先于显式头）")
	}
	if idx["header"] > idx["host"] {
		t.Error("header 必须优先于 host（host 是用户可控输入）")
	}
	if idx["host"] > idx["envDefault"] {
		t.Error("envDefault 必须排最后（它会让所有无标识请求落到同一租户）")
	}
	if idx["envDefault"] != len(HintPrecedence)-1 {
		t.Error("envDefault 必须是最后一位")
	}
}

// ───────────────────────────── 解析：正常路径 ─────────────────────────────

func mkLookup(t *Tenant) LookupFunc {
	return func(ctx context.Context, id string) (*Tenant, error) {
		if t == nil || t.ID != id {
			return nil, nil // 不存在
		}
		return t, nil
	}
}

const uidA = "11111111-1111-4111-8111-111111111111"
const uidB = "22222222-2222-4222-8222-222222222222"

func TestResolve_共享档(t *testing.T) {
	r := &Resolver{Lookup: mkLookup(&Tenant{
		ID: uidA, Code: "acme", Name: "Acme", Tier: TierShared, Status: StatusActive,
	})}
	res := r.Resolve(context.Background(), Hint{Claim: uidA})
	if !res.Allowed() {
		t.Fatalf("应当放行，实际拒绝：%s", res.Reason())
	}
	tc, err := res.Context()
	if err != nil {
		t.Fatal(err)
	}
	if tc.Tier != TierShared {
		t.Errorf("档位应为 shared，实际 %q", tc.Tier)
	}
	if tc.SchemaName != nil {
		t.Errorf("shared 档的 SchemaName 必须为 nil，实际 %q", *tc.SchemaName)
	}
	// ★ shared 档的 RLS 输入必须有值 —— 否则策略会求成空，可能放行全部
	if tc.RLSTenantID != uidA {
		t.Errorf("shared 档 RLS 输入必须等于租户 ID，实际 %q", tc.RLSTenantID)
	}
	if r.LastSource != "claim" {
		t.Errorf("应记录来源 claim，实际 %q", r.LastSource)
	}
}

func TestResolve_独立档(t *testing.T) {
	sn, err := SchemaNameFor(uidB)
	if err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Lookup: mkLookup(&Tenant{
		ID: uidB, Code: "bigcorp", Name: "BigCorp", Tier: TierDedicated, Status: StatusActive,
		SchemaName: &sn,
	})}
	res := r.Resolve(context.Background(), Hint{Claim: uidB})
	if !res.Allowed() {
		t.Fatalf("应当放行，实际拒绝：%s", res.Reason())
	}
	tc, _ := res.Context()
	if tc.SchemaName == nil || *tc.SchemaName != sn {
		t.Errorf("独立档 schema 应为 %q，实际 %v", sn, tc.SchemaName)
	}
}

// ───────────────────────────── 解析：fail-closed 全路径 ─────────────────────────────

func TestResolve_所有失败路径都必须拒绝(t *testing.T) {
	sn, _ := SchemaNameFor(uidA)
	cases := []struct {
		name   string
		hint   Hint
		lookup LookupFunc
		want   RejectReason
	}{
		{"无任何提示", Hint{}, mkLookup(nil), ReasonMissing},
		{"提示非 uuid", Hint{Claim: "acme"}, mkLookup(nil), ReasonMalformed},
		{"提示是注入串", Hint{Claim: "' OR 1=1--"}, mkLookup(nil), ReasonMalformed},
		{"库中不存在", Hint{Claim: uidA}, mkLookup(nil), ReasonNotFound},
		{"状态为 provisioning", Hint{Claim: uidA}, mkLookup(&Tenant{
			ID: uidA, Tier: TierShared, Status: StatusProvisioning}), ReasonNotActive},
		{"状态为 suspended", Hint{Claim: uidA}, mkLookup(&Tenant{
			ID: uidA, Tier: TierShared, Status: StatusSuspended}), ReasonNotActive},
		{"状态为 closed", Hint{Claim: uidA}, mkLookup(&Tenant{
			ID: uidA, Tier: TierShared, Status: StatusClosed}), ReasonNotActive},
		{"档位值域外", Hint{Claim: uidA}, mkLookup(&Tenant{
			ID: uidA, Tier: "dedicatd", Status: StatusActive}), ReasonInternal},
		{"独立档缺 schema", Hint{Claim: uidA}, mkLookup(&Tenant{
			ID: uidA, Tier: TierDedicated, Status: StatusActive, SchemaName: nil}), ReasonInternal},
		{"独立档 schema 为空串", Hint{Claim: uidA}, mkLookup(&Tenant{
			ID: uidA, Tier: TierDedicated, Status: StatusActive, SchemaName: strp("")}), ReasonInternal},
		{"共享档却带 schema", Hint{Claim: uidA}, mkLookup(&Tenant{
			ID: uidA, Tier: TierShared, Status: StatusActive, SchemaName: &sn}), ReasonInternal},
		{"库查询报错", Hint{Claim: uidA}, func(context.Context, string) (*Tenant, error) {
			return nil, errors.New("connection refused")
		}, ReasonInternal},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Resolver{Lookup: c.lookup}
			res := r.Resolve(context.Background(), c.hint)
			if res.Allowed() {
				t.Fatalf("★ 必须拒绝但放行了 —— 这是越权漏洞")
			}
			if res.Reason() != c.want {
				t.Errorf("拒绝原因应为 %q，实际 %q", c.want, res.Reason())
			}
			// 拒绝时绝不能给出可用的上下文
			if _, err := res.Context(); err == nil {
				t.Error("★ 拒绝结果不得提供可用上下文")
			} else if !errors.Is(err, ErrNoTenant) {
				t.Errorf("拒绝应返回 ErrNoTenant，实际 %v", err)
			}
		})
	}
}

func TestResolve_Lookup为nil时必须拒绝(t *testing.T) {
	// ★ 没有查询能力时**必须**拒绝，绝不能「放行但不过滤」。
	r := &Resolver{Lookup: nil}
	res := r.Resolve(context.Background(), Hint{Claim: uidA})
	if res.Allowed() {
		t.Fatal("★ Lookup 缺失时放行 = 所有租户数据互相可见")
	}
}

func TestResolve_默认禁止host来源(t *testing.T) {
	// ★ 零值 Resolver 的 DisallowHost 是 false，但这里显式验证 true 的行为，
	//   并把「生产应置 true」作为一条可执行断言固定下来。
	r := &Resolver{Lookup: mkLookup(&Tenant{
		ID: uidA, Tier: TierShared, Status: StatusActive}), DisallowHost: true}
	res := r.Resolve(context.Background(), Hint{Host: uidA})
	if res.Allowed() {
		t.Fatal("★ 禁用 host 来源后仍按 Host 解析出租户 —— 租户边界被交给用户")
	}
	// 同一租户用 claim 传入则应当放行（证明拒绝的是「来源」而非「这个租户」）
	res2 := r.Resolve(context.Background(), Hint{Claim: uidA})
	if !res2.Allowed() {
		t.Fatalf("claim 来源应放行，实际拒绝：%s", res2.Reason())
	}
}

func TestResolve_优先级claim压过host(t *testing.T) {
	// claim 指向 A、host 指向 B ⇒ 必须解析成 A
	snA, _ := SchemaNameFor(uidA)
	r := &Resolver{
		Lookup: func(_ context.Context, id string) (*Tenant, error) {
			switch id {
			case uidA:
				return &Tenant{ID: uidA, Tier: TierDedicated, Status: StatusActive, SchemaName: &snA}, nil
			case uidB:
				return &Tenant{ID: uidB, Tier: TierShared, Status: StatusActive}, nil
			}
			return nil, nil
		},
	}
	res := r.Resolve(context.Background(), Hint{Claim: uidA, Host: uidB})
	if !res.Allowed() {
		t.Fatalf("应放行：%s", res.Reason())
	}
	tc, _ := res.Context()
	if tc.TenantID != uidA {
		t.Fatalf("★ 应解析成 claim 指定的 %s，实际 %s（租户被 Host 覆盖）", uidA, tc.TenantID)
	}
}

// ───────────────────────────── schema 名安全 ─────────────────────────────

func TestSanitizeSchema_白名单(t *testing.T) {
	ok := []string{"t_abc", "t_1111111111111111", "t_a1"}
	for _, s := range ok {
		if _, err := SanitizeSchema(s); err != nil {
			t.Errorf("%q 应当合法：%v", s, err)
		}
	}
	bad := []string{
		"", "public", "pg_catalog", "information_schema", "pg_toast",
		"1abc", "T_ABC", "t_a-b", "t_a; DROP TABLE x", "t_a\"b",
		"t_" + strings.Repeat("x", 70),
		"abc",      // 缺前缀
		"tenant_a", // 缺前缀
	}
	for _, s := range bad {
		if got, err := SanitizeSchema(s); err == nil {
			t.Errorf("★ %q 应当被拒，实际通过为 %q", s, got)
		}
	}
}

func TestSchemaNameFor_由uuid派生(t *testing.T) {
	// 确定性：同一 uuid 永远同一 schema
	a1, err := SchemaNameFor(uidA)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := SchemaNameFor(uidA)
	if a1 != a2 {
		t.Errorf("派生应当确定，%q != %q", a1, a2)
	}
	// 不同 uuid 必须不同（否则两个租户共用一个 schema = 灾难）
	b1, err := SchemaNameFor(uidB)
	if err != nil {
		t.Fatal(err)
	}
	if a1 == b1 {
		t.Fatalf("★ 不同租户派生出同一 schema：%q", a1)
	}
	// 必须通过白名单
	if _, err := SanitizeSchema(a1); err != nil {
		t.Errorf("派生的 schema 名未通过白名单：%v", err)
	}
	// 非 uuid 必须拒绝
	if _, err := SchemaNameFor("acme"); err == nil {
		t.Error("非 uuid 不应能派生 schema 名")
	}
	// 大写 uuid 也应接受（规范化后一致）
	up, err := SchemaNameFor(strings.ToUpper(uidA))
	if err != nil {
		t.Fatalf("大写 uuid 应当接受：%v", err)
	}
	if up != a1 {
		t.Errorf("大小写 uuid 应派生出同一 schema：%q vs %q", up, a1)
	}
}

// ───────────────────────────── 缓存键隔离 ─────────────────────────────

func TestTenantCacheKey_必须带租户前缀(t *testing.T) {
	k1 := tenantCacheKeyForTest(uidA, "q=revenue", "2026-03")
	k2 := tenantCacheKeyForTest(uidB, "q=revenue", "2026-03")
	if k1 == k2 {
		t.Fatal("★ 不同租户的同一查询产生了相同缓存键 —— 会串租")
	}
	if !strings.HasPrefix(k1, uidA) {
		t.Errorf("租户 ID 必须在键最前，实际 %q", k1)
	}
	// 无租户 ⇒ 空串（永远 miss，安全退化）
	if k := tenantCacheKeyForTest("", "q=revenue"); k != "" {
		t.Errorf("无租户应返回空串，实际 %q", k)
	}
}

// ───────────────────────────── 会话变量 SQL 纪律 ─────────────────────────────

func TestSessionSQL_必须用LOCAL语义(t *testing.T) {
	// ★ SET LOCAL（set_config 第三个参数 true）只作用于当前事务。
	//   用会话级 SET 会让连接池把租户变量串给下一个请求 —— 串租。
	for name, sql := range map[string]string{
		"tenant":      SetLocalTenantSQL,
		"search_path": SearchPathSQL,
	} {
		if !strings.Contains(sql, "set_config") {
			t.Errorf("%s 应使用 set_config：%q", name, sql)
		}
		if !strings.Contains(sql, ", true)") {
			t.Errorf("★ %s 必须用 LOCAL 语义（set_config 第三参数 true）：%q", name, sql)
		}
		if !strings.Contains(sql, "$1") {
			t.Errorf("%s 必须参数化（$1），不得拼接：%q", name, sql)
		}
	}
	// 会话变量名必须一致（与 RLS 策略里的 current_setting 名字对齐）
	if !strings.Contains(SetLocalTenantSQL, "app.tenant_id") {
		t.Errorf("会话变量名应为 app.tenant_id：%q", SetLocalTenantSQL)
	}
}

func TestStatusServeable_只有active可服务(t *testing.T) {
	if !StatusActive.Serveable() {
		t.Error("active 应可服务")
	}
	for _, s := range []TenantStatus{StatusProvisioning, StatusSuspended, StatusClosed, "", "ACTIVE"} {
		if s.Serveable() {
			t.Errorf("★ 状态 %q 不应可服务", s)
		}
	}
}

// ───────────────────────────── 辅助 ─────────────────────────────

func strp(s string) *string { return &s }

// tenantCacheKeyForTest 是 TenantCacheKey 的别名（Task #57 后 Go 侧已有真实现）。
//
// ★ 历史：本 helper 曾**重新实现**契约规则，因为没有 Go 侧实现。
//   Task #57 引入了 tenant.TenantCacheKey（与 contracts/tenant.ts 的
//   tenantCacheKey 同构）后，保留一份「复刻」就成了**第三份真相** ——
//   真实现改了而复刻没改，测试会绿着放过漂移。
//   故此处直接转发到真实现：测试作用于生产代码，漂移不可能发生。
//
// ★ 与 TS 契约的一致性改由 cachekey_test.go 的
//   TestTenantCacheKey_分隔符与TS契约同构 看护（钉住 U+001F）。
func tenantCacheKeyForTest(tenantID string, parts ...string) string {
	return TenantCacheKey(tenantID, parts...)
}
