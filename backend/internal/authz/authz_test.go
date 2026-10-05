package authz_test

import (
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
)

func tplIT() *authz.Entitlement {
	return &authz.Entitlement{
		Account:               "tpl.it",
		Modules:               authz.ModuleGrant{Enabled: []string{"module.admin"}},
		MaxLevel:              authz.L1,
		CanViewBusinessValues: false, // D7：IT 恒不可见业务数值
	}
}

func tplManager() *authz.Entitlement {
	return &authz.Entitlement{
		Account:  "tpl.manager",
		Modules:  authz.ModuleGrant{Enabled: []string{"module.report", "module.pnl"}},
		MaxLevel: authz.L4,
		Dimensions: []authz.DimensionGrant{
			{Dim: "channel", Values: []string{"TK-TH"}},
		},
		DataUseGroups: []authz.GroupScopeGrant{
			{Group: "grp.ops", MaxLevel: authz.L4},
			{Group: "grp.cost_profit", MaxLevel: authz.L4},
			{Group: "grp.inventory", MaxLevel: authz.L4},
			{Group: "grp.roi", MaxLevel: authz.L4},
		},
		CanViewBusinessValues: true,
	}
}

func newResolver() *authz.Resolver {
	return authz.NewResolver(map[string]*authz.Entitlement{
		"tpl.it":      tplIT(),
		"tpl.manager": tplManager(),
	})
}

// D7：IT 即使被误配 canViewBusinessValues=true，求值后也必须为 false。
func TestD7_ITNeverSeesBusinessValues(t *testing.T) {
	r := newResolver()
	e := tplIT()
	e.CanViewBusinessValues = true // 恶意/误配
	view := r.Resolve(e, nil)
	if view.CanViewBusinessValues {
		t.Fatalf("D7 violated: IT must never view business values")
	}
}

// DENY 优先：模块在多处启用、一处禁用 ⇒ 最终禁用。
func TestDenyWins_Module(t *testing.T) {
	r := newResolver()
	e := &authz.Entitlement{
		Account: "u1",
		Modules: authz.ModuleGrant{
			Enabled:  []string{"module.pnl", "module.report"},
			Disabled: []string{"module.pnl"}, // 显式禁用
		},
		MaxLevel:              authz.L3,
		CanViewBusinessValues: true,
	}
	view := r.Resolve(e, nil)
	for _, m := range view.Modules {
		if m == "module.pnl" {
			t.Fatalf("DENY priority violated: module.pnl should be disabled")
		}
	}
	// 来源里应记录 deniedBy
	found := false
	for _, d := range view.Source.DeniedBy {
		if d == "module:module.pnl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deniedBy should record module:module.pnl, got %v", view.Source.DeniedBy)
	}
}

// 勾选组依赖：grp.roi 单独勾选（缺 cost_profit/inventory）应被剔除。
func TestGroupDependencyClosure(t *testing.T) {
	r := newResolver()
	e := &authz.Entitlement{
		Account: "u2",
		Modules: authz.ModuleGrant{Enabled: []string{}},
		DataUseGroups: []authz.GroupScopeGrant{
			{Group: "grp.roi", MaxLevel: authz.L4}, // 缺依赖
		},
		MaxLevel:              authz.L4,
		CanViewBusinessValues: true,
	}
	view := r.Resolve(e, nil)
	for _, g := range view.DataUseGroups {
		if g == "grp.roi" {
			t.Fatalf("grp.roi should be dropped when deps missing")
		}
	}
}

// 勾选组依赖满足：四组齐全 ⇒ grp.roi 保留。
func TestGroupDependencySatisfied(t *testing.T) {
	r := newResolver()
	e := &authz.Entitlement{
		Account: "u3",
		Modules: authz.ModuleGrant{Enabled: []string{}},
		DataUseGroups: []authz.GroupScopeGrant{
			{Group: "grp.ops", MaxLevel: authz.L4},
			{Group: "grp.cost_profit", MaxLevel: authz.L4},
			{Group: "grp.inventory", MaxLevel: authz.L4},
			{Group: "grp.roi", MaxLevel: authz.L4},
		},
		MaxLevel:              authz.L4,
		CanViewBusinessValues: true,
	}
	view := r.Resolve(e, nil)
	has := map[authz.DataUseGroup]bool{}
	for _, g := range view.DataUseGroups {
		has[g] = true
	}
	if !has["grp.roi"] {
		t.Fatalf("grp.roi should be present when deps satisfied, got %v", view.DataUseGroups)
	}
}

