package authz_test

import (
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
)

// ─────────────────────────────────────────────────────────────────────────────
// 闸门：勾选组（DataUseGroup）的**组级限定不得被静默丢弃**
//
// 病历：Resolve 的 absorb 收集器此前只做一件事 ——
//
//	for _, g := range src.DataUseGroups { groupSet[DataUseGroup(g.Group)] = true }
//
// 组名收下了，但组的 **MaxLevel / Fields / ScopedModules / Denied** 全部丢弃。
// 于是 D11 设计的「四组各自独立密级」（运营数据 L2、投资回报 L1）在服务端
// **写了不生效** —— 与 activation.go 开头记的是同一个病：
// 字段存在、接口不报错、求值结果里永远看不到它 ⇒ 断言恒真。
//
// ★ 注意：Denied（组内显式禁用）此前**恰好**被单独照顾过（groupFieldDeny），
//   所以只测 Denied 是测不出这个 bug 的。本闸门专测「限定要出现在求值结果里」。
// ─────────────────────────────────────────────────────────────────────────────

func grpEnt() *authz.Entitlement {
	return &authz.Entitlement{
		Account:  "acc.ops1",
		MaxLevel: authz.L3,
		Modules:  authz.ModuleGrant{Enabled: []string{"module.report"}},
		Dimensions: []authz.DimensionGrant{},
		DataUseGroups: []authz.GroupScopeGrant{
			{
				Group:         string(authz.GrpOps),
				MaxLevel:      authz.L2,
				Fields:        []string{"gmv", "orders"},
				ScopedModules: []string{"module.report"},
			},
			{
				// ★ grp.roi 依赖 cost_profit + inventory（GroupDeps），
				//   二者未勾选时本组会被依赖闭合剔除 —— 这正是本闸门
				//   「先自证依赖已满足」的意义：夹具写错会让断言看着像实现错。
				Group:    string(authz.GrpROI),
				MaxLevel: authz.L1,
			},
			{Group: string(authz.GrpCostProfit), MaxLevel: authz.L2},
			{Group: string(authz.GrpInventory), MaxLevel: authz.L2},
		},
	}
}

func TestResolve_GroupScopes_NotDropped(t *testing.T) {
	view := authz.NewResolver(nil).Resolve(grpEnt(), nil)

	if len(view.GroupScopes) != 4 {
		t.Fatalf("组级限定被丢弃：应输出 4 组限定，实得 %d（%+v）",
			len(view.GroupScopes), view.GroupScopes)
	}
	byGroup := map[string]authz.GroupScopeGrant{}
	for _, s := range view.GroupScopes {
		byGroup[s.Group] = s
	}
	ops, ok := byGroup[string(authz.GrpOps)]
	if !ok {
		t.Fatal("grp.ops 的组级限定缺失")
	}
	if ops.MaxLevel != authz.L2 {
		t.Errorf("组级密级丢失: got %q want L2", ops.MaxLevel)
	}
	if len(ops.Fields) != 2 {
		t.Errorf("组内字段子集丢失: %v", ops.Fields)
	}
	if len(ops.ScopedModules) != 1 || ops.ScopedModules[0] != "module.report" {
		t.Errorf("组内模块子集丢失: %v", ops.ScopedModules)
	}
	// 四组各自独立密级 —— 这是 D11 的核心，不能因为账号 MaxLevel=L3 就把组拍平
	if byGroup[string(authz.GrpROI)].MaxLevel != authz.L1 {
		t.Errorf("grp.roi 的组级密级丢失: %q",
			byGroup[string(authz.GrpROI)].MaxLevel)
	}
	// 账号整体密级不受组级限定影响（组级是「组内上限」，不是账号上限）
	if view.MaxLevel != authz.L3 {
		t.Errorf("组级限定错误地改写了账号密级: %q", view.MaxLevel)
	}
}

