package template

import (
	"encoding/json"
	"testing"
	"time"
)

// ───────────────────────────── 测试帮手 ─────────────────────────────

func mkTemplate(scope Scope, owner, page string) *Template {
	return &Template{
		ID:         "tpl_" + string(scope) + "_" + owner + "_" + page,
		Name:       "我的视图",
		Scope:      scope,
		Owner:      owner,
		Page:       page,
		QueryState: []byte(`{"v":"1.0"}`),
		Columns:    []ColumnPref{{Key: "month", Visible: true, Order: 0}},
		Layout:     LayoutPref{Expanded: map[string]bool{"L0": true}},
		Version:    Version,
		CreatedAt:  time.Unix(1000, 0).UTC(),
		UpdatedAt:  time.Unix(1000, 0).UTC(),
	}
}

func viewer(acct string, opts ...func(*Viewer)) Viewer {
	v := Viewer{Account: acct}
	for _, o := range opts {
		o(&v)
	}
	return v
}

func inDept(d string) func(*Viewer) { return func(v *Viewer) { v.Dept = d } }
func inGroups(gs ...string) func(*Viewer) {
	return func(v *Viewer) { v.Groups = append(v.Groups, gs...) }
}
func asAdmin() func(*Viewer) { return func(v *Viewer) { v.IsSystemAdmin = true } }

// ───────────────────────────── Validate ─────────────────────────────

func TestValidate_Good(t *testing.T) {
	if p := Validate(mkTemplate(ScopePersonal, "u.alice", "/report")); len(p) != 0 {
		t.Fatalf("期望合法，得到问题：%v", p)
	}
}

func TestValidate_NameRequired(t *testing.T) {
	// 「自命名」是需求能力，空名意味着模板列表无法辨识 —— 必须拦。
	tpl := mkTemplate(ScopePersonal, "u.alice", "/report")
	tpl.Name = "   "
	if p := Validate(tpl); len(p) == 0 {
		t.Fatal("★ 空名应被拒（模板需用户自命名）")
	}
}

func TestValidate_BadScope(t *testing.T) {
	tpl := mkTemplate(ScopePersonal, "u.alice", "/report")
	tpl.Scope = "public"
	if p := Validate(tpl); len(p) == 0 {
		t.Fatal("非法档位应被拒")
	}
}

func TestValidate_QueryStateRequired(t *testing.T) {
	tpl := mkTemplate(ScopePersonal, "u.alice", "/report")
	tpl.QueryState = nil
	if p := Validate(tpl); len(p) == 0 {
		t.Fatal("★ 空 QueryState 应被拒（模板的本质就是持久化的 QueryState）")
	}
}

func TestValidate_PageRequired(t *testing.T) {
	tpl := mkTemplate(ScopePersonal, "u.alice", "")
	if p := Validate(tpl); len(p) == 0 {
		t.Fatal("page 不能为空")
	}
}

func TestValidate_DuplicateColumnOrder(t *testing.T) {
	tpl := mkTemplate(ScopePersonal, "u.alice", "/report")
	tpl.Columns = []ColumnPref{
		{Key: "a", Order: 0},
		{Key: "b", Order: 0}, // 重复
	}
	if p := Validate(tpl); len(p) == 0 {
		t.Fatal("★ 列 order 重复应被拒（否则前端排序不确定）")
	}
}

func TestValidate_BadShare(t *testing.T) {
	tpl := mkTemplate(ScopeTeam, "u.mgr", "/report")
	tpl.Shares = []Share{{SubjectKind: "team", SubjectID: "x"}}
	if p := Validate(tpl); len(p) == 0 {
		t.Fatal("非法分享对象 kind 应被拒")
	}
}

// ───────────────────────────── CanRead：三档可见性 ─────────────────────────────

func TestCanRead_Personal_OnlyOwner(t *testing.T) {
	tpl := mkTemplate(ScopePersonal, "u.alice", "/report")

	// 本人可见
	if !CanRead(tpl, viewer("u.alice")) {
		t.Fatal("本人应可读自己的个人模板")
	}
	// ★ 同部门同事不可见 —— 个人档若被别人看到，等于习惯设置互相泄露
	if CanRead(tpl, viewer("u.bob", inDept("D1"))) {
		t.Fatal("★ 个人档不得被他读（含同部门）")
	}
	// 同组也不可见
	if CanRead(tpl, viewer("u.bob", inGroups("grp_sales"))) {
		t.Fatal("★ 个人档不得被同组读")
	}
}

func TestCanRead_System_Everyone(t *testing.T) {
	tpl := mkTemplate(ScopeSystem, "u.it", "/report")
	for _, acct := range []string{"u.alice", "u.bob", "u.nobody"} {
		if !CanRead(tpl, viewer(acct)) {
			t.Fatalf("★ 系统档应对全体可见，%s 却被拒", acct)
		}
	}
}

