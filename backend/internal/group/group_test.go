package group

import (
	"reflect"
	"sort"
	"testing"
)

// ───────────────────────────── Evaluate 基本语义 ─────────────────────────────

func TestEvaluate_PersonalOnly(t *testing.T) {
	p := &Personal{
		Account:      "u.a",
		BaseTemplate: "tpl.brand_mgr",
		Modules:      []string{"module.report"},
		Dimensions:   map[string][]string{"channel": {"TK-TH"}},
		MaxLevel:     L3,
	}
	e := Evaluate(p, nil, nil)
	if !reflect.DeepEqual(e.Modules, []string{"module.report"}) {
		t.Fatalf("个人模块应为 [module.report]，实际 %v", e.Modules)
	}
	if e.MaxLevel != L3 {
		t.Fatalf("密级应为 L3，实际 %s", e.MaxLevel)
	}
	if got := e.Dimensions["channel"]; !reflect.DeepEqual(got, []string{"TK-TH"}) {
		t.Fatalf("渠道维度应为 [TK-TH]，实际 %v", got)
	}
}

func TestEvaluate_UnionsGroupGrants(t *testing.T) {
	p := &Personal{Account: "u.a", Modules: []string{"module.report"}, MaxLevel: L1}
	g := &Group{
		ID:   "grp.hz",
		Name: "华东渠道组",
		Grants: []Grant{
			{Kind: KindModule, Key: "module.pnl"},
			{Kind: KindLevel, Key: "level.L3"},
			{Kind: KindDimension, Key: "dimension.channel", Values: []string{"TK-TH", "SP-TH"}},
			{Kind: KindDataUseGroup, Key: "data_use_group.grp.ops"},
		},
	}
	ms := []*Membership{{Account: "u.a", GroupID: "grp.hz", InheritsGrants: true}}

	e := Evaluate(p, []*Group{g}, ms)

	wantMods := []string{"module.pnl", "module.report"}
	if !reflect.DeepEqual(e.Modules, wantMods) {
		t.Fatalf("模块应取并集 %v，实际 %v", wantMods, e.Modules)
	}
	if e.MaxLevel != L3 {
		t.Fatalf("密级应升到 L3（组授权），实际 %s", e.MaxLevel)
	}
	if got := e.Dimensions["channel"]; !reflect.DeepEqual(got, []string{"SP-TH", "TK-TH"}) {
		t.Fatalf("渠道维度应合并并排序，实际 %v", got)
	}
	if !reflect.DeepEqual(e.DataUseGroups, []string{"grp.ops"}) {
		t.Fatalf("勾选组应为 [grp.ops]，实际 %v", e.DataUseGroups)
	}
	// 来源可追溯：pnl 来自 grp.hz
	if got := e.Sources["module.pnl"]; !reflect.DeepEqual(got, []string{"grp.hz"}) {
		t.Fatalf("module.pnl 来源应为 [grp.hz]，实际 %v", got)
	}
}

// ★★ 核心不变量：退出继承的成员**不**获得组授权（但在组内）。
func TestEvaluate_NonInheritingMemberGetsNothing(t *testing.T) {
	p := &Personal{Account: "u.a", MaxLevel: L1}
	g := &Group{ID: "grp.hz", Name: "华东", Grants: []Grant{{Kind: KindModule, Key: "module.pnl"}}}
	ms := []*Membership{{Account: "u.a", GroupID: "grp.hz", InheritsGrants: false}}

	e := Evaluate(p, []*Group{g}, ms)
	if len(e.Modules) != 0 {
		t.Fatalf("退出继承者不应获得组模块，实际 %v", e.Modules)
	}
	if len(e.GroupIDs) != 0 {
		t.Fatalf("退出继承者不应计入参与求值的分组，实际 %v", e.GroupIDs)
	}
}

