// req_test.go —— M-REQ 权限申请流（D14）全生命周期测试。
//
// 覆盖：校验（D7 判重/跨部门）→ 路由（审批人须自身拥有被申请权限）→
//       审批 → 会签（否决/未决）→ 撤回 → 到期回收。
//
// 每条断言都对应 docs/02 M-REQ 的一条验收项。
package req

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
)

// ───────────────────────────── 测试夹具 ─────────────────────────────

// fixture 构造一个两级组织：u.lead(T1) → u.manager(T2) → u.staff(T3)。
//
// 权限设计（关键，决定了哪条路由断言成立）：
//   - u.lead    ：L4 + 全部模块（可批准任何申请）
//   - u.manager ：L3 + module.report + module.pnl
//   - u.staff   ：L1 + module.report
type fixture struct {
	svc  *Service
	ents map[string]*authz.Entitlement
	grps map[string][]*authz.Entitlement
}

func newFixture() *fixture {
	org := chain.NewOrgDirectory()
	org.Put(&chain.OrgNode{Account: "u.lead", Tier: "T1", PrimaryDept: "d.exec", CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "u.manager", Supervisor: "u.lead", Tier: "T2", PrimaryDept: "d.ops", CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "u.staff", Supervisor: "u.manager", Tier: "T3", PrimaryDept: "d.ops", Active: true})
	org.Put(&chain.OrgNode{Account: "u.it", Supervisor: "u.lead", Tier: "T2", PrimaryDept: "d.it", Active: true})
	org.Put(&chain.OrgNode{Account: "u.other", Supervisor: "u.lead", Tier: "T2", PrimaryDept: "d.fin", Active: true})

	ents := map[string]*authz.Entitlement{
		"u.lead": {
			Account: "u.lead", BaseTemplate: "tpl.admin",
			Modules:      authz.ModuleGrant{Enabled: []string{"module.report", "module.pnl", "module.admin"}},
			Dimensions:   []authz.DimensionGrant{{Dim: "channel", Values: []string{"*"}}},
			MaxLevel:     authz.L4,
		},
		"u.manager": {
			Account: "u.manager", BaseTemplate: "tpl.brand_mgr",
			Modules:    authz.ModuleGrant{Enabled: []string{"module.report", "module.pnl"}},
			Dimensions: []authz.DimensionGrant{{Dim: "channel", Values: []string{"TK-TH"}}},
			MaxLevel:   authz.L3,
		},
		"u.staff": {
			Account: "u.staff", BaseTemplate: "tpl.base",
			Modules:    authz.ModuleGrant{Enabled: []string{"module.report"}},
			Dimensions: []authz.DimensionGrant{{Dim: "channel", Values: []string{"TK-TH"}}},
			MaxLevel:   authz.L1,
		},
		"u.it": {
			Account: "u.it", BaseTemplate: "tpl.it",
			Modules:  authz.ModuleGrant{Enabled: []string{"module.admin"}},
			MaxLevel: authz.L1,
		},
		"u.other": {
			Account: "u.other", BaseTemplate: "tpl.finance",
			Modules:    authz.ModuleGrant{Enabled: []string{"module.report"}},
			Dimensions: []authz.DimensionGrant{{Dim: "channel", Values: []string{"SP-TH"}}},
			MaxLevel:   authz.L2,
		},
	}

	res := authz.NewResolver(nil)
	svc := NewService(org, res, func(a string) (*authz.Entitlement, []*authz.Entitlement) {
		e, ok := ents[a]
		if !ok {
			// 未知账号：返回空授权（而非 nil）—— 求值器不应收到 nil
			return &authz.Entitlement{Account: a, MaxLevel: authz.L1}, nil
		}
		return e, nil
	})
	fixed := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return fixed }

	return &fixture{svc: svc, ents: ents, grps: map[string][]*authz.Entitlement{}}
}

// ───────────────────────────── 校验：D7 ─────────────────────────────

