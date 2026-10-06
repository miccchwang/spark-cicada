// integration_test.go —— M-TEMPLATE 真库端到端测试。
//
// 运行方式：
//
//	SPARK_TEST_DB_DSN='postgres://user:pass@host:5432/spark_test?sslmode=disable' \
//	  go test ./internal/templatestore/ -run Integration -v
//
// ★ 跳过 vs 失败的纪律（与 groupstore / store 一致）：
//   - 本地未设 DSN ⇒ t.Skip，不阻塞日常开发。
//   - CI 设 SPARK_REQUIRE_DB=1 ⇒ 未设 DSN 时直接失败，绝不静默跳过。
//
// ★ 本文件要钉死的三件事（纯逻辑单测覆盖不到）：
//  1. queryState 是 **jsonb 原样往返**，不是被 base64 编码过的字符串。
//     （历史上 Template.QueryState 用过 []byte，json 会把它编成 base64 ——
//     前端拿到的是乱码，且发对象时直接 400。）
//  2. 「本页默认」的部分唯一索引真的生效：同 (scope,owner,page) 不能有两个默认。
//  3. team 档「无显式分享」时按 owner 主部门可见 —— 这条只在 SQL + 纯函数
//     串起来才跑得到。
package templatestore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/db"
	"github.com/miccchwang/spark-cicada/backend/internal/template"
)

func requireDB() bool {
	v := strings.TrimSpace(os.Getenv("SPARK_REQUIRE_DB"))
	return v == "1" || strings.EqualFold(v, "true")
}

func openTestDB(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		if requireDB() {
			t.Fatal("★ 真库集成测试被要求必须运行（SPARK_REQUIRE_DB=1），但未设置 SPARK_TEST_DB_DSN。")
		}
		t.Skip("未配置 SPARK_TEST_DB_DSN —— 跳过真库集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := filepath.Join("..", "..", "..", "sql", "migrations")
	migs, err := db.LoadMigrations(dir)
	if err != nil {
		t.Fatalf("加载迁移失败：%v", err)
	}
	m, err := db.NewMigrator(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败：%v", err)
	}
	if _, err := m.Up(ctx, migs); err != nil {
		m.Close()
		t.Fatalf("应用迁移失败：%v", err)
	}
	return New(m.Pool()), func() { m.Close() }
}

// ───────────────────────────── 帮手 ─────────────────────────────

// testSharedTenantID 取本测试库的 shared 档租户 id（没有就建一个）。
//
// ★ 0012 起 dim_org 主键 = (tenant_id, account)，账号不再全局唯一，
//
//	因此写 dim_org 必须显式带 tenant_id，且冲突目标要写成 (tenant_id, account)。
func testSharedTenantID(t *testing.T, ctx context.Context, s *Store) string {
	t.Helper()
	var id string
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM dim_tenant WHERE tier = 'shared' ORDER BY created_at LIMIT 1`).Scan(&id)
	if err == nil && id != "" {
		return id
	}
	const fixed = "00000000-0000-4000-8000-0000000000c1"
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO dim_tenant (id, code, name, tier, status, region)
		VALUES ($1, 'test-shared', 'test shared', 'shared', 'active', 'test')
		ON CONFLICT (id) DO NOTHING`, fixed)
	return fixed
}

func seedOrg(t *testing.T, ctx context.Context, s *Store, acct, dept string) {
	t.Helper()
	tid := testSharedTenantID(t, ctx, s)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO dim_org (tenant_id, account, display_name, tier, primary_dept)
		VALUES ($1, $2, $2, 'T3', $3)
		ON CONFLICT (tenant_id, account) DO UPDATE SET primary_dept = EXCLUDED.primary_dept`,
		tid, acct, dept); err != nil {
		t.Fatalf("建 dim_org %s: %v", acct, err)
	}
}

func cleanupTpl(ctx context.Context, s *Store, id string) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM dim_view_template WHERE id=$1`, id)
}