// ★★ DENY 优先：组内允许不得覆盖个人 DENY。
func TestEvaluate_PersonalDenyBeatsGroupAllow(t *testing.T) {
	p := &Personal{
		Account:  "u.a",
		Modules:  []string{"module.report"},
		MaxLevel: L1,
		Denies:   []string{"module:module.pnl"},
	}
	g := &Group{ID: "grp.hz", Name: "华东", Grants: []Grant{{Kind: KindModule, Key: "module.pnl"}}}
	ms := []*Membership{{Account: "u.a", GroupID: "grp.hz", InheritsGrants: true}}

	e := Evaluate(p, []*Group{g}, ms)
	for _, m := range e.Modules {
		if m == "module.pnl" {
			t.Fatalf("个人 DENY 必须优先于组允许，但 module.pnl 仍在有效模块中：%v", e.Modules)
		}
	}
	if !reflect.DeepEqual(e.Modules, []string{"module.report"}) {
		t.Fatalf("有效模块应只剩个人模块，实际 %v", e.Modules)
	}
}

// ★★ DENY 优先的另一方向：组 DENY 覆盖**个人**允许。
func TestEvaluate_GroupDenyBeatsPersonalAllow(t *testing.T) {
	p := &Personal{Account: "u.a", Modules: []string{"module.report", "module.pnl"}, MaxLevel: L2}
	g := &Group{ID: "grp.audit", Name: "审计组", Grants: []Grant{
		{Kind: KindModule, Key: "module.pnl", Deny: true},
	}}
	ms := []*Membership{{Account: "u.a", GroupID: "grp.audit", InheritsGrants: true}}

	e := Evaluate(p, []*Group{g}, ms)
	if !reflect.DeepEqual(e.Modules, []string{"module.report"}) {
		t.Fatalf("组 DENY 应剔除个人已允许的 module.pnl，实际 %v", e.Modules)
	}
	if len(e.Denies) != 1 || e.Denies[0] != "module:module.pnl" {
		t.Fatalf("生效拒绝项应可审计，实际 %v", e.Denies)
	}
}

func TestEvaluate_DimensionDenyRemovesWholeDim(t *testing.T) {
	p := &Personal{Account: "u.a", Dimensions: map[string][]string{"channel": {"TK-TH"}}, MaxLevel: L2}
	g := &Group{ID: "grp.x", Name: "x", Grants: []Grant{{Kind: KindDimension, Key: "dimension.channel", Deny: true}}}
	ms := []*Membership{{Account: "u.a", GroupID: "grp.x", InheritsGrants: true}}

	e := Evaluate(p, []*Group{g}, ms)
	if _, ok := e.Dimensions["channel"]; ok {
		t.Fatalf("维度 DENY 应整维剔除，实际仍存在：%v", e.Dimensions)
	}
}

func TestEvaluate_LevelDenyStarResetsToL1(t *testing.T) {
	p := &Personal{Account: "u.a", MaxLevel: L4}
	g := &Group{ID: "grp.x", Name: "x", Grants: []Grant{{Kind: KindLevel, Key: "level.*", Deny: true}}}
	ms := []*Membership{{Account: "u.a", GroupID: "grp.x", InheritsGrants: true}}

	e := Evaluate(p, []*Group{g}, ms)
	if e.MaxLevel != L1 {
		t.Fatalf("level:* DENY 应降回 L1，实际 %s", e.MaxLevel)
	}
}

// ★★ D7：IT 模板恒不可见业务数值 —— 即使组授权声称可见。
func TestEvaluate_ITNeverSeesBusinessValues(t *testing.T) {
	p := &Personal{
		Account:               "it.ops",
		BaseTemplate:          "tpl.it",
		MaxLevel:              L1,
		CanViewBusinessValues: true, // 即便被人为置 true
	}
	g := &Group{ID: "grp.x", Name: "x", Grants: []Grant{}}
	ms := []*Membership{{Account: "it.ops", GroupID: "grp.x", InheritsGrants: true}}

	e := Evaluate(p, []*Group{g}, ms)
	if e.CanViewBusinessValues {
		t.Fatal("★ D7 违反：IT 模板的 canViewBusinessValues 必须恒为 false")
	}
}

func TestEvaluate_RestrictedGroupBlocksBusinessValues(t *testing.T) {
	p := &Personal{Account: "u.a", MaxLevel: L3, CanViewBusinessValues: true}
	g := &Group{ID: "grp.ext", Name: "外部协作", Restricted: true}
	ms := []*Membership{{Account: "u.a", GroupID: "grp.ext", InheritsGrants: true}}

	e := Evaluate(p, []*Group{g}, ms)
	if e.CanViewBusinessValues {
		t.Fatal("受限分组应强制 canViewBusinessValues=false（D7 兜底）")
	}
}

