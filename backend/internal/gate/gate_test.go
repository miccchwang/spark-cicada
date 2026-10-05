// gate_test.go —— 验收闸门 G1–G12 的 CI 断言。
//
// 本文件是 docs/05-验收闸门.md 的「可执行镜像」：
// 每条 Test 对应闸门清单里的一行断言，失败即阻断合并。
package gate_test

import (
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

// ───────────────────────────── G2 ─────────────────────────────

func TestG2_DefaultCollapsed(t *testing.T) {
	levels := []contracts.LevelSummary{
		{Level: "L0", Key: "overview", DefaultExpanded: true},
		{Level: "L1", Key: "domain", DefaultExpanded: false},
		{Level: "L2", Key: "channel", DefaultExpanded: false},
		{Level: "L3", Key: "store", DefaultExpanded: false},
		{Level: "L4", Key: "sku", DefaultExpanded: false},
	}
	if v := gate.CheckDefaultCollapsed(levels); len(v) != 0 {
		t.Fatalf("G2 默认收起违规: %v", v)
	}
	// 反向：把 L2 置为展开应被判违规
	bad := append([]contracts.LevelSummary{}, levels...)
	bad[2].DefaultExpanded = true
	if v := gate.CheckDefaultCollapsed(bad); len(v) == 0 {
		t.Fatal("G2 应检出 L2 被错误展开")
	}
}

// ───────────────────────────── G3 ─────────────────────────────

func TestG3_NoZeroImputation(t *testing.T) {
	dc := &contracts.DataContract{
		V: contracts.DataContractVersion,
		Rows: []contracts.Row{
			{
				"month": contracts.NewStr("2026-09"),
				// gp 有值（真实计算）
				"gp": contracts.NewNum(12345.6),
				// cogs 缺失 ⇒ null（绝不补 0）
				"cogs": contracts.Null(),
			},
		},
		Columns: []contracts.ColumnDef{
			{Key: "month", Perm: "L1", Kind: "text"},
			{Key: "gp", Perm: "L3", Kind: "currency", AlgoID: "algo.gp"},
			{Key: "cogs", Perm: "L3", Kind: "currency", AlgoID: "algo.cogs"},
		},
		AlgoTrace: []contracts.AlgoTrace{
			{Field: "gp", AlgoID: "algo.gp", Skipped: false},
			{Field: "cogs", AlgoID: "algo.cogs", Skipped: true, Reason: "slot missing"},
		},
		Gaps: []contracts.DataGap{
			{Field: "cogs", Slot: "slot.cogs", Coverage: 0, Gate: 0.8, Reason: "missing"},
		},
	}
	if v := gate.CheckNoZeroImputation(dc, nil); len(v) != 0 {
		t.Fatalf("G3 缺失值违规: %v", v)
	}
	// 反向：把 cogs 补成 0 ⇒ 必须被检出
	dc.Rows[0]["cogs"] = contracts.NewNum(0)
	if v := gate.CheckNoZeroImputation(dc, nil); len(v) == 0 {
		t.Fatal("G3 应检出补 0 占位")
	}
	// 白名单为真实 0 ⇒ 合法
	if v := gate.CheckNoZeroImputation(dc, map[string]bool{"cogs": true}); len(v) == 0 {
		t.Fatal("G3 白名单内真实 0 不应被判违规")
	}
}

// ───────────────────────────── G4 ─────────────────────────────

func TestG4_AlgorithmNoDataSource(t *testing.T) {
	good := []gate.AlgoDoc{
		{ID: "algo.gp", Formula: "rev - cogs", DependsOnSlots: []string{"slot.rev", "slot.cogs"},
			Raw: map[string]any{"id": "algo.gp", "formula": "rev - cogs"}},
	}
	if v := gate.CheckAlgorithmNoDataSource(good); len(v) != 0 {
		t.Fatalf("G4 合法算法被误判: %v", v)
	}
	bad := []gate.AlgoDoc{
		{ID: "algo.gp", Raw: map[string]any{"id": "algo.gp", "table": "fact_sales_daily", "source": "db"}},
	}
	if v := gate.CheckAlgorithmNoDataSource(bad); len(v) == 0 {
		t.Fatal("G4 应检出算法混入数据源字段")
	}
}

func TestG4_SlotsRegistered(t *testing.T) {
	registered := map[string]bool{"slot.rev": true, "slot.cogs": true}
	docs := []gate.AlgoDoc{
		{ID: "algo.gp", DependsOnSlots: []string{"slot.rev", "slot.cogs"}},
	}
	if v := gate.CheckSlotsRegistered(docs, registered); len(v) != 0 {
		t.Fatalf("G4 引用完整性误判: %v", v)
	}
	docs = append(docs, gate.AlgoDoc{ID: "algo.gmp", DependsOnSlots: []string{"slot.affiliate"}})
	if v := gate.CheckSlotsRegistered(docs, registered); len(v) == 0 {
		t.Fatal("G4 应检出未注册槽 slot.affiliate")
	}
}

// ───────────────────────────── G5 ─────────────────────────────

func TestG5_CoverageGate(t *testing.T) {
	// 覆盖率达标
	ok := gate.DecideCoverage([]gate.CoverageCase{
		{SlotID: "slot.cogs", Coverage: 0.95, Gate: 0.80, Status: "ACTIVE"},
	})
	if ok.Skip {
		t.Fatalf("G5 达标覆盖率不应跳过: %s", ok.Reason)
	}
	// 覆盖率不足 ⇒ 跳过
	low := gate.DecideCoverage([]gate.CoverageCase{
		{SlotID: "slot.affiliate", Coverage: 0.42, Gate: 0.80, Status: "ACTIVE"},
	})
	if !low.Skip {
		t.Fatal("G5 覆盖率不足应跳过")
	}
	// 槽缺失 ⇒ 硬跳过
	miss := gate.DecideCoverage([]gate.CoverageCase{
		{SlotID: "slot.affiliate", Coverage: 0, Gate: 0.80, Status: "MISSING"},
	})
	if !miss.Skip {
		t.Fatal("G5 MISSING 槽应硬跳过")
	}
}

// ───────────────────────────── G6 ─────────────────────────────

func TestG6_VersionDrift(t *testing.T) {
	if d := gate.VersionDrift(map[string]int{"algo.gp": 3}, map[string]int{"algo.gp": 3}); len(d) != 0 {
		t.Fatalf("G6 版本一致不应漂移: %v", d)
	}
	if d := gate.VersionDrift(map[string]int{"algo.gp": 2}, map[string]int{"algo.gp": 3}); len(d) == 0 {
		t.Fatal("G6 应检出算法版本漂移")
	}
}

// ───────────────────────────── G7 ─────────────────────────────

func TestG7_DelegationNoOverflow(t *testing.T) {
	granter := &authz.EntitlementView{
		Modules: []string{"m.ops", "m.roi"}, MaxLevel: authz.L3,
		Dimensions: map[string][]string{"brand": {"KONVY"}},
	}
	// 合法代授：同级、子集模块、维度一致
	okGrantee := &authz.EntitlementView{
		Modules: []string{"m.ops"}, MaxLevel: authz.L2,
		Dimensions: map[string][]string{"brand": {"KONVY"}},
	}
	// 溢出代授：密级超上界 + 模块越权
	badGrantee := &authz.EntitlementView{
		Modules: []string{"m.ops", "m.secret"}, MaxLevel: authz.L4,
		Dimensions: map[string][]string{"brand": {"KONVY"}},
	}
	v := gate.CheckDelegationNoOverflow([]gate.DelegationCase{
		{Name: "legal", Granter: granter, Grantee: okGrantee, ExpectAllowed: true},
		{Name: "overflow", Granter: granter, Grantee: badGrantee, ExpectAllowed: false},
	})
	if len(v) != 0 {
		t.Fatalf("G7 代授不溢出断言失败: %v", v)
	}
}

func TestG7_GroupDenyPrecedence(t *testing.T) {
	res := authz.NewResolver(map[string]*authz.Entitlement{})
	// 账号显式允许字段 gp，但分组里 denied=[gp] ⇒ DENY 优先
	e := &authz.Entitlement{
		Account: "u1",
		FieldOverrides: []authz.FieldOverride{{Field: "gp", Allow: true}},
	}
	grp := &authz.Entitlement{
		Account: "grp.cost_profit",
		DataUseGroups: []authz.GroupScopeGrant{
			{Group: "grp.cost_profit", MaxLevel: authz.L3, Denied: []string{"gp"}},
		},
	}
	if v := gate.CheckGroupDenyPrecedence(res, e, []*authz.Entitlement{grp}, "gp"); len(v) != 0 {
		t.Fatalf("G7 DENY 优先断言失败: %v", v)
	}
}

func TestG7_ITSeesNoBusinessValues(t *testing.T) {
	res := authz.NewResolver(map[string]*authz.Entitlement{})
	// IT 账号即使被授予业务组，也不可见业务数值（D7 硬红线）
	it := &authz.Entitlement{
		Account:               "it.ops",
		BaseTemplate:          "tpl.it",
		CanViewBusinessValues: true, // 即便配置误置为 true
		DataUseGroups: []authz.GroupScopeGrant{
			{Group: "grp.cost_profit", MaxLevel: authz.L4},
		},
	}
	if v := gate.CheckITNoBusinessGroup(res, it, nil); len(v) != 0 {
		t.Fatalf("G7 IT 业务数值断言失败: %v", v)
	}
}

func TestG7_CosignTrigger(t *testing.T) {
	org := chain.NewOrgDirectory()
	org.Put(&chain.OrgNode{Account: "top", Tier: "T1", CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "vp", Supervisor: "top", Tier: "T2", PrimaryDept: "D1", CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "me", Supervisor: "vp", Tier: "T3", PrimaryDept: "D1", CanApprove: true, Active: true})

	ccp := chain.DefaultCcPolicy()

	// 低风险（L2、无敏感组、短期）⇒ 知会
	low, err := org.Route(chain.RouteInput{Applicant: "me", DraftLevel: "L2", ExpiryDays: 30}, ccp)
	if err != nil {
		t.Fatal(err)
	}
	if v := gate.CheckCosignTrigger(low, false); len(v) != 0 {
		t.Fatalf("G7 低风险误触发会签: %v", v)
	}
	// L4 ⇒ 会签
	l4, _ := org.Route(chain.RouteInput{Applicant: "me", DraftLevel: "L4", ExpiryDays: 30}, ccp)
	if v := gate.CheckCosignTrigger(l4, true); len(v) != 0 {
		t.Fatalf("G7 L4 应触发会签: %v", v)
	}
	// 含 grp.roi ⇒ 会签
	g, _ := org.Route(chain.RouteInput{Applicant: "me", DraftLevel: "L2", DraftGroups: []string{"grp.roi"}, ExpiryDays: 30}, ccp)
	if v := gate.CheckCosignTrigger(g, true); len(v) != 0 {
		t.Fatalf("G7 grp.roi 应触发会签: %v", v)
	}
	// 批量 ≥10 ⇒ 会签
	b, _ := org.Route(chain.RouteInput{Applicant: "me", DraftLevel: "L2", ExpiryDays: 30, BatchSize: 10}, ccp)
	if v := gate.CheckCosignTrigger(b, true); len(v) != 0 {
		t.Fatalf("G7 批量≥10 应触发会签: %v", v)
	}
	// 有效期 >90 ⇒ 会签
	e, _ := org.Route(chain.RouteInput{Applicant: "me", DraftLevel: "L2", ExpiryDays: 91}, ccp)
	if v := gate.CheckCosignTrigger(e, true); len(v) != 0 {
		t.Fatalf("G7 有效期>90 应触发会签: %v", v)
	}
}

func TestG7_PlusOnePlusTwo(t *testing.T) {
	org := chain.NewOrgDirectory()
	org.Put(&chain.OrgNode{Account: "top", Tier: "T1", CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "vp", Supervisor: "top", Tier: "T2", PrimaryDept: "D1", CanApprove: true, Active: true})
	// me 有虚线汇报 dl，但审批只走主属 vp
	org.Put(&chain.OrgNode{Account: "me", Supervisor: "vp", Tier: "T3", PrimaryDept: "D1",
		DottedLineSupervisors: []string{"dl"}, CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "dl", Supervisor: "top", Tier: "T2", PrimaryDept: "D2", CanApprove: true, Active: true})

	ac, err := org.Route(chain.RouteInput{Applicant: "me", DraftLevel: "L2", ExpiryDays: 30}, chain.DefaultCcPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if v := gate.CheckPlusOnePlusTwo(org, ac, "me"); len(v) != 0 {
		t.Fatalf("G7 +1审批/+2抄送 断言失败: %v", v)
	}
}

func TestG7_CosignVeto(t *testing.T) {
	org, res, ents := rbFixtures()
	svc := req.NewService(org, res, ents)
	svc.CcPolicy = chain.DefaultCcPolicy()

	// 构造 L4 ⇒ 会签
	r := &req.Request{
		ID: "R1", Applicant: "me",
		Draft: req.Draft{MaxLevel: authz.L4},
	}
	v, err := svc.Submit(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK {
		t.Fatalf("提交应通过: %s", v.Message)
	}
	if r.Status != req.StatusCosignPending {
		t.Fatalf("L4 应进入 COSIGN_PENDING，实际=%s", r.Status)
	}
	// 找一个会签人否决
	vetoBy := ""
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign {
			vetoBy = cc.Cc
			break
		}
	}
	if vetoBy == "" {
		t.Fatal("会签列表为空")
	}
	status, viol := gate.CheckCosignVeto(svc, r, vetoBy)
	if len(viol) != 0 {
		t.Fatalf("G7 会签否决断言失败: %v (status=%s)", viol, status)
	}
}

func TestG7_CrossDeptNotSelfServiceable(t *testing.T) {
	dc := &chain.DataChain{CrossDept: true, Depts: []string{"D1", "D2"}}
	if v := gate.CheckCrossDeptSelfService(dc, chain.DefaultCrossDeptPolicy()); len(v) != 0 {
		t.Fatalf("G7 跨部门自助申请断言失败: %v", v)
	}
}

// ───────────────────────────── G10 ─────────────────────────────

func TestG10_AuditAppendOnly(t *testing.T) {
	if v := gate.CheckAuditAppendOnly("INSERT"); len(v) != 0 {
		t.Fatalf("G10 INSERT 应被允许: %v", v)
	}
	for _, op := range []string{"UPDATE", "DELETE"} {
		if v := gate.CheckAuditAppendOnly(op); len(v) == 0 {
			t.Fatalf("G10 %s 应被拒绝（append-only）", op)
		}
	}
}

// ───────────────────────────── G11 ─────────────────────────────

func TestG11_NoCredentialsInURL(t *testing.T) {
	bad := []string{
		"https://user:pa55@example.com/api",
		"postgres://admin:s3cret@db.internal:5432/spark",
	}
	for _, u := range bad {
		if v := gate.CheckNoCredentialsInURL(u); len(v) == 0 {
			t.Fatalf("G11 应检出 URL 凭据：%s", u)
		}
	}
	good := []string{
		"https://example.com/api",
		"https://example.com/cb?code=abc",
		"postgres://db.internal:5432/spark",
	}
	for _, u := range good {
		if v := gate.CheckNoCredentialsInURL(u); len(v) != 0 {
			t.Fatalf("G11 误报合法 URL：%s → %v", u, v)
		}
	}
}

func TestG11_NoSecretsInArtifacts(t *testing.T) {
	hits := gate.CheckNoSecretsInArtifacts("dist/config.json",
		`{"oauth_client_secret":"abc123","api_key":"xyz"}`)
	if len(hits) < 2 {
		t.Fatalf("G11 应检出 client_secret 与 api_key，实际 %v", hits)
	}
	clean := gate.CheckNoSecretsInArtifacts("dist/app.js", `console.log("hello")`)
	if len(clean) != 0 {
		t.Fatalf("G11 误报干净产物: %v", clean)
	}
}

// ───────────────────────────── G12 ─────────────────────────────

func TestG12_MonthlyQuotaPerRegion(t *testing.T) {
	// 新加坡 & 美国各自独立计数
	sg := gate.QuotaKey("ap-southeast-1", "2026-10")
	us := gate.QuotaKey("us-east-1", "2026-10")
	if sg == us {
		t.Fatal("G12 配额键必须分地域")
	}
	if ok, _ := gate.CheckMonthlyQuota(0); !ok {
		t.Fatal("G12 首次下载应允许")
	}
	if ok, reason := gate.CheckMonthlyQuota(1); ok {
		t.Fatalf("G12 同域同月第二次下载应被拒，reason=%s", reason)
	}
}

func TestG12_FencingBeforeFailover(t *testing.T) {
	good := gate.Fencing{OldPrimaryWriteBlocked: true, WitnessAcquired: true, LSNReconciled: true}
	if v := gate.CheckFailoverFencing(good); len(v) != 0 {
		t.Fatalf("G12 完备 fencing 不应报错: %v", v)
	}
	// 跳过 fencing ⇒ 必须被拒
	bad := gate.Fencing{OldPrimaryWriteBlocked: false, WitnessAcquired: true, LSNReconciled: true}
	if v := gate.CheckFailoverFencing(bad); len(v) == 0 {
		t.Fatal("G12 未隔离旧主写通道应被拒")
	}
}

func TestG12_NoCrossRegionWrite(t *testing.T) {
	if v := gate.CheckNoCrossRegionWrite("ap-southeast-1", "ap-southeast-1"); len(v) != 0 {
		t.Fatalf("G12 同域写不应报错: %v", v)
	}
	if v := gate.CheckNoCrossRegionWrite("us-east-1", "ap-southeast-1"); len(v) == 0 {
		t.Fatal("G12 跨地域写同一行应被拒")
	}
}

func TestG12_RollbackKeepsAudit(t *testing.T) {
	if v := gate.CheckRollbackKeepsAudit(100, 100); len(v) != 0 {
		t.Fatalf("G12 回滚不动审计（持平）不应报错: %v", v)
	}
	if v := gate.CheckRollbackKeepsAudit(100, 99); len(v) == 0 {
		t.Fatal("G12 回滚后审计行数减少应报错")
	}
}

// ───────────────────────────── 共享夹具 ─────────────────────────────

// rbFixtures 构造一套最小组织/权限夹具：top ← vp ← me。
func rbFixtures() (*chain.OrgDirectory, *authz.Resolver, func(string) (*authz.Entitlement, []*authz.Entitlement)) {
	org := chain.NewOrgDirectory()
	org.Put(&chain.OrgNode{Account: "top", Tier: "T1", PrimaryDept: "HQ",
		CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "vp", Supervisor: "top", Tier: "T2", PrimaryDept: "D1",
		CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "me", Supervisor: "vp", Tier: "T3", PrimaryDept: "D1",
		CanApprove: true, Active: true})

	templates := map[string]*authz.Entitlement{
		"tpl.lead": {
			Account:  "tpl.lead",
			Modules:  authz.ModuleGrant{Enabled: []string{"m.ops", "m.roi", "m.inventory"}},
			MaxLevel: authz.L4,
			Dimensions: []authz.DimensionGrant{{Dim: "brand", Values: nil}},
			DataUseGroups: []authz.GroupScopeGrant{
				{Group: "grp.ops", MaxLevel: authz.L4},
				{Group: "grp.cost_profit", MaxLevel: authz.L4},
				{Group: "grp.inventory", MaxLevel: authz.L4},
				{Group: "grp.roi", MaxLevel: authz.L4},
			},
		},
	}
	res := authz.NewResolver(templates)
	res.Now = func() time.Time { return time.Now() }

	ents := func(account string) (*authz.Entitlement, []*authz.Entitlement) {
		switch account {
		case "top":
			return &authz.Entitlement{Account: "top", BaseTemplate: "tpl.lead"}, nil
		case "vp":
			return &authz.Entitlement{Account: "vp", BaseTemplate: "tpl.lead"}, nil
		case "me":
			return &authz.Entitlement{Account: "me", BaseTemplate: "tpl.lead"}, nil
		}
		return &authz.Entitlement{Account: account}, nil
	}
	return org, res, ents
}