func TestCanRead_Team_ExplicitDeptShare(t *testing.T) {
	tpl := mkTemplate(ScopeTeam, "u.mgr", "/report")
	tpl.Shares = []Share{{SubjectKind: "dept", SubjectID: "D_SALES"}}

	if !CanRead(tpl, viewer("u.alice", inDept("D_SALES"))) {
		t.Fatal("命中部门分享应可读")
	}
	if CanRead(tpl, viewer("u.bob", inDept("D_OTHER"))) {
		t.Fatal("★ 非分享部门不得可读")
	}
}

func TestCanRead_Team_ExplicitGroupShare(t *testing.T) {
	tpl := mkTemplate(ScopeTeam, "u.mgr", "/report")
	tpl.Shares = []Share{{SubjectKind: "group", SubjectID: "grp_ops"}}

	if !CanRead(tpl, viewer("u.alice", inGroups("grp_ops"))) {
		t.Fatal("命中组分享应可读")
	}
	// ★ 一人多组取并集：只要有一个组命中即可
	if !CanRead(tpl, viewer("u.alice", inGroups("grp_a", "grp_ops", "grp_b"))) {
		t.Fatal("多组并集命中应可读")
	}
	if CanRead(tpl, viewer("u.bob", inGroups("grp_other"))) {
		t.Fatal("未命中任何组不得可读")
	}
}

func TestCanRead_Team_ExplicitShareDoesNotFallBackToDept(t *testing.T) {
	// ★ 一旦显式指定了分享对象，就不再「顺带」按同部门放行 ——
	//   否则「只分享给 grp_ops」的心智模型会被悄悄破坏。
	tpl := mkTemplate(ScopeTeam, "u.mgr", "/report")
	tpl.Shares = []Share{{SubjectKind: "group", SubjectID: "grp_ops"}}

	sameDept := viewer("u.alice", inDept("D_SALES"))
	if CanRead(tpl, sameDept) {
		t.Fatal("★ 显式分享后不得再按同部门放行")
	}
}

func TestCanRead_Team_NoShare_OwnerDeptOnly(t *testing.T) {
	tpl := mkTemplate(ScopeTeam, "u.mgr", "/report")
	// 无 shares ⇒ 用 owner 的主部门做同部门判定
	if !CanReadTeamWithOwnerDept(tpl, viewer("u.alice", inDept("D_SALES")), "D_SALES") {
		t.Fatal("同部门应可读（无显式分享时）")
	}
	if CanReadTeamWithOwnerDept(tpl, viewer("u.bob", inDept("D_OTHER")), "D_SALES") {
		t.Fatal("★ 异部门不得可读")
	}
	// owner 本人始终可读（哪怕部门对不上）
	if !CanReadTeamWithOwnerDept(tpl, viewer("u.mgr", inDept("D_X")), "D_SALES") {
		t.Fatal("owner 本人应可读")
	}
	// 单纯 CanRead（没有 owner dept 上下文）在无分享时应保守拒绝
	if CanRead(tpl, viewer("u.alice", inDept("D_SALES"))) {
		t.Fatal("无 owner dept 上下文时 CanRead 应保守拒绝，交由 WithOwnerDept 判定")
	}
}

func TestCanRead_AdminSeesAll(t *testing.T) {
	for _, sc := range []Scope{ScopePersonal, ScopeTeam, ScopeSystem} {
		tpl := mkTemplate(sc, "u.someone", "/report")
		if !CanRead(tpl, viewer("u.it", asAdmin())) {
			t.Fatalf("★ 管理员应可读 %s 档（便于治理）", sc)
		}
	}
}

func TestCanRead_NilSafe(t *testing.T) {
	if CanRead(nil, viewer("u.a")) {
		t.Fatal("nil 模板应为不可读")
	}
	if CanReadTeamWithOwnerDept(nil, viewer("u.a"), "D1") {
		t.Fatal("nil 模板应为不可读")
	}
}

// ───────────────────────────── CanWrite：比读更严 ─────────────────────────────

func TestCanWrite_OnlyOwnerOrAdmin(t *testing.T) {
	tpl := mkTemplate(ScopeTeam, "u.mgr", "/report")
	tpl.Shares = []Share{{SubjectKind: "dept", SubjectID: "D_SALES"}}

	// 「读得到」的人未必「改得动」
	reader := viewer("u.alice", inDept("D_SALES"))
	if !CanRead(tpl, reader) {
		t.Fatal("前提：该用户应可读")
	}
	if CanWrite(tpl, reader) {
		t.Fatal("★ 可读 ≠ 可写：非 owner 不得改他人模板")
	}
	if !CanWrite(tpl, viewer("u.mgr")) {
		t.Fatal("owner 应可写")
	}
	if !CanWrite(tpl, viewer("u.it", asAdmin())) {
		t.Fatal("管理员应可写")
	}
	// system 档也遵循「可读 ≠ 可写」—— 否则任何人可改所有人的默认视图
	sysTpl := mkTemplate(ScopeSystem, "u.it", "/report")
	if CanWrite(sysTpl, viewer("u.alice")) {
		t.Fatal("★ 系统档对全体可读，但只有管理员可写")
	}
}

