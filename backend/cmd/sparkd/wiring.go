// 装配辅助 —— 把内置的组织/模板/授权数据接到服务上。
//
// 生产环境这些数据来自 DB（dim_org / entitlement）；此处提供**最小可运行**夹具，
// 让 sparkd 能端到端启动并被 /api/me、/api/query 探活。
package main

import (
	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
)

// buildOrgDirectory 构造演示组织架构（F9=A 单主属）。
func buildOrgDirectory() *chain.OrgDirectory {
	d := chain.NewOrgDirectory()
	put := func(n *chain.OrgNode) { d.Put(n) }

	put(&chain.OrgNode{Account: "ceo", Tier: "T1", PrimaryDept: "HQ", CanApprove: true, Active: true})
	put(&chain.OrgNode{Account: "vp.sea", Supervisor: "ceo", Tier: "T2", PrimaryDept: "SEA",
		CanApprove: true, Active: true})
	put(&chain.OrgNode{Account: "vp.us", Supervisor: "ceo", Tier: "T2", PrimaryDept: "US",
		CanApprove: true, Active: true})
	put(&chain.OrgNode{Account: "lead.sea", Supervisor: "vp.sea", Tier: "T3", PrimaryDept: "SEA",
		CanApprove: true, Active: true})
	put(&chain.OrgNode{Account: "ops.sea", Supervisor: "lead.sea", Tier: "T4", PrimaryDept: "SEA",
		CanApprove: true, Active: true})
	// IT 账号：虚线汇报到 ceo（仅抄送，不入审批）
	put(&chain.OrgNode{Account: "it.ops", Supervisor: "lead.sea", Tier: "T4", PrimaryDept: "SEA",
		DottedLineSupervisors: []string{"ceo"}, CanApprove: false, Active: true})
	return d
}

// builtinTemplates 内置账号模板。
//
// 注意：模板**不含** canViewBusinessValues 的置真逻辑 —— D7 由 Resolver 兜底。
func builtinTemplates() map[string]*authz.Entitlement {
	allBusinessGroups := []authz.GroupScopeGrant{
		{Group: string(authz.GrpOps), MaxLevel: authz.L4},
		{Group: string(authz.GrpCostProfit), MaxLevel: authz.L4},
		{Group: string(authz.GrpInventory), MaxLevel: authz.L4},
		{Group: string(authz.GrpROI), MaxLevel: authz.L4},
	}
	return map[string]*authz.Entitlement{
		// 业务负责人：可见业务数值
		"tpl.lead": {
			Account:               "tpl.lead",
			Modules:               authz.ModuleGrant{Enabled: []string{"m.ops", "m.cost", "m.inventory", "m.roi"}},
			MaxLevel:              authz.L4,
			Dimensions:            []authz.DimensionGrant{{Dim: "brand"}},
			DataUseGroups:         allBusinessGroups,
			CanViewBusinessValues: true,
		},
		// IT：只给运维模块，**不给**业务数值（D7）
		"tpl.it": {
			Account:  "tpl.it",
			Modules:  authz.ModuleGrant{Enabled: []string{"m.ops"}},
			MaxLevel: authz.L2,
			Dimensions: []authz.DimensionGrant{{Dim: "brand"}},
			// 不设 CanViewBusinessValues
		},
	}
}

// buildEntitlementSource 返回账号 → 授权 的查询函数。
func buildEntitlementSource() func(string) (*authz.Entitlement, []*authz.Entitlement) {
	accounts := map[string]string{
		"ceo":      "tpl.lead",
		"vp.sea":   "tpl.lead",
		"vp.us":    "tpl.lead",
		"lead.sea": "tpl.lead",
		"ops.sea":  "tpl.lead",
		"it.ops":   "tpl.it",
	}
	return func(account string) (*authz.Entitlement, []*authz.Entitlement) {
		tpl, ok := accounts[account]
		if !ok {
			return nil, nil
		}
		return &authz.Entitlement{
			Account:      account,
			BaseTemplate: tpl,
		}, nil // 分组授权（groupGrants）由 DB 提供；此处为空
	}
}