// ★ IT 不可申请业务数值（D7）。
func TestValidate_ITCannotRequestBusinessData(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.1", Applicant: "u.it",
		Draft: Draft{
			Modules:       []string{"module.report"},
			DataUseGroups: []authz.GroupScopeGrant{{Group: "grp.roi"}},
			MaxLevel:      authz.L2,
		},
		Purpose: "想看利润",
	}
	v, err := f.svc.Validate(r, nil)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	if v.OK {
		t.Fatal("★ D7 违反：IT 申请业务数据组/高密级必须被拒")
	}
	if len(v.OutOfScope) == 0 {
		t.Fatal("应给出超范围明细，便于 UI 提示")
	}
	if !strings.Contains(v.Message, "D7") {
		t.Fatalf("拒绝原因应显式提及 D7，实际 %q", v.Message)
	}
}

// IT 申请低密级、非业务模块仍应放行（避免误伤）。
func TestValidate_ITCanRequestNonBusinessModule(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.2", Applicant: "u.it",
		Draft:   Draft{Modules: []string{"module.report"}, MaxLevel: authz.L2},
		Purpose: "只读报表壳，无业务数值",
	}
	v, err := f.svc.Validate(r, nil)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	if !v.OK {
		t.Fatalf("IT 申请 L2 及以下、无业务数据组应放行，实际被拒：%s", v.Message)
	}
}

// ───────────────────────────── 校验：判重 ─────────────────────────────

func TestValidate_ReportsDuplicates(t *testing.T) {
	f := newFixture()
	// u.staff 已拥有 module.report
	r := &Request{
		ID: "req.3", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.report"}, MaxLevel: authz.L1},
		Purpose: "重复申请",
	}
	v, err := f.svc.Validate(r, nil)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	found := false
	for _, d := range v.Duplicates {
		if d == "module:module.report" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应把已拥有的 module.report 列为重复项，实际 %v", v.Duplicates)
	}
}

// ───────────────────────────── 路由：审批人须自身拥有 ─────────────────────────────

// ★ 核心约束（S ⊆ 权限(P)）：审批人权限不足时应**上溯**到权限超集者。
//
// 场景：u.staff 申请 module.pnl + L3。其 +1 是 u.manager（L3 + report + pnl）——
// 刚好覆盖 ⇒ 由 manager 批。若 manager 不覆盖，须上溯到 u.lead（L4 + 全模块）。
func TestValidate_RoutesToApproverWhoCovers(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.4", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L3},
		Purpose: "要看 P&L",
	}
	v, err := f.svc.Validate(r, nil)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	if !v.OK {
		t.Fatalf("应校验通过，实际 %s", v.Message)
	}
	if len(v.Route) != 1 {
		t.Fatalf("应有且仅有 1 个审批步骤，实际 %d", len(v.Route))
	}
	if v.Route[0].Approver != "u.manager" {
		t.Fatalf("应路由到 u.manager（+1 且权限覆盖），实际 %s", v.Route[0].Approver)
	}
}

// ★ 上溯：manager 不覆盖时，必须升级到 lead —— 而不是「找不到就放行」。
func TestValidate_EscalatesWhenSupervisorLacksPermission(t *testing.T) {
	f := newFixture()
	// 申请 module.admin —— manager 没有该模块
	r := &Request{
		ID: "req.5", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.admin"}, MaxLevel: authz.L3},
		Purpose: "需要管理台",
	}
	v, err := f.svc.Validate(r, nil)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	if !v.OK {
		t.Fatalf("lead 拥有 module.admin，应上溯成功，实际被拒：%s", v.Message)
	}
	if v.Route[0].Approver != "u.lead" {
		t.Fatalf("应上溯到 u.lead，实际 %s", v.Route[0].Approver)
	}
}