// 负向自测：同组两处声明时，密级取**更宽松**者、字段取并集；
// 若实现写成「后写覆盖前写」或「只留第一条」，本测试会响。
func TestResolve_GroupScopes_MergedOnUnion_Negative(t *testing.T) {
	e := grpEnt()
	// 分组授权再给同一个组一份更宽松的限定
	grants := []*authz.Entitlement{{
		Account: "grp.directors",
		DataUseGroups: []authz.GroupScopeGrant{{
			Group:    string(authz.GrpOps),
			MaxLevel: authz.L4, // 更宽松
			Fields:   []string{"profit"}, // 并集 ⇒ {gmv,orders,profit}
		}},
	}}

	view := authz.NewResolver(nil).Resolve(e, grants)
	for _, s := range view.GroupScopes {
		if s.Group != string(authz.GrpOps) {
			continue
		}
		if s.MaxLevel != authz.L4 {
			t.Errorf("组级密级未取更宽松者: got %q want L4", s.MaxLevel)
		}
		if len(s.Fields) != 3 {
			t.Errorf("组内字段未取并集: %v", s.Fields)
		}
		return
	}
	t.Fatal("grp.ops 的组级限定缺失")
}

// 负向自测：组内**空字段集 = 组内全部**，空吞并任何集合仍为「全部」。
// 若实现把空集当作「什么都不要」，一片空 Fields 会静默变成零权限。
func TestResolve_GroupScopes_EmptyFieldsMeansAll_Negative(t *testing.T) {
	e := &authz.Entitlement{
		Account:  "acc.ops1",
		MaxLevel: authz.L1,
		Modules:  authz.ModuleGrant{},
		DataUseGroups: []authz.GroupScopeGrant{
			{Group: string(authz.GrpOps), MaxLevel: authz.L2}, // Fields 空 = 全部
		},
	}
	grants := []*authz.Entitlement{{
		Account: "grp.x",
		DataUseGroups: []authz.GroupScopeGrant{{
			Group:  string(authz.GrpOps),
			Fields: []string{"gmv"},
		}},
	}}
	view := authz.NewResolver(nil).Resolve(e, grants)
	for _, s := range view.GroupScopes {
		if s.Group == string(authz.GrpOps) {
			if len(s.Fields) != 0 {
				t.Errorf("空字段集被当作子集处理（应为「组内全部」）: %v", s.Fields)
			}
			return
		}
	}
	t.Fatal("grp.ops 的组级限定缺失")
}

// ───────────── 时间盒与组限定共存：回收判定不得受组限定影响 ─────────────

func TestResolve_GroupScopesWithExpiredGrant(t *testing.T) {
	e := grpEnt()
	e.Grants = []authz.GrantRecord{{
		Origin:    authz.OriginRequest,
		GrantedBy: "acc.mgr",
		ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		RequestID: "REQ-OLD",
	}}
	view := authz.NewResolver(nil).Resolve(e, nil)
	if len(view.Source.ExpiredGrants) == 0 {
		t.Error("过期来源在含组限定时未进入 ExpiredGrants")
	}
	if len(view.GroupScopes) != 4 {
		t.Errorf("组级限定缺失: %+v", view.GroupScopes)
	}
}

// ───────────── 依赖闭合与组限定必须同时正确 ─────────────
//
// 本闸门在开发中真的响过一次：夹具只勾了 grp.roi 而没勾它的两个依赖，
// 于是 grp.roi 被依赖闭合**正确**剔除，却看起来像「组限定被丢弃」。
// 记下来防止下次误判 —— 断言失败先自证夹具，再怀疑实现。
func TestResolve_GroupDeps_ClosureBeatsScopeReporting(t *testing.T) {
	e := &authz.Entitlement{
		Account:  "acc.ops1",
		MaxLevel: authz.L3,
		Modules:  authz.ModuleGrant{},
		DataUseGroups: []authz.GroupScopeGrant{
			// 只给 grp.roi，却漏掉它依赖的 cost_profit + inventory
			{Group: string(authz.GrpROI), MaxLevel: authz.L1},
		},
	}
	view := authz.NewResolver(nil).Resolve(e, nil)

	for _, g := range view.DataUseGroups {
		if string(g) == string(authz.GrpROI) {
			t.Error("依赖未满足的 grp.roi 仍出现在求值结果中")
		}
	}
	for _, s := range view.GroupScopes {
		if s.Group == string(authz.GrpROI) {
			t.Error("被依赖闭合剔除的组，其组级限定也不得输出")
		}
	}
	// 剔除原因必须可查（不静默消失）
	hit := false
	for _, d := range view.Source.DeniedBy {
		if len(d) > 10 && d[:10] == "groupDeps:" {
			hit = true
		}
	}
	if !hit {
		t.Errorf("依赖剔除未留痕: %v", view.Source.DeniedBy)
	}
}