func TestEvaluate_MissingGroupIsIgnoredNotFatal(t *testing.T) {
	p := &Personal{Account: "u.a", MaxLevel: L1}
	ms := []*Membership{{Account: "u.a", GroupID: "grp.ghost", InheritsGrants: true}}
	e := Evaluate(p, nil, ms)
	if len(e.Modules) != 0 {
		t.Fatalf("引用了不存在的分组应被忽略（不 panic、不误授），实际 %v", e.Modules)
	}
}

// ★ 并集必须**顺序确定**（否则前端预览会抖动）。
func TestEvaluate_OutputIsDeterministic(t *testing.T) {
	p := &Personal{Account: "u.a", Modules: []string{"module.z", "module.a"}, MaxLevel: L1}
	g := &Group{ID: "grp.x", Name: "x", Grants: []Grant{
		{Kind: KindModule, Key: "module.m"},
		{Kind: KindModule, Key: "module.b"},
	}}
	ms := []*Membership{{Account: "u.a", GroupID: "grp.x", InheritsGrants: true}}

	first := Evaluate(p, []*Group{g}, ms).Modules
	for i := 0; i < 20; i++ {
		got := Evaluate(p, []*Group{g}, ms).Modules
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("求值结果不确定（第 %d 次）：%v vs %v", i, got, first)
		}
	}
	want := []string{"module.a", "module.b", "module.m", "module.z"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("模块应排序输出 %v，实际 %v", want, first)
	}
}

func TestEvaluate_MultiGroupUnion(t *testing.T) {
	p := &Personal{Account: "u.a", MaxLevel: L1}
	g1 := &Group{ID: "grp.a", Name: "A", Grants: []Grant{{Kind: KindModule, Key: "module.pnl"}}}
	g2 := &Group{ID: "grp.b", Name: "B", Grants: []Grant{{Kind: KindLevel, Key: "level.L2"}}}
	ms := []*Membership{
		{Account: "u.a", GroupID: "grp.a", InheritsGrants: true},
		{Account: "u.a", GroupID: "grp.b", InheritsGrants: true},
	}
	e := Evaluate(p, []*Group{g1, g2}, ms)
	if !reflect.DeepEqual(e.Modules, []string{"module.pnl"}) {
		t.Fatalf("多组应取并集，实际 %v", e.Modules)
	}
	if e.MaxLevel != L2 {
		t.Fatalf("多组并集密级应为 L2，实际 %s", e.MaxLevel)
	}
	if !reflect.DeepEqual(e.GroupIDs, []string{"grp.a", "grp.b"}) {
		t.Fatalf("参与分组应确定排序，实际 %v", e.GroupIDs)
	}
}

// ───────────────────────────── 维度合并边界 ─────────────────────────────

// "*" 是全量标记：与具体值并存时必须坍缩为 ["*"]，否则下游会按具体值过滤而丢数据。
func TestMergeValues_StarCollapses(t *testing.T) {
	got := mergeValues([]string{"TK-TH"}, []string{"*"})
	if !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatalf("含 * 时应坍缩为 [*]，实际 %v", got)
	}
}

// ───────────────────────────── Preview ─────────────────────────────

func TestPreview_ShowsPostJoinPermissions(t *testing.T) {
	p := &Personal{Account: "u.b", Modules: []string{"module.report"}, MaxLevel: L1}
	g := &Group{ID: "grp.hz", Name: "华东", Grants: []Grant{
		{Kind: KindModule, Key: "module.pnl"},
		{Kind: KindLevel, Key: "level.L3"},
	}}

	// 该账号**尚未**加入任何组
	e := Preview(g, p, nil)
	mods := append([]string{}, e.Modules...)
	sort.Strings(mods)
	want := []string{"module.pnl", "module.report"}
	if !reflect.DeepEqual(mods, want) {
		t.Fatalf("预览应显示『加入后』的权限 %v，实际 %v", want, mods)
	}
	if e.MaxLevel != L3 {
		t.Fatalf("预览密级应为 L3，实际 %s", e.MaxLevel)
	}
}