// ★ fail-closed：链上**无人**覆盖时必须报错，绝不静默放行。
func TestValidate_FailsClosedWhenNobodyCovers(t *testing.T) {
	f := newFixture()
	// 申请一个所有人都没有的模块
	r := &Request{
		ID: "req.6", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.nonexistent"}, MaxLevel: authz.L1},
		Purpose: "无人可批",
	}
	_, err := f.svc.Validate(r, nil)
	if err == nil {
		t.Fatal("★ fail-closed 违反：链上无人覆盖却未报错（会静默放行越权）")
	}
}

// ───────────────────────────── 跨部门 ─────────────────────────────

func TestValidate_CrossDeptIsBlockedFromSelfService(t *testing.T) {
	f := newFixture()
	dc := &chain.DataChain{
		Nodes: []chain.DataChainNode{
			{Scope: "channel:TK-TH", Owner: "u.manager", Dept: "d.ops"},
			{Scope: "channel:SP-TH", Owner: "u.other", Dept: "d.fin"},
		},
		Depts:     []string{"d.ops", "d.fin"},
		CrossDept: true,
	}
	r := &Request{
		ID: "req.7", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.report"}, MaxLevel: authz.L1},
		Purpose: "跨部门申请",
	}
	v, err := f.svc.Validate(r, dc)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	if v.OK {
		t.Fatal("★ 跨部门不可自助申请（A1），应被拒")
	}
	if !strings.Contains(v.Message, "BLOCKED_CROSS_DEPT") {
		t.Fatalf("拒绝原因应为 BLOCKED_CROSS_DEPT，实际 %q", v.Message)
	}
}

// ───────────────────────────── Submit 状态机 ─────────────────────────────

func TestSubmit_ApprovingWhenNoCosign(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.8", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L1},
		Purpose: "常规申请",
	}
	v, err := f.svc.Submit(r, nil)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	if !v.OK {
		t.Fatalf("应通过，实际 %s", v.Message)
	}
	if r.Status != StatusApproving {
		t.Fatalf("无会签时应为 APPROVING，实际 %s", r.Status)
	}
	if len(r.Approvals) != 1 {
		t.Fatalf("应写入 1 步审批，实际 %d", len(r.Approvals))
	}
}

// ★ L4 触发会签（F8）⇒ 状态必须是 COSIGN_PENDING，而非 APPROVING。
func TestSubmit_L4TriggersCosignPending(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.9", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L4},
		Purpose: "需要 L4",
	}
	v, err := f.svc.Submit(r, nil)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	if !v.OK {
		t.Fatalf("应通过（lead 可批），实际 %s", v.Message)
	}
	if r.Status != StatusCosignPending {
		t.Fatalf("L4 应触发会签 ⇒ COSIGN_PENDING，实际 %s", r.Status)
	}
	hasCosign := false
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign {
			hasCosign = true
		}
	}
	if !hasCosign {
		t.Fatalf("应收录至少一条 cosign 抄送，实际 %+v", r.CCs)
	}
}

func TestSubmit_CrossDeptSetsBlockedStatus(t *testing.T) {
	f := newFixture()
	dc := &chain.DataChain{Depts: []string{"d.ops", "d.fin"}, CrossDept: true}
	r := &Request{
		ID: "req.10", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.report"}, MaxLevel: authz.L1},
		Purpose: "跨部门",
	}
	_, err := f.svc.Submit(r, dc)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	if r.Status != StatusBlockedCrossDep {
		t.Fatalf("跨部门被拒后状态应为 BLOCKED_CROSS_DEPT，实际 %s", r.Status)
	}
}

// ───────────────────────────── 审批 / 会签 ─────────────────────────────

func TestApprove_HappyPath(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.11", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L1},
		Purpose: "常规",
	}
	if _, err := f.svc.Submit(r, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Approve(r, "u.manager", "同意"); err != nil {
		t.Fatalf("审批应成功：%v", err)
	}
	if r.Status != StatusApproved {
		t.Fatalf("状态应为 APPROVED，实际 %s", r.Status)
	}
	if r.ResolvedAt == nil {
		t.Fatal("APPROVED 必须记录 resolvedAt")
	}
	if r.Approvals[0].Action != "APPROVE" {
		t.Fatalf("审批步骤应记为 APPROVE，实际 %s", r.Approvals[0].Action)
	}
}