func cleanupOrg(ctx context.Context, s *Store, acct string) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM dim_view_template WHERE owner=$1`, acct)
	_, _ = s.pool.Exec(ctx, `DELETE FROM dim_org WHERE account=$1`, acct)
}

func mkTpl(id, owner, page string, scope template.Scope) *template.Template {
	return &template.Template{
		ID:         id,
		Name:       "测试模板 " + id,
		Scope:      scope,
		Owner:      owner,
		Page:       page,
		QueryState: json.RawMessage(`{"v":"1.0","time":{"mode":"preset","preset":"mtd"}}`),
		Columns:    []template.ColumnPref{{Key: "month", Visible: true, Order: 0}},
		Layout:     template.LayoutPref{Expanded: map[string]bool{"L0": true}},
		Version:    template.Version,
	}
}

// ───────────────────────────── 1. jsonb 原样往返 ─────────────────────────────

// TestIntegration_SchemaIsFromMigration0007 断言表确实来自 0007。
func TestIntegration_SchemaIsFromMigration0007(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()

	// 0007 建的表与视图必须存在
	for _, rel := range []string{"dim_view_template", "dim_view_template_share", "v_view_template_usage"} {
		var n int
		err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_class c
			JOIN pg_namespace ns ON ns.oid = c.relnamespace
			WHERE ns.nspname = 'public' AND c.relname = $1`, rel).Scan(&n)
		if err != nil {
			t.Fatalf("查 pg_class %s: %v", rel, err)
		}
		if n == 0 {
			t.Fatalf("★ 0007 应创建 %s，但真库里没有", rel)
		}
	}
}

// ★ 核心：queryState 必须**语义原样**存取，不能变成 base64。
//
// 注意「语义」而非「字节」：PostgreSQL 的 jsonb 会规范化 JSON ——
// 补空格、并且**重排对象键**（jsonb 内部按 key 长度+字典序存储）。
// 所以 `{"mode":"preset","grain":"month"}` 读回来可能变成
// `{"mode": "preset", "grain": "month"}`，甚至键顺序不同。
// 这是正常的、也是不可避免的（除非改用 json 类型，但那样失去索引与校验）。
//
// 因此断言分两层：
//  1. **反 base64**：读回来的必须是合法 JSON 对象（而不是一坨 base64 字符串）。
//     这条才是真正的回归护栏 —— 历史上 QueryState 用过 []byte，
//     json 会把它编成 base64，前端拿到乱码且发对象时直接 400。
//  2. **深相等**：解析后逐字段比对，保证内容未损坏、未丢字段。
func TestIntegration_QueryStateRoundTripRawJSON(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_raw"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)

	in := `{"v":"1.0","time":{"mode":"preset","preset":"mtd","grain":"month"},"filters":[{"field":"channel_code","op":"in","value":["TK-TH"]}]}`
	tpl := mkTpl("tpl_int_raw", acct, "/report", template.ScopePersonal)
	tpl.QueryState = json.RawMessage(in)
	defer cleanupTpl(ctx, s, "tpl_int_raw")

	if err := s.Save(ctx, tpl, acct); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, "tpl_int_raw")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// ① 反 base64：必须能当作 JSON 对象解析
	if !json.Valid(got.QueryState) {
		t.Fatalf("★ queryState 不是合法 JSON（可能被 base64 编码了）：%s", got.QueryState)
	}
	var gotObj map[string]any
	if err := json.Unmarshal(got.QueryState, &gotObj); err != nil {
		t.Fatalf("★ queryState 不是 JSON 对象：%v，原始=%s", err, got.QueryState)
	}
	// base64 的典型特征：整个值是字符串且能解出二进制。这里用「是否以 { 开头」快速排除。
	if len(got.QueryState) == 0 || got.QueryState[0] != '{' {
		t.Fatalf("★ queryState 应以 { 开头（不是 base64 字符串），得到 %.40s", got.QueryState)
	}

	// ② 深相等：语义必须完全一致
	var wantObj map[string]any
	if err := json.Unmarshal([]byte(in), &wantObj); err != nil {
		t.Fatalf("测试输入非法：%v", err)
	}
	if !deepEqualJSON(wantObj, gotObj) {
		t.Fatalf("★ queryState 语义不一致（内容损坏）：\n  期望 %v\n  得到 %v", wantObj, gotObj)
	}
	// 关键的深层字段抽查（防止 deepEqual 自身写错时漏检）
	tm, _ := gotObj["time"].(map[string]any)
	if tm == nil || tm["preset"] != "mtd" || tm["grain"] != "month" {
		t.Fatalf("time 字段损坏：%v", gotObj["time"])
	}
	fs, _ := gotObj["filters"].([]any)
	if len(fs) != 1 {
		t.Fatalf("filters 应保留 1 条，得到 %v", gotObj["filters"])
	}
	f0, _ := fs[0].(map[string]any)
	if f0 == nil || f0["field"] != "channel_code" || f0["op"] != "in" {
		t.Fatalf("filter 字段损坏：%v", fs[0])
	}
}