// 临时授权时间盒：过期后不再计入来源。
func TestTempGrantExpiry(t *testing.T) {
	r := authz.NewResolver(nil)
	r.Now = func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }
	e := &authz.Entitlement{
		Account: "u4",
		Modules: authz.ModuleGrant{Enabled: []string{}},
		MaxLevel:              authz.L2,
		CanViewBusinessValues: true,
		TempGrants: []authz.TempGrant{
			{Scope: "L4:cogs", ExpiresAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), GrantedBy: "T1"},
		},
	}
	view := r.Resolve(e, nil)
	if len(view.Source.FromTemp) != 0 {
		t.Fatalf("expired temp grant should not appear, got %v", view.Source.FromTemp)
	}
}

// D13：可授出 ⊆ 自身权限 —— 模块溢出必须被拒。
func TestDelegationBound_ModuleOverflow(t *testing.T) {
	granter := &authz.EntitlementView{
		Modules:  []string{"module.report"},
		MaxLevel: authz.L3,
		Dimensions: map[string][]string{"channel": {"TK-TH"}},
	}
	grantee := &authz.EntitlementView{
		Modules:  []string{"module.report", "module.pnl"}, // pnl 超出
		MaxLevel: authz.L3,
		Dimensions: map[string][]string{"channel": {"TK-TH"}},
	}
	if authz.DelegationAllowed(granter, grantee) {
		t.Fatalf("D13 violated: module overflow must be rejected")
	}
}

// D13：密级溢出必须被拒。
func TestDelegationBound_LevelOverflow(t *testing.T) {
	granter := &authz.EntitlementView{MaxLevel: authz.L2, Dimensions: map[string][]string{}}
	grantee := &authz.EntitlementView{MaxLevel: authz.L4, Dimensions: map[string][]string{}}
	if authz.DelegationAllowed(granter, grantee) {
		t.Fatalf("D13 violated: level overflow must be rejected")
	}
}

// D13：业务数值不可越授。
func TestDelegationBound_BusinessValues(t *testing.T) {
	granter := &authz.EntitlementView{
		MaxLevel: authz.L4, CanViewBusinessValues: false,
		Dimensions: map[string][]string{},
	}
	grantee := &authz.EntitlementView{
		MaxLevel: authz.L4, CanViewBusinessValues: true,
		Dimensions: map[string][]string{},
	}
	if authz.DelegationAllowed(granter, grantee) {
		t.Fatalf("D13 violated: cannot delegate business-value visibility you lack")
	}
}

// D13：完全子集应允许。
func TestDelegationBound_SubsetAllowed(t *testing.T) {
	granter := &authz.EntitlementView{
		Modules: []string{"module.report", "module.pnl"},
		MaxLevel: authz.L4, CanViewBusinessValues: true,
		Dimensions: map[string][]string{"channel": {"*"}},
	}
	grantee := &authz.EntitlementView{
		Modules: []string{"module.report"},
		MaxLevel: authz.L3, CanViewBusinessValues: true,
		Dimensions: map[string][]string{"channel": {"TK-TH"}},
	}
	if !authz.DelegationAllowed(granter, grantee) {
		t.Fatalf("subset delegation should be allowed")
	}
}

// 分组授权并集：账号 + 分组的模块应合并。
func TestGroupUnion(t *testing.T) {
	r := newResolver()
	e := &authz.Entitlement{
		Account: "u5",
		Modules: authz.ModuleGrant{Enabled: []string{"module.filter"}},
		MaxLevel: authz.L2,
		CanViewBusinessValues: true,
	}
	grp := &authz.Entitlement{
		Account: "group:东区",
		Modules: authz.ModuleGrant{Enabled: []string{"module.report"}},
		MaxLevel: authz.L3,
		CanViewBusinessValues: true,
	}
	view := r.Resolve(e, []*authz.Entitlement{grp})
	has := map[string]bool{}
	for _, m := range view.Modules {
		has[m] = true
	}
	if !has["module.filter"] || !has["module.report"] {
		t.Fatalf("group union failed: got %v", view.Modules)
	}
	if view.MaxLevel != authz.L3 {
		t.Fatalf("max level should be L3 (wider), got %s", view.MaxLevel)
	}
}