// ★★ 最易漏的一条：会签人**尚未表态**时不得通过。
//
// 曾经的实现只看 `Vetoed` —— 未表态时 Vetoed=false，于是一路放行，
// 会签完全形同虚设。此测试钉死这个语义。
func TestApprove_BlockedWhileCosignPending(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.12", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L4},
		Purpose: "L4 需会签",
	}
	if _, err := f.svc.Submit(r, nil); err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusCosignPending {
		t.Fatalf("前置条件：应为 COSIGN_PENDING，实际 %s", r.Status)
	}
	err := f.svc.Approve(r, "u.lead", "同意")
	if err == nil {
		t.Fatal("★ 会签未决时 Approve 必须失败 —— 否则会签形同虚设")
	}
	if r.Status != StatusCosignPending {
		t.Fatalf("失败后状态应保持 COSIGN_PENDING（不得被推进），实际 %s", r.Status)
	}
	if !strings.Contains(err.Error(), "cosign pending") {
		t.Fatalf("错误信息应说明会签未决，实际 %v", err)
	}
}

// ★ 会签人表态同意后 ⇒ 回到 APPROVING，可正常审批。
func TestCosign_ApproveUnblocksApproval(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.13", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L4},
		Purpose: "L4 需会签",
	}
	if _, err := f.svc.Submit(r, nil); err != nil {
		t.Fatal(err)
	}
	var cosigner string
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign {
			cosigner = cc.Cc
			break
		}
	}
	if cosigner == "" {
		t.Fatal("前置条件：应有 cosign 抄送人")
	}
	if err := f.svc.Cosign(r, cosigner, true, "无异议"); err != nil {
		t.Fatalf("会签同意应成功：%v", err)
	}
	if r.Status != StatusApproving {
		t.Fatalf("全部会签通过后应回到 APPROVING，实际 %s", r.Status)
	}
	if err := f.svc.Approve(r, "u.lead", "同意"); err != nil {
		t.Fatalf("会签完成后审批应成功：%v", err)
	}
	if r.Status != StatusApproved {
		t.Fatalf("应为 APPROVED，实际 %s", r.Status)
	}
}

// ★ 会签否决 ⇒ 直接驳回，且不可再审批通过。
func TestCosign_VetoRejectsAndBlocksApproval(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.14", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L4},
		Purpose: "L4",
	}
	if _, err := f.svc.Submit(r, nil); err != nil {
		t.Fatal(err)
	}
	var cosigner string
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign {
			cosigner = cc.Cc
			break
		}
	}
	if err := f.svc.Cosign(r, cosigner, false, "风险过高"); err != nil {
		t.Fatalf("会签否决应记录成功（否决本身不是错误）：%v", err)
	}
	if r.Status != StatusRejected {
		t.Fatalf("否决后应为 REJECTED，实际 %s", r.Status)
	}
	if err := f.svc.Approve(r, "u.lead", "仍想通过"); err == nil {
		t.Fatal("★ 已被会签否决的申请不得再被审批通过")
	}
}

// ★ 知会（notify）不阻断：u.staff 申请普通权限时，+2(u.lead) 是知会，
// 不触发会签 ⇒ 状态应为 APPROVING 而非 COSIGN_PENDING；
// 且必须**确实存在**一条 notify 抄送（否则本测试是空转，测不到东西）。
func TestSubmit_NotifyModeDoesNotBlock(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.15", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L2},
		Purpose: "中风险，仅知会",
	}
	v, err := f.svc.Submit(r, nil)
	if err != nil {
		t.Fatalf("不应返回错误：%v", err)
	}
	if !v.OK {
		t.Fatalf("应通过，实际 %s", v.Message)
	}
	if r.Status != StatusApproving {
		t.Fatalf("知会不应阻断 ⇒ APPROVING，实际 %s", r.Status)
	}
	if len(r.CCs) == 0 {
		t.Fatal("★ 本场景应产生 1 条 +2 知会抄送 —— 空集合会让本测试形同虚设")
	}
	notifySeen := false
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign {
			t.Fatalf("本场景不应触发会签，实际 %+v", r.CCs)
		}
		if cc.Mode == chain.CcNotify {
			notifySeen = true
		}
	}
	if !notifySeen {
		t.Fatalf("应存在 notify 抄送，实际 %+v", r.CCs)
	}
}