// ───────────────────────────── CanSetScope：建模板时的档位授权 ─────────────────────────────

func TestCanSetScope(t *testing.T) {
	cases := []struct {
		name         string
		scope        Scope
		v            Viewer
		isSupervisor bool
		want         bool
	}{
		{"个人-普通用户", ScopePersonal, viewer("u.a"), false, true},
		{"团队-普通用户", ScopeTeam, viewer("u.a"), false, false},
		{"团队-主管", ScopeTeam, viewer("u.mgr"), true, true},
		{"系统-主管", ScopeSystem, viewer("u.mgr"), true, false},
		{"系统-管理员", ScopeSystem, viewer("u.it", asAdmin()), false, true},
		{"未知档位", Scope("x"), viewer("u.a"), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CanSetScope(c.scope, c.v, c.isSupervisor); got != c.want {
				t.Fatalf("CanSetScope(%s)=%v，期望 %v", c.scope, got, c.want)
			}
		})
	}
}

// ───────────────────────────── FilterReadable ─────────────────────────────

func TestFilterReadable(t *testing.T) {
	all := []*Template{
		mkTemplate(ScopePersonal, "u.alice", "/report"),
		mkTemplate(ScopePersonal, "u.bob", "/report"),
		mkTemplate(ScopeSystem, "u.it", "/report"),
		mkTemplate(ScopeTeam, "u.mgr", "/report"),
	}
	got := FilterReadable(all, viewer("u.alice"))
	// alice 应看到：自己的 personal + system；看不到 bob 的 personal 与 mgr 的 team
	if len(got) != 2 {
		t.Fatalf("期望 2 条可读，得到 %d", len(got))
	}
	for _, g := range got {
		if g.Owner == "u.bob" {
			t.Fatal("★ 不得读到他人个人模板")
		}
		if g.Scope == ScopeTeam {
			t.Fatal("★ 无分享的 team 模板不得被局外人读")
		}
	}
}

// ───────────────────────────── SortByUseCountDesc ─────────────────────────────

func TestSortByUseCountDesc(t *testing.T) {
	a := mkTemplate(ScopePersonal, "u.a", "/report")
	a.ID, a.UseCount = "tpl_a", 3
	b := mkTemplate(ScopePersonal, "u.b", "/report")
	b.ID, b.UseCount = "tpl_b", 10
	c := mkTemplate(ScopePersonal, "u.c", "/report")
	c.ID, c.UseCount = "tpl_c", 1

	got := SortByUseCountDesc([]*Template{a, b, c})
	want := []string{"tpl_b", "tpl_a", "tpl_c"}
	for i, g := range got {
		if g.ID != want[i] {
			t.Fatalf("第 %d 位应为 %s，得到 %s", i, want[i], g.ID)
		}
	}
}

func TestSortByUseCountDesc_StableTiebreak(t *testing.T) {
	// 次数与更新时间都相同时，用 id 字典序，保证输出确定（列表不会自己跳动）
	x := mkTemplate(ScopePersonal, "u.x", "/report")
	x.ID, x.UseCount = "tpl_z", 5
	y := mkTemplate(ScopePersonal, "u.y", "/report")
	y.ID, y.UseCount = "tpl_a", 5

	for run := 0; run < 5; run++ {
		got := SortByUseCountDesc([]*Template{x, y})
		if got[0].ID != "tpl_a" {
			t.Fatalf("第 %d 次运行排序不稳定：期望 tpl_a 在前，得到 %s", run, got[0].ID)
		}
	}
}

func TestSortByUseCountDesc_DoesNotMutateInput(t *testing.T) {
	a := mkTemplate(ScopePersonal, "u.a", "/report")
	a.ID, a.UseCount = "tpl_a", 1
	b := mkTemplate(ScopePersonal, "u.b", "/report")
	b.ID, b.UseCount = "tpl_b", 9

	in := []*Template{a, b}
	_ = SortByUseCountDesc(in)
	if in[0].ID != "tpl_a" {
		t.Fatal("★ 排序不得就地修改入参（否则调用方拿到被重排的切片）")
	}
}

// ───────────────────────────── DefaultsForPage ─────────────────────────────