func TestPreview_AlreadyMemberWhoOptedOut(t *testing.T) {
	p := &Personal{Account: "u.b", MaxLevel: L1}
	g := &Group{ID: "grp.hz", Name: "华东", Grants: []Grant{{Kind: KindModule, Key: "module.pnl"}}}
	// 当前：在组内但退出了继承
	ms := []*Membership{{Account: "u.b", GroupID: "grp.hz", InheritsGrants: false}}

	e := Preview(g, p, ms)
	if !reflect.DeepEqual(e.Modules, []string{"module.pnl"}) {
		t.Fatalf("预览应显示『恢复继承后』的权限，实际 %v", e.Modules)
	}
}

// ───────────────────────────── Validate ─────────────────────────────

func TestValidate_AcceptsWellFormedGroup(t *testing.T) {
	g := &Group{
		ID:   "grp.hz",
		Name: "华东渠道组",
		Grants: []Grant{
			{Kind: KindModule, Key: "module.pnl"},
			{Kind: KindDimension, Key: "dimension.channel", Values: []string{"TK-TH"}},
			{Kind: KindLevel, Key: "level.L3"},
		},
	}
	if p := Validate(g); len(p) != 0 {
		t.Fatalf("合法分组不应报问题，实际 %v", p)
	}
}

func TestValidate_RejectsKindKeyMismatch(t *testing.T) {
	g := &Group{ID: "grp.x", Name: "x", Grants: []Grant{{Kind: KindModule, Key: "level.L3"}}}
	p := Validate(g)
	if len(p) == 0 {
		t.Fatal("kind 与 key 前缀不符必须报错（否则求值静默匹配不上）")
	}
}

func TestValidate_RejectsDuplicateGrant(t *testing.T) {
	g := &Group{ID: "grp.x", Name: "x", Grants: []Grant{
		{Kind: KindModule, Key: "module.pnl"},
		{Kind: KindModule, Key: "module.pnl"},
	}}
	if p := Validate(g); len(p) == 0 {
		t.Fatal("重复授权必须报错")
	}
}

func TestValidate_RejectsBadLevel(t *testing.T) {
	g := &Group{ID: "grp.x", Name: "x", Grants: []Grant{{Kind: KindLevel, Key: "level.L9"}}}
	if p := Validate(g); len(p) == 0 {
		t.Fatal("非法密级必须报错")
	}
}

// ★ 受限分组不得授出 >L2（D7 兜底）。
func TestValidate_RestrictedGroupCannotGrantHighLevel(t *testing.T) {
	g := &Group{ID: "grp.ext", Name: "外部", Restricted: true, Grants: []Grant{{Kind: KindLevel, Key: "level.L4"}}}
	p := Validate(g)
	if len(p) == 0 {
		t.Fatal("受限分组授出 L4 必须被拒（D7 兜底）")
	}
}

func TestValidate_ReportsAllProblemsNotJustFirst(t *testing.T) {
	g := &Group{ID: "", Name: "", Grants: []Grant{
		{Kind: KindModule, Key: "module.a"},
		{Kind: KindLevel, Key: "level.L9"},
		{Kind: KindModule, Key: "module.a"},
	}}
	p := Validate(g)
	if len(p) < 4 {
		t.Fatalf("应一次性报出全部问题（ID/名称/密级/重复 ≥4），实际 %d 条：%v", len(p), p)
	}
}

func TestValidate_NilGroup(t *testing.T) {
	if p := Validate(nil); len(p) == 0 {
		t.Fatal("nil 分组必须报错而非 panic")
	}
}

// ───────────────────────────── Rank / MaxLevel / MinLevel ─────────────────────────────

func TestRank_Ordering(t *testing.T) {
	if !(Rank(L1) < Rank(L2) && Rank(L2) < Rank(L3) && Rank(L3) < Rank(L4)) {
		t.Fatal("密级序数必须严格递增")
	}
	if Rank(Level("bogus")) != 0 {
		t.Fatal("未知密级序数应为 0")
	}
}

func TestMaxMinLevel(t *testing.T) {
	if MaxLevel(L1, L4) != L4 || MaxLevel(L4, L1) != L4 {
		t.Fatal("MaxLevel 不对称")
	}
	if MinLevel(L1, L4) != L1 || MinLevel(L4, L1) != L1 {
		t.Fatal("MinLevel 不对称")
	}
	if MaxLevel(L3, L3) != L3 {
		t.Fatal("相同密级应返回自身")
	}
}