// ★ 知会人无决定权：对其执行 Cosign 必须报错（防止把「知会」当「会签」用）。
//
// 场景：u.staff(T3) 申请 —— +1 = u.manager(T2)，+2 = u.lead(T1) 为知会
// （非 L4 / 非跨部门 / 非投资组 / 非批量 ⇒ DefaultMode=notify）。
// ★ 注意：不能让 u.manager 当申请人 —— 其 +1 已是 T1(u.lead)，
// 那时 supOfApprover 为空 ⇒ CcRule=NONE ⇒ 根本没有抄送，测试前提就不成立。
func TestCosign_NotifyCcHasNoVetoPower(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.16", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L1},
		Purpose: "只有知会",
	}
	if _, err := f.svc.Submit(r, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.CCs) == 0 {
		t.Fatal("前置条件：应有知会抄送")
	}
	// 前提校验：本场景的抄送必须是 notify，否则后面断言的是别的东西
	if r.CCs[0].Mode != chain.CcNotify {
		t.Fatalf("前置条件：本场景应为 notify 模式，实际 %s", r.CCs[0].Mode)
	}
	err := f.svc.Cosign(r, r.CCs[0].Cc, false, "我想否决")
	if err == nil {
		t.Fatal("★ 知会人无决定权，Cosign 必须报错")
	}
	if !strings.Contains(err.Error(), "notify") {
		t.Fatalf("错误信息应点明是 notify 而非 cosign，实际 %v", err)
	}
}

func TestCosign_UnknownAccountRejected(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.17", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L4},
		Purpose: "L4",
	}
	if _, err := f.svc.Submit(r, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Cosign(r, "u.stranger", true, ""); err == nil {
		t.Fatal("非抄送人不得会签")
	}
}

// ───────────────────────────── Reject / Withdraw ─────────────────────────────

func TestReject_SetsRejectedAndRecords(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.18", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L1},
		Purpose: "x",
	}
	if _, err := f.svc.Submit(r, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Reject(r, "u.manager", "业务不必要"); err != nil {
		t.Fatalf("拒绝应成功：%v", err)
	}
	if r.Status != StatusRejected {
		t.Fatalf("应为 REJECTED，实际 %s", r.Status)
	}
	if r.Approvals[0].Action != "REJECT" {
		t.Fatalf("审批步骤应记为 REJECT，实际 %s", r.Approvals[0].Action)
	}
	if r.ResolvedAt == nil {
		t.Fatal("终结态必须记录 resolvedAt")
	}
}

func TestWithdraw_OnlyApplicantAndNotAfterTerminal(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.19", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L1},
		Purpose: "x",
	}
	if _, err := f.svc.Submit(r, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Withdraw(r, "u.manager"); err == nil {
		t.Fatal("非申请人不得撤回")
	}
	if err := f.svc.Withdraw(r, "u.staff"); err != nil {
		t.Fatalf("申请人应可撤回：%v", err)
	}
	if r.Status != StatusWithdrawn {
		t.Fatalf("应为 WITHDRAWN，实际 %s", r.Status)
	}
	// 已终结 ⇒ 再撤回应报错（幂等性：不让状态反复横跳）
	if err := f.svc.Withdraw(r, "u.staff"); err == nil {
		t.Fatal("已终结的申请不得再次撤回")
	}
}