func TestDefaultsForPage_Priority(t *testing.T) {
	// 个人默认 > 团队默认 > 系统默认
	sys := mkTemplate(ScopeSystem, "u.it", "/report")
	sys.ID, sys.IsDefault = "tpl_sys", true
	team := mkTemplate(ScopeTeam, "u.mgr", "/report")
	team.ID, team.IsDefault = "tpl_team", true
	team.Shares = []Share{{SubjectKind: "dept", SubjectID: "D_SALES"}}
	per := mkTemplate(ScopePersonal, "u.alice", "/report")
	per.ID, per.IsDefault = "tpl_per", true

	v := viewer("u.alice", inDept("D_SALES"))

	if got := DefaultsForPage([]*Template{per, team, sys}, v, "/report"); got == nil || got.ID != "tpl_per" {
		t.Fatalf("应优先个人默认，得到 %v", got)
	}
	// 去掉个人默认 ⇒ 团队默认
	if got := DefaultsForPage([]*Template{team, sys}, v, "/report"); got == nil || got.ID != "tpl_team" {
		t.Fatalf("应取团队默认，得到 %v", got)
	}
	// 只剩系统默认
	if got := DefaultsForPage([]*Template{sys}, v, "/report"); got == nil || got.ID != "tpl_sys" {
		t.Fatalf("应取系统默认，得到 %v", got)
	}
}

func TestDefaultsForPage_NoDefaultReturnsNil(t *testing.T) {
	// ★ 没有默认就返回 nil，绝不「随便挑一个」——
	//   否则用户会莫名被套上一个不属于自己口径的视图。
	tpl := mkTemplate(ScopeSystem, "u.it", "/report") // is_default=false
	if got := DefaultsForPage([]*Template{tpl}, viewer("u.alice"), "/report"); got != nil {
		t.Fatalf("★ 无显式默认时应返回 nil，得到 %v", got.ID)
	}
}

func TestDefaultsForPage_RespectsPageIsolation(t *testing.T) {
	// 报表页的默认不得被 P&L 页请求取走
	tpl := mkTemplate(ScopeSystem, "u.it", "/report")
	tpl.ID, tpl.IsDefault = "tpl_report", true
	if got := DefaultsForPage([]*Template{tpl}, viewer("u.alice"), "/pnl"); got != nil {
		t.Fatal("★ 模板按页隔离：/pnl 不应取到 /report 的默认")
	}
}

func TestDefaultsForPage_SkipsUnreadable(t *testing.T) {
	// 别人的个人默认不可见 ⇒ 不能被选为默认
	other := mkTemplate(ScopePersonal, "u.bob", "/report")
	other.IsDefault = true
	if got := DefaultsForPage([]*Template{other}, viewer("u.alice"), "/report"); got != nil {
		t.Fatal("★ 不得套用不可见的默认模板")
	}
}

// ───────────────────────────── JSON 往返 ─────────────────────────────

func TestTemplate_JSONRoundTrip(t *testing.T) {
	tpl := mkTemplate(ScopeTeam, "u.mgr", "/report")
	tpl.Shares = []Share{{SubjectKind: "dept", SubjectID: "D_SALES"}}
	tpl.IsDefault = true
	tpl.UseCount = 7

	raw, err := json.Marshal(tpl)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var back Template
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if back.ID != tpl.ID || back.Scope != tpl.Scope || !back.IsDefault || back.UseCount != 7 {
		t.Fatalf("往返不一致：%+v", back)
	}
	// QueryState 原样往返（本层不解释其结构）
	if string(back.QueryState) != string(tpl.QueryState) {
		t.Fatalf("queryState 应原样往返，得到 %s", back.QueryState)
	}
	if len(back.Shares) != 1 || back.Shares[0].SubjectID != "D_SALES" {
		t.Fatalf("shares 往返丢失：%+v", back.Shares)
	}
}

// ★ 契约对齐：字段名必须与 contracts/view-template.ts 一致，
//   否则前端拿到的是另一套命名（历史上已在 req/chain 上踩过一次）。
func TestTemplate_JSONFieldNamesMatchContract(t *testing.T) {
	tpl := mkTemplate(ScopeSystem, "u.it", "/report")
	tpl.Shares = []Share{{SubjectKind: "group", SubjectID: "grp_x"}}
	raw, _ := json.Marshal(tpl)

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("非法 JSON：%v", err)
	}
	required := []string{
		"id", "name", "scope", "owner", "page", "queryState",
		"columns", "layout", "useCount", "createdAt", "updatedAt",
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			t.Fatalf("★ 契约字段缺失：%s（须与 contracts/view-template.ts 一致）", k)
		}
	}
	// ColumnPref 的字段名也要对齐
	cols, _ := m["columns"].([]any)
	if len(cols) != 1 {
		t.Fatalf("columns 序列化异常：%v", m["columns"])
	}
	col0 := cols[0].(map[string]any)
	for _, k := range []string{"key", "visible", "order"} {
		if _, ok := col0[k]; !ok {
			t.Fatalf("★ ColumnPref 字段缺失：%s", k)
		}
	}
}