// deepEqualJSON 比较两个「已解析为 any 的 JSON」是否语义相等。
//
// 之所以不用 reflect.DeepEqual：PG jsonb 会把数字读成 float64，
// 而不同来源的数字类型可能不同；这里统一按 JSON 语义比。
func deepEqualJSON(a, b any) bool {
	ab, err1 := json.Marshal(normalizeJSON(a))
	bb, err2 := json.Marshal(normalizeJSON(b))
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// normalizeJSON 递归地对 map 的键排序，消除顺序差异。
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys)*2)
		for _, k := range keys {
			out = append(out, k, normalizeJSON(x[k]))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = normalizeJSON(x[i])
		}
		return out
	default:
		return v
	}
}

// ───────────────────────────── 2. 默认唯一性 ─────────────────────────────

// ★ 同 (scope, owner, page) 下不可能有两个 is_default（部分唯一索引 + SetDefault 事务）。
func TestIntegration_DefaultIsUniquePerOwnerPage(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_def"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)

	a := mkTpl("tpl_int_def_a", acct, "/report", template.ScopePersonal)
	b := mkTpl("tpl_int_def_b", acct, "/report", template.ScopePersonal)
	defer cleanupTpl(ctx, s, "tpl_int_def_a")
	defer cleanupTpl(ctx, s, "tpl_int_def_b")

	if err := s.Save(ctx, a, acct); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if err := s.Save(ctx, b, acct); err != nil {
		t.Fatalf("Save b: %v", err)
	}
	if err := s.SetDefault(ctx, a.ID); err != nil {
		t.Fatalf("SetDefault a: %v", err)
	}
	if err := s.SetDefault(ctx, b.ID); err != nil {
		t.Fatalf("SetDefault b: %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM dim_view_template
		WHERE scope='personal' AND owner=$1 AND page='/report' AND is_default`, acct).Scan(&n); err != nil {
		t.Fatalf("统计默认: %v", err)
	}
	if n != 1 {
		t.Fatalf("★ 同 owner+page 应恰好 1 个默认，得到 %d", n)
	}
	// 且应是后设的那个
	var which string
	if err := s.pool.QueryRow(ctx, `
		SELECT id FROM dim_view_template
		WHERE scope='personal' AND owner=$1 AND page='/report' AND is_default`, acct).Scan(&which); err != nil {
		t.Fatalf("取默认: %v", err)
	}
	if which != b.ID {
		t.Fatalf("★ 后设的默认应生效，期望 %s，得到 %s", b.ID, which)
	}
}

// ★ 部分唯一索引必须真的挡住「绕过 SetDefault 直接插两行默认」。
func TestIntegration_DefaultPartialIndexBlocksDirectInsert(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_idx"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)
	defer cleanupTpl(ctx, s, "tpl_int_idx_x")
	defer cleanupTpl(ctx, s, "tpl_int_idx_y")

	// 直接 INSERT 两行 is_default=true（绕过 SetDefault）⇒ 第二行必须撞唯一索引
	_, err := s.pool.Exec(ctx, `
		INSERT INTO dim_view_template
			(id, name, scope, owner, page, query_state, is_default)
		VALUES ('tpl_int_idx_x','x','personal',$1,'/report','{}'::jsonb, true)`, acct)
	if err != nil {
		t.Fatalf("插入首行默认应成功：%v", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO dim_view_template
			(id, name, scope, owner, page, query_state, is_default)
		VALUES ('tpl_int_idx_y','y','personal',$1,'/report','{}'::jsonb, true)`, acct)
	if err == nil {
		t.Fatal("★ 部分唯一索引应阻止第二行 is_default=true（否则会出现两个默认）")
	}
}

// ───────────────────────────── 3. 可见性（SQL + 纯函数串联） ─────────────────────────────

func TestIntegration_VisibleFor_PersonalInvisibleToOthers(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	seedOrg(t, ctx, s, "u_tpl_alice", "d.sales")
	seedOrg(t, ctx, s, "u_tpl_bob", "d.ops")
	defer cleanupOrg(ctx, s, "u_tpl_alice")
	defer cleanupOrg(ctx, s, "u_tpl_bob")

	a := mkTpl("tpl_int_alice", "u_tpl_alice", "/report", template.ScopePersonal)
	defer cleanupTpl(ctx, s, "tpl_int_alice")
	if err := s.Save(ctx, a, "u_tpl_alice"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// alice 自己看得到
	va, err := s.ResolveViewer(ctx, "u_tpl_alice", nil)
	if err != nil {
		t.Fatalf("ResolveViewer alice: %v", err)
	}
	la, err := s.VisibleFor(ctx, va, "/report")
	if err != nil {
		t.Fatalf("VisibleFor alice: %v", err)
	}
	if !hasTpl(la, "tpl_int_alice") {
		t.Fatal("本人应看到自己的个人模板")
	}

	// ★ bob 看不到（个人档）
	vb, err := s.ResolveViewer(ctx, "u_tpl_bob", nil)
	if err != nil {
		t.Fatalf("ResolveViewer bob: %v", err)
	}
	lb, err := s.VisibleFor(ctx, vb, "/report")
	if err != nil {
		t.Fatalf("VisibleFor bob: %v", err)
	}
	if hasTpl(lb, "tpl_int_alice") {
		t.Fatal("★ 他人个人模板不得可见")
	}
}

func TestIntegration_VisibleFor_TeamNoShareByOwnerDept(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	seedOrg(t, ctx, s, "u_tpl_mgr", "d.sales")
	seedOrg(t, ctx, s, "u_tpl_same", "d.sales")
	seedOrg(t, ctx, s, "u_tpl_other", "d.ops")
	defer cleanupOrg(ctx, s, "u_tpl_mgr")
	defer cleanupOrg(ctx, s, "u_tpl_same")
	defer cleanupOrg(ctx, s, "u_tpl_other")

	tpl := mkTpl("tpl_int_team", "u_tpl_mgr", "/report", template.ScopeTeam)
	defer cleanupTpl(ctx, s, "tpl_int_team")
	if err := s.Save(ctx, tpl, "u_tpl_mgr"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 同部门可见（team 档无显式分享 ⇒ 按 owner 主部门判定）
	vSame, _ := s.ResolveViewer(ctx, "u_tpl_same", nil)
	lSame, err := s.VisibleFor(ctx, vSame, "/report")
	if err != nil {
		t.Fatalf("VisibleFor same: %v", err)
	}
	if !hasTpl(lSame, "tpl_int_team") {
		t.Fatalf("★ 同部门应可见 team 模板（viewer dept=%q）", vSame.Dept)
	}

	// 异部门不可见
	vOther, _ := s.ResolveViewer(ctx, "u_tpl_other", nil)
	lOther, err := s.VisibleFor(ctx, vOther, "/report")
	if err != nil {
		t.Fatalf("VisibleFor other: %v", err)
	}
	if hasTpl(lOther, "tpl_int_team") {
		t.Fatal("★ 异部门不得可见 team 模板")
	}
}

func TestIntegration_VisibleFor_ExplicitShareOverridesDept(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	seedOrg(t, ctx, s, "u_tpl_own2", "d.sales")
	seedOrg(t, ctx, s, "u_tpl_samedept", "d.sales")
	seedOrg(t, ctx, s, "u_tpl_shared", "d.ops")
	defer cleanupOrg(ctx, s, "u_tpl_own2")
	defer cleanupOrg(ctx, s, "u_tpl_samedept")
	defer cleanupOrg(ctx, s, "u_tpl_shared")

	tpl := mkTpl("tpl_int_share", "u_tpl_own2", "/report", template.ScopeTeam)
	tpl.Shares = []template.Share{{SubjectKind: "dept", SubjectID: "d.ops"}}
	defer cleanupTpl(ctx, s, "tpl_int_share")
	if err := s.Save(ctx, tpl, "u_tpl_own2"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 显式分享给 d.ops ⇒ d.ops 可读
	vShared, _ := s.ResolveViewer(ctx, "u_tpl_shared", nil)
	lShared, _ := s.VisibleFor(ctx, vShared, "/report")
	if !hasTpl(lShared, "tpl_int_share") {
		t.Fatal("命中显式分享部门应可见")
	}

	// ★ 同部门但未被分享 ⇒ 不可见（显式分享后不再按同部门放行）
	vSame, _ := s.ResolveViewer(ctx, "u_tpl_samedept", nil)
	lSame, _ := s.VisibleFor(ctx, vSame, "/report")
	if hasTpl(lSame, "tpl_int_share") {
		t.Fatal("★ 显式分享后，未命中的同部门用户不得可见")
	}
}

// ───────────────────────────── 4. 页隔离 & 使用次数 ─────────────────────────────

func TestIntegration_VisibleFor_PageIsolation(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_page"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)

	r := mkTpl("tpl_int_r", acct, "/report", template.ScopePersonal)
	p := mkTpl("tpl_int_p", acct, "/pnl", template.ScopePersonal)
	defer cleanupTpl(ctx, s, "tpl_int_r")
	defer cleanupTpl(ctx, s, "tpl_int_p")
	if err := s.Save(ctx, r, acct); err != nil {
		t.Fatalf("Save r: %v", err)
	}
	if err := s.Save(ctx, p, acct); err != nil {
		t.Fatalf("Save p: %v", err)
	}

	v, _ := s.ResolveViewer(ctx, acct, nil)
	l, _ := s.VisibleFor(ctx, v, "/report")
	if !hasTpl(l, "tpl_int_r") || hasTpl(l, "tpl_int_p") {
		t.Fatal("★ 模板按页隔离：/report 列表不应含 /pnl 模板")
	}
}

func TestIntegration_BumpUse_IncrementsAndAudits(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_use"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)

	// ★ audit_log 是 append-only（0001 的触发器禁止 UPDATE/DELETE），
	//   因此本测试**不能**假设表是空的 —— 上一轮跑过的记录还在。
	//   用每次运行唯一的模板 id 来隔离统计范围，否则第二次跑就会看到 6 条。
	tplID := "tpl_int_use_" + time.Now().Format("20060102150405.000000000")

	tpl := mkTpl(tplID, acct, "/report", template.ScopePersonal)
	defer cleanupTpl(ctx, s, tplID)
	if err := s.Save(ctx, tpl, acct); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.BumpUse(ctx, tplID, acct); err != nil {
			t.Fatalf("BumpUse #%d: %v", i, err)
		}
	}
	got, err := s.Load(ctx, tplID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.UseCount != 3 {
		t.Fatalf("use_count 应为 3，得到 %d", got.UseCount)
	}
	// 审计留痕（append-only）：套用行为可事后追溯
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_log
		WHERE action='view_template.apply' AND target=$1 AND actor=$2`, tplID, acct).Scan(&n); err != nil {
		t.Fatalf("查审计: %v", err)
	}
	if n != 3 {
		t.Fatalf("★ 套用应留 3 条审计（本模板 %s），得到 %d", tplID, n)
	}
}

// ───────────────────────────── 5. 分享对象事务性 ─────────────────────────────

// ★ Save 必须原子地写「模板 + 分享」：若只写模板漏写分享，
//
//	team 档会从「只分享给 d.ops」退化成「同部门可见」= 静默扩大可见范围。
func TestIntegration_Save_SharesAreAtomic(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_atomic"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)

	tpl := mkTpl("tpl_int_atomic", acct, "/report", template.ScopeTeam)
	tpl.Shares = []template.Share{
		{SubjectKind: "dept", SubjectID: "d.ops"},
		{SubjectKind: "group", SubjectID: "grp_x"},
	}
	defer cleanupTpl(ctx, s, "tpl_int_atomic")
	if err := s.Save(ctx, tpl, acct); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, "tpl_int_atomic")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Shares) != 2 {
		t.Fatalf("★ 分享应有 2 条，得到 %d（漏写分享 = 静默扩大可见范围）", len(got.Shares))
	}
}

// ★ 全量替换语义：再次 Save 时旧分享必须被清掉，不能残留。
func TestIntegration_Save_ReplacesShares(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_repl"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)

	tpl := mkTpl("tpl_int_repl", acct, "/report", template.ScopeTeam)
	tpl.Shares = []template.Share{
		{SubjectKind: "dept", SubjectID: "d.a"},
		{SubjectKind: "dept", SubjectID: "d.b"},
	}
	defer cleanupTpl(ctx, s, "tpl_int_repl")
	if err := s.Save(ctx, tpl, acct); err != nil {
		t.Fatalf("Save #1: %v", err)
	}

	tpl.Shares = []template.Share{{SubjectKind: "dept", SubjectID: "d.c"}}
	if err := s.Save(ctx, tpl, acct); err != nil {
		t.Fatalf("Save #2: %v", err)
	}
	got, err := s.Load(ctx, "tpl_int_repl")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Shares) != 1 || got.Shares[0].SubjectID != "d.c" {
		t.Fatalf("★ 分享应被全量替换为 [d.c]，得到 %+v（残留旧分享 = 可见范围意外扩大）", got.Shares)
	}
}

// ───────────────────────────── 6. 删除 ─────────────────────────────

func TestIntegration_Delete_ScopedByOwner(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	seedOrg(t, ctx, s, "u_tpl_del_a", "d.ops")
	seedOrg(t, ctx, s, "u_tpl_del_b", "d.ops")
	defer cleanupOrg(ctx, s, "u_tpl_del_a")
	defer cleanupOrg(ctx, s, "u_tpl_del_b")

	tpl := mkTpl("tpl_int_del", "u_tpl_del_a", "/report", template.ScopePersonal)
	defer cleanupTpl(ctx, s, "tpl_int_del")
	if err := s.Save(ctx, tpl, "u_tpl_del_a"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 非 owner 删 ⇒ 返回 false 且不删
	ok, err := s.Delete(ctx, "tpl_int_del", "u_tpl_del_b", false)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok {
		t.Fatal("★ 非 owner 不应删成功")
	}
	if _, err := s.Load(ctx, "tpl_int_del"); err != nil {
		t.Fatal("★ 他人模板不得被删掉")
	}

	// owner 删 ⇒ true
	ok, err = s.Delete(ctx, "tpl_int_del", "u_tpl_del_a", false)
	if err != nil {
		t.Fatalf("Delete owner: %v", err)
	}
	if !ok {
		t.Fatal("owner 删应成功")
	}
	if _, err := s.Load(ctx, "tpl_int_del"); err == nil {
		t.Fatal("删除后不应还能读到")
	}
}

func TestIntegration_Delete_AdminOverridesOwner(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	seedOrg(t, ctx, s, "u_tpl_del_own", "d.ops")
	seedOrg(t, ctx, s, "u_tpl_del_admin", "d.it")
	defer cleanupOrg(ctx, s, "u_tpl_del_own")
	defer cleanupOrg(ctx, s, "u_tpl_del_admin")

	tpl := mkTpl("tpl_int_del2", "u_tpl_del_own", "/report", template.ScopePersonal)
	defer cleanupTpl(ctx, s, "tpl_int_del2")
	if err := s.Save(ctx, tpl, "u_tpl_del_own"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	ok, err := s.Delete(ctx, "tpl_int_del2", "u_tpl_del_admin", true)
	if err != nil {
		t.Fatalf("Delete admin: %v", err)
	}
	if !ok {
		t.Fatal("管理员应可删任意模板")
	}
}

// ───────────────────────────── 7. 校验落库 ─────────────────────────────

func TestIntegration_Save_RejectsInvalid(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_invalid"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)

	bad := mkTpl("tpl_int_bad", acct, "/report", template.ScopePersonal)
	bad.Name = "" // 非法：空名
	defer cleanupTpl(ctx, s, "tpl_int_bad")

	if err := s.Save(ctx, bad, acct); err == nil {
		t.Fatal("★ 空名应在 Save 层被拦（Validate 前置）")
	}
	// 且不应落库
	if _, err := s.Load(ctx, "tpl_int_bad"); err == nil {
		t.Fatal("非法模板不应落库")
	}
}

func TestIntegration_Save_UpsertUpdates(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_up"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)

	tpl := mkTpl("tpl_int_up", acct, "/report", template.ScopePersonal)
	defer cleanupTpl(ctx, s, "tpl_int_up")
	if err := s.Save(ctx, tpl, acct); err != nil {
		t.Fatalf("Save #1: %v", err)
	}
	tpl.Name = "改名后"
	tpl.QueryState = json.RawMessage(`{"v":"1.0","time":{"mode":"preset","preset":"ytd"}}`)
	if err := s.Save(ctx, tpl, acct); err != nil {
		t.Fatalf("Save #2: %v", err)
	}
	got, err := s.Load(ctx, "tpl_int_up")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != "改名后" {
		t.Fatalf("name 未更新：%s", got.Name)
	}
	if !strings.Contains(string(got.QueryState), "ytd") {
		t.Fatalf("queryState 未更新：%s", got.QueryState)
	}
	// upsert 不应产生第二行
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dim_view_template WHERE id='tpl_int_up'`).Scan(&n); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if n != 1 {
		t.Fatalf("upsert 应只有 1 行，得到 %d", n)
	}
}

// ───────────────────────────── 8. 排序 ─────────────────────────────

func TestIntegration_VisibleFor_SortedByUseCount(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()
	const acct = "u_tpl_sort"
	seedOrg(t, ctx, s, acct, "d.ops")
	defer cleanupOrg(ctx, s, acct)

	// ★ 用唯一前缀，避免上一轮遗留的同名模板混进统计
	//   （本测试按前缀筛同页模板，旧的同名行会让数目对不上）
	prefix := "tpl_int_s" + time.Now().Format("150405.000000000") + "_"
	ids := []string{prefix + "1", prefix + "2", prefix + "3"}

	for i, id := range ids {
		tpl := mkTpl(id, acct, "/report", template.ScopePersonal)
		defer cleanupTpl(ctx, s, id)
		if err := s.Save(ctx, tpl, acct); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
		// 分别用 1 / 5 / 3 次
		for j := 0; j < []int{1, 5, 3}[i]; j++ {
			if err := s.BumpUse(ctx, id, acct); err != nil {
				t.Fatalf("BumpUse: %v", err)
			}
		}
	}

	v, _ := s.ResolveViewer(ctx, acct, nil)
	l, err := s.VisibleFor(ctx, v, "/report")
	if err != nil {
		t.Fatalf("VisibleFor: %v", err)
	}
	// 取本测试新增的三条，验证相对顺序：第2个(5) > 第3个(3) > 第1个(1)
	var order []string
	for _, tpl := range l {
		if strings.HasPrefix(tpl.ID, prefix) {
			order = append(order, tpl.ID)
		}
	}
	want := []string{ids[1], ids[2], ids[0]}
	if len(order) != 3 {
		t.Fatalf("应取到 3 条，得到 %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("★ 应按常用度排序，期望 %v，得到 %v", want, order)
		}
	}
}

// ───────────────────────────── 帮手 ─────────────────────────────

func hasTpl(list []*template.Template, id string) bool {
	for _, t := range list {
		if t.ID == id {
			return true
		}
	}
	return false
}