func TestApprove_WrongStatusRejected(t *testing.T) {
	f := newFixture()
	r := &Request{ID: "req.20", Applicant: "u.staff", Status: StatusDraft}
	if err := f.svc.Approve(r, "u.manager", ""); err == nil {
		t.Fatal("DRAFT 状态不得审批")
	}
}

// ───────────────────────────── 到期回收 ─────────────────────────────

// ★ 时间盒：到期即回收；长期申请不回收。
func TestReclaim_ExpirySemantics(t *testing.T) {
	f := newFixture()
	base := f.svc.Now()

	// 未到期
	future := base.Add(24 * time.Hour)
	r1 := &Request{ID: "r1", Status: StatusApproved, RequestedExpiry: &future}
	if f.svc.Reclaim(r1) {
		t.Fatal("未到期不应回收")
	}

	// 已到期
	past := base.Add(-time.Second)
	r2 := &Request{ID: "r2", Status: StatusApproved, RequestedExpiry: &past}
	if !f.svc.Reclaim(r2) {
		t.Fatal("已到期必须回收")
	}

	// 到期时刻本身：视为到期（!Before 语义）
	exact := base
	r3 := &Request{ID: "r3", Status: StatusApproved, RequestedExpiry: &exact}
	if !f.svc.Reclaim(r3) {
		t.Fatal("恰好到期时刻应视为到期（边界）")
	}

	// 长期（无 expiry）
	r4 := &Request{ID: "r4", Status: StatusApproved}
	if f.svc.Reclaim(r4) {
		t.Fatal("长期申请不得被回收")
	}

	// 非 APPROVED 的申请不涉及回收
	r5 := &Request{ID: "r5", Status: StatusRejected, RequestedExpiry: &past}
	if f.svc.Reclaim(r5) {
		t.Fatal("非 APPROVED 状态不应触发回收")
	}

	// nil 安全
	if f.svc.Reclaim(nil) {
		t.Fatal("nil 申请应安全返回 false")
	}
}

// ───────────────────────────── 全链路闭环 ─────────────────────────────

// 端到端：提交 → 会签 → 审批 → 到期回收。
func TestLifecycle_EndToEnd(t *testing.T) {
	f := newFixture()
	expiry := f.svc.Now().Add(2 * time.Hour)
	r := &Request{
		ID: "req.e2e", Applicant: "u.staff",
		Draft:           Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L4},
		Purpose:         "端到端",
		RequestedExpiry: &expiry,
	}

	v, err := f.svc.Submit(r, nil)
	if err != nil || !v.OK {
		t.Fatalf("提交应通过：err=%v msg=%s", err, v.Message)
	}
	if r.Status != StatusCosignPending {
		t.Fatalf("① 应为 COSIGN_PENDING，实际 %s", r.Status)
	}

	var cosigner string
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign {
			cosigner = cc.Cc
		}
	}
	if err := f.svc.Cosign(r, cosigner, true, "ok"); err != nil {
		t.Fatalf("② 会签应成功：%v", err)
	}
	if r.Status != StatusApproving {
		t.Fatalf("② 应为 APPROVING，实际 %s", r.Status)
	}

	if err := f.svc.Approve(r, "u.lead", "同意"); err != nil {
		t.Fatalf("③ 审批应成功：%v", err)
	}
	if r.Status != StatusApproved || r.ResolvedAt == nil {
		t.Fatalf("③ 应为 APPROVED 且有 resolvedAt，实际 %s / %v", r.Status, r.ResolvedAt)
	}

	// ④ 未到期
	if f.svc.Reclaim(r) {
		t.Fatal("④ 未到期不应回收")
	}
	// ⑤ 把时钟推到到期之后
	f.svc.Now = func() time.Time { return expiry.Add(time.Second) }
	if !f.svc.Reclaim(r) {
		t.Fatal("⑤ 到期后必须回收")
	}
}

// ───────────────────────────── 辅助函数 ─────────────────────────────