func TestIsIT(t *testing.T) {
	for _, s := range []string{"tpl.it", "tpl.it.v2", "tpl.it.x"} {
		if !IsIT(s) {
			t.Fatalf("%q 应判为 IT 模板", s)
		}
	}
	for _, s := range []string{"tpl.finance", "", "it", "tpl.item"} {
		if IsIT(s) {
			t.Fatalf("%q 不应判为 IT 模板（注意 tpl.item 是前缀陷阱）", s)
		}
	}
}

// ───────────────────────────── 拒绝键归一化（★ 曾静默失效）─────────────────────────────

// TestNormDeny_Canonical 自测守护住「DENY 键两侧写法必须一致」这条不变量。
//
// 这一条单测的价值在于：它**不是**在验证业务规则，而是在验证
// 「让业务规则得以生效的机械前提」。多态输入（带冒号/不带冒号/kind 含下划线）
// 是这类 bug 的高发区 —— 归一化写错时业务规则**不报错，只是不生效**。
func TestNormDeny_Canonical(t *testing.T) {
	cases := map[string]string{
		"module:module.pnl":              "module:module.pnl",
		"module.pnl":                     "module:module.pnl",
		"dimension.channel":              "dimension:dimension.channel",
		"data_use_group.grp.roi":         "data_use_group:data_use_group.grp.roi",
		"data_use_group:grp.roi":         "data_use_group:data_use_group.grp.roi",
		"level.*":                        "level:level.*",
		"  module:module.pnl  ":          "module:module.pnl",
	}
	for in, want := range cases {
		if got := normDeny(in); got != want {
			t.Errorf("normDeny(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestDenyKey_MatchesNormDeny 断言两侧产出**同构**，这是 DENY 生效的前提。
func TestDenyKey_MatchesNormDeny(t *testing.T) {
	for _, g := range []Grant{
		{Kind: KindModule, Key: "module.pnl"},
		{Kind: KindDimension, Key: "dimension.channel"},
		{Kind: KindDataUseGroup, Key: "data_use_group.grp.roi"},
		{Kind: KindLevel, Key: "level.*"},
	} {
		if got, want := denyKey(g.Kind, g.Key), normDeny(g.Key); got != want {
			t.Errorf("denyKey(%q,%q)=%q 与 normDeny(%q)=%q 不一致 —— DENY 会静默失效",
				g.Kind, g.Key, got, g.Key, want)
		}
	}
}

// ★ 回归：DENY 不生效是「不报错但不工作」类缺陷，必须逐个 kind 钉死。
func TestEvaluate_DenyWorksForEveryKind(t *testing.T) {
	cases := []struct {
		name  string
		p     *Personal
		grant Grant
		check func(*Evaluated) bool
	}{
		{
			name:  "module",
			p:     &Personal{Account: "u", Modules: []string{"module.pnl"}, MaxLevel: L1},
			grant: Grant{Kind: KindModule, Key: "module.pnl", Deny: true},
			check: func(e *Evaluated) bool { return len(e.Modules) == 0 },
		},
		{
			name:  "dimension",
			p:     &Personal{Account: "u", Dimensions: map[string][]string{"channel": {"TK-TH"}}, MaxLevel: L1},
			grant: Grant{Kind: KindDimension, Key: "dimension.channel", Deny: true},
			check: func(e *Evaluated) bool { _, ok := e.Dimensions["channel"]; return !ok },
		},
		{
			name:  "data_use_group",
			p:     &Personal{Account: "u", DataUseGroups: []string{"grp.roi"}, MaxLevel: L1},
			grant: Grant{Kind: KindDataUseGroup, Key: "data_use_group.grp.roi", Deny: true},
			check: func(e *Evaluated) bool { return len(e.DataUseGroups) == 0 },
		},
		{
			name:  "level",
			p:     &Personal{Account: "u", MaxLevel: L4},
			grant: Grant{Kind: KindLevel, Key: "level.*", Deny: true},
			check: func(e *Evaluated) bool { return e.MaxLevel == L1 },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &Group{ID: "grp.x", Name: "x", Grants: []Grant{c.grant}}
			ms := []*Membership{{Account: c.p.Account, GroupID: "grp.x", InheritsGrants: true}}
			e := Evaluate(c.p, []*Group{g}, ms)
			if !c.check(e) {
				t.Fatalf("kind=%s 的 DENY 未生效（这是静默失效类缺陷）", c.name)
			}
		})
	}
}