func TestCovers_ModuleLevelDimension(t *testing.T) {
	res := authz.NewResolver(nil)
	view := res.Resolve(&authz.Entitlement{
		Account:    "a",
		Modules:    authz.ModuleGrant{Enabled: []string{"module.pnl"}},
		Dimensions: []authz.DimensionGrant{{Dim: "channel", Values: []string{"TK-TH"}}},
		MaxLevel:   authz.L3,
	}, nil)

	// 覆盖
	if !covers(view, Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L2}) {
		t.Fatal("子集应被判为覆盖")
	}
	// 模块不足
	if covers(view, Draft{Modules: []string{"module.admin"}, MaxLevel: authz.L1}) {
		t.Fatal("缺少模块不应被判为覆盖")
	}
	// 密级超限
	if covers(view, Draft{MaxLevel: authz.L4}) {
		t.Fatal("密级超限不应被判为覆盖")
	}
	// 维度超范围
	if covers(view, Draft{MaxLevel: authz.L1, Dimensions: []DimGrant{{Dim: "channel", Values: []string{"SP-TH"}}}}) {
		t.Fatal("维度取值超出范围不应被判为覆盖")
	}
	// nil 安全
	if covers(nil, Draft{}) {
		t.Fatal("nil view 不应被判为覆盖（fail-closed）")
	}
}

func TestLevelRank_Ordering(t *testing.T) {
	if !(levelRank(authz.L1) < levelRank(authz.L2) &&
		levelRank(authz.L2) < levelRank(authz.L3) &&
		levelRank(authz.L3) < levelRank(authz.L4)) {
		t.Fatal("密级序数必须严格递增")
	}
}

func TestIsIT(t *testing.T) {
	for _, s := range []string{"tpl.it", "tpl.it.v2"} {
		if !isIT(s) {
			t.Fatalf("%q 应判为 IT", s)
		}
	}
	for _, s := range []string{"tpl.base", "", "tpl.item"} {
		if isIT(s) {
			t.Fatalf("%q 不应判为 IT", s)
		}
	}
}

// ───────────────────────────── 负向：路由不可静默放行 ─────────────────────────────

// ★ 把「无覆盖者」改造成「静默放行」是很难被测出的 —— 这里同时断言
// **返回值为 nil** 与 **错误非空**，保证将来有人把
// `return nil, errors.New(...)` 改成 `return v, nil` 时立即红。
//
// 注意：仅断言 err != nil 是不够的 —— 若实现改成
// `return &Validation{OK:false}, nil`，err 也就是 nil，测试会漏。
func TestValidate_NoCoverReturnsErrorNotSoftPass(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.neg", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.secret"}, MaxLevel: authz.L1},
		Purpose: "无人可批",
	}
	v, err := f.svc.Validate(r, nil)
	if err == nil {
		t.Fatal("无人覆盖时必须返回 error，不得返回 (nil, nil) 或软通过")
	}
	if v != nil {
		t.Fatalf("★ fail-closed 违反：出错时必须返回 nil 校验结果，实际 %+v（调用方可能误用 OK 字段放行）", v)
	}
}

// ★ 对照：Submit 也必须把错误原样透传（不得吞掉 error 后返回可放行的 Validation）。
func TestSubmit_NoCoverPropagatesError(t *testing.T) {
	f := newFixture()
	r := &Request{
		ID: "req.neg2", Applicant: "u.staff",
		Draft:   Draft{Modules: []string{"module.secret"}, MaxLevel: authz.L1},
		Purpose: "无人可批",
	}
	v, err := f.svc.Submit(r, nil)
	if err == nil {
		t.Fatal("Submit 不得吞掉 Validate 的 error")
	}
	if v != nil {
		t.Fatalf("出错时 Submit 也应返回 nil，实际 %+v", v)
	}
	if r.Status == StatusApproving || r.Status == StatusApproved {
		t.Fatalf("★ 严禁在路由失败时把申请推进到 %s", r.Status)
	}
}

// ★ 预留：errors 包用于断言错误链，防止将来错误被包成不可识别的字符串。
var _ = errors.Is
