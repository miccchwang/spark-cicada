package req_test

import (
	"errors"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

// ─────────────────────────────────────────────────────────────────────────────
// 闸门：G7「申请未批不通」（D14）+ 获批生效的幂等 / D7 / 来源溯源
//
// 这组断言针对的是一个**曾经整段缺失**的闭环：
//   docs/02 M-REQ 要求「审批 → 自动开通 → 到期回收」，
//   但 v1.2 之前 `Approve` 置 APPROVED 之后没有任何代码把授权落到账号上。
//   于是 G7「申请未批不通」在实现侧恒真 —— 不是因为实现正确，而是因为
//   **压根不存在生效路径**。本闸门钉死「存在生效路径」且「未批绝不生效」。
//
// 每条正向断言都配一条负向自测（_Negative_），证明闸门在实现被改坏时会响。
// ─────────────────────────────────────────────────────────────────────────────

func baseEnt(account string) *authz.Entitlement {
	return &authz.Entitlement{
		Account:   account,
		MaxLevel:  authz.L1,
		Modules:   authz.ModuleGrant{},
		Dimensions: []authz.DimensionGrant{},
	}
}

func approvedReq(id string) *req.Request {
	return &req.Request{
		ID:        id,
		Applicant: "acc.ops1",
		Draft: req.Draft{
			Modules:  []string{"module.report", "module.pnl"},
			MaxLevel: authz.L2,
			Dimensions: []req.DimGrant{
				{Dim: "channel", Values: []string{"c1"}, IncludeDescendants: true},
			},
			DataUseGroups: []authz.GroupScopeGrant{
				{Group: string(authz.GrpOps), MaxLevel: authz.L2},
			},
		},
		Status:   req.StatusApproved,
		Approvals: []req.ApprovalStep{
			{Approver: "acc.mgr", Tier: "T2", Action: "APPROVE", At: time.Now()},
		},
	}
}

// ───────────── ① 乐观情形：获批 → 真的生效 ─────────────

func TestActivate_ApprovedRequest_TakesEffect(t *testing.T) {
	target := baseEnt("acc.ops1")
	r := approvedReq("REQ-0001")

	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("获批申请应可生效，却失败: %v", err)
	}

	// 模块真的落上去了（不是「声称已生效」）
	for _, m := range []string{"module.report", "module.pnl"} {
		if !contains(target.Modules.Enabled, m) {
			t.Errorf("模块 %s 未生效", m)
		}
	}
	// 维度
	if len(target.Dimensions) != 1 || target.Dimensions[0].Dim != "channel" {
		t.Fatalf("维度未生效: %+v", target.Dimensions)
	}
	if !target.Dimensions[0].IncludeDescendants {
		t.Error("includeDescendants 丢失")
	}
	// 密级
	if target.MaxLevel != authz.L2 {
		t.Errorf("密级未提升: got %s want L2", target.MaxLevel)
	}
	// 勾选组
	if len(target.DataUseGroups) != 1 || target.DataUseGroups[0].Group != string(authz.GrpOps) {
		t.Errorf("勾选组未生效: %+v", target.DataUseGroups)
	}
	// 来源可溯：这是「来源分层」断言的另一半 —— 没有 GrantRecord，
	// EntitlementView.source.fromApprovedRequests 就永远是空数组。
	if len(target.Grants) != 1 {
		t.Fatalf("应写入 1 条来源记录，实得 %d", len(target.Grants))
	}
	g := target.Grants[0]
	if g.Origin != authz.OriginRequest {
		t.Errorf("来源类型错误: %s", g.Origin)
	}
	if g.RequestID != "REQ-0001" {
		t.Errorf("来源未关联申请单: %q", g.RequestID)
	}
	if g.GrantedBy != "acc.mgr" {
		t.Errorf("来源未记录审批人: %q", g.GrantedBy)
	}
}

// ───────────── ② 未批不通（G7 核心断言，D14）─────────────

func TestActivate_NotApproved_Rejected(t *testing.T) {
	// 所有「非 APPROVED」状态都必须被拒，且**不得修改目标授权**。
	for _, st := range []req.Status{
		req.StatusDraft,
		req.StatusSubmitted,
		req.StatusApproving,
		req.StatusCosignPending,
		req.StatusRejected,
		req.StatusWithdrawn,
		req.StatusBlockedCrossDep,
	} {
		t.Run(string(st), func(t *testing.T) {
			target := baseEnt("acc.ops1")
			r := approvedReq("REQ-" + string(st))
			r.Status = st

			err := req.Activate(r, target, time.Now())
			if !errors.Is(err, req.ErrNotApproved) {
				t.Fatalf("状态 %s 应被拒（ErrNotApproved），实得: %v", st, err)
			}
			// 断言「未批不通」的实质：不能只靠返回 error，目标必须纹丝不动
			assertUntouched(t, target)
		})
	}
}

func assertUntouched(t *testing.T, e *authz.Entitlement) {
	t.Helper()
	if len(e.Modules.Enabled) != 0 || len(e.Modules.Disabled) != 0 {
		t.Errorf("未批申请却改动了模块: %+v", e.Modules)
	}
	if len(e.Dimensions) != 0 {
		t.Errorf("未批申请却改动了维度: %+v", e.Dimensions)
	}
	if len(e.DataUseGroups) != 0 {
		t.Errorf("未批申请却改动了勾选组: %+v", e.DataUseGroups)
	}
	if e.MaxLevel != authz.L1 {
		t.Errorf("未批申请却改动了密级: %s", e.MaxLevel)
	}
	if len(e.Grants) != 0 {
		t.Errorf("未批申请却写了来源记录: %+v", e.Grants)
	}
}

// ───────────── ③ 幂等：同一单重复生效不叠权限 ─────────────

func TestActivate_Idempotent(t *testing.T) {
	target := baseEnt("acc.ops1")
	r := approvedReq("REQ-0002")

	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("首次生效失败: %v", err)
	}
	nMod1 := len(target.Modules.Enabled)
	nDim1 := len(target.Dimensions)
	nGrp1 := len(target.DataUseGroups)

	err := req.Activate(r, target, time.Now())
	if !errors.Is(err, req.ErrAlreadyApplied) {
		t.Fatalf("重复生效应返回 ErrAlreadyApplied，实得: %v", err)
	}
	if len(target.Modules.Enabled) != nMod1 {
		t.Errorf("重复生效叠加了模块: %d → %d", nMod1, len(target.Modules.Enabled))
	}
	if len(target.Dimensions) != nDim1 {
		t.Errorf("重复生效叠加了维度: %d → %d", nDim1, len(target.Dimensions))
	}
	if len(target.DataUseGroups) != nGrp1 {
		t.Errorf("重复生效叠加了勾选组: %d → %d", nGrp1, len(target.DataUseGroups))
	}
	if len(target.Grants) != 1 {
		t.Errorf("重复生效重复记来源: %d 条", len(target.Grants))
	}
}

// ───────────── ④ D7：IT 不可因申请获得业务数值 ─────────────

func TestActivate_ITAccount_Blocked_D7(t *testing.T) {
	target := baseEnt("acc.it")
	target.BaseTemplate = "tpl.it"
	r := approvedReq("REQ-0003")

	err := req.Activate(r, target, time.Now())
	if !errors.Is(err, req.ErrITCannotActivate) {
		t.Fatalf("IT 账号生效应被拒（D7），实得: %v", err)
	}
	assertUntouched(t, target)
}

// 变体：模板为空但账号名本身标识 IT（isIT 的双判定路径）。
func TestActivate_ITAccount_ByAccountName_D7(t *testing.T) {
	target := baseEnt("tpl.it.svc")
	r := approvedReq("REQ-0004")

	if err := req.Activate(r, target, time.Now()); !errors.Is(err, req.ErrITCannotActivate) {
		t.Fatalf("账号名标识 IT 时也应被拒，实得: %v", err)
	}
	assertUntouched(t, target)
}

// ───────────── ⑤ DENY 优先：申请不得解除显式禁用 ─────────────

func TestActivate_DenyWins_CannotReEnableDisabledModule(t *testing.T) {
	target := baseEnt("acc.ops1")
	// 管理员显式禁用了 module.pnl
	target.Modules.Disabled = []string{"module.pnl"}
	r := approvedReq("REQ-0005") // 草案里含 module.pnl

	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	// DENY 优先：即便申请里带了它，也不得重新启用
	if contains(target.Modules.Enabled, "module.pnl") {
		t.Error("DENY 被申请翻案：module.pnl 不应出现在 enabled")
	}
	if !contains(target.Modules.Enabled, "module.report") {
		t.Error("未被禁用的模块应正常生效")
	}
}

// 端到端确认 DENY 仍由 Resolve 兜底（申请生效不改变 DENY 优先语义）。
func TestActivate_DenyStillWins_AfterResolve(t *testing.T) {
	target := baseEnt("acc.ops1")
	target.Modules.Disabled = []string{"module.pnl"}
	r := approvedReq("REQ-0006")
	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	view := authz.NewResolver(nil).Resolve(target, nil)
	if contains(view.Modules, "module.pnl") {
		t.Error("Resolve 后 module.pnl 仍然可见 —— DENY 未生效")
	}
}

// ───────────── ⑥ 来源分层：fromApprovedRequests 不再恒空 ─────────────

func TestResolve_SourceFromApprovedRequests_Populated(t *testing.T) {
	target := baseEnt("acc.ops1")
	r := approvedReq("REQ-0007")
	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("生效失败: %v", err)
	}

	view := authz.NewResolver(nil).Resolve(target, nil)
	if len(view.Source.FromApproved) == 0 {
		t.Fatal("fromApprovedRequests 仍为空 —— 来源分层未接线")
	}
	// 必须能定位到具体申请单
	found := false
	for _, s := range view.Source.FromApproved {
		if s == "request:acc.mgr@REQ-0007" {
			found = true
		}
	}
	if !found {
		t.Errorf("来源标识不可定位到申请单: %v", view.Source.FromApproved)
	}
}

// 负向自测：来源分层必须**如实**——代授来源不能混进「已批准申请」桶，
// 否则排障时会把「上级代授」误当成「申请获批」，追责链条断掉。
func TestResolve_SourceLayering_DelegationNotInApprovedBucket(t *testing.T) {
	e := baseEnt("acc.ops1")
	e.Grants = []authz.GrantRecord{
		{Origin: authz.OriginSupervisor, GrantedBy: "acc.dir", At: "2026-10-06T00:00:00Z"},
	}
	view := authz.NewResolver(nil).Resolve(e, nil)
	if len(view.Source.FromApproved) != 0 {
		t.Errorf("代授被误记入已批准申请桶: %v", view.Source.FromApproved)
	}
	if len(view.Source.FromDelegation) != 1 {
		t.Fatalf("代授未记入 fromDelegations: %v", view.Source.FromDelegation)
	}
	if view.Source.FromDelegation[0] != "delegate:acc.dir" {
		t.Errorf("代授来源标识错误: %v", view.Source.FromDelegation)
	}
}

// 负向自测：未知来源不得被静默丢弃。
//
// 注意基线条目：Resolve 在并入账号自身授权时，会无条件追加一条
// `entitlement:<account>` 到 fromGrant（第 3 步的固定痕迹），
// 因此基线恒为 1 条。本测试断言的是「未知来源**额外**产生 1 条」——
// 若把 default 分支写成静默丢弃，合计会掉回 1（而非 2），据此失败。
func TestResolve_SourceLayering_UnknownOriginNotSilentlyDropped(t *testing.T) {
	e := baseEnt("acc.ops1")
	e.Grants = []authz.GrantRecord{
		{Origin: authz.GrantOrigin("SOMETHING_NEW"), GrantedBy: "acc.x"},
	}
	view := authz.NewResolver(nil).Resolve(e, nil)
	total := len(view.Source.FromTemplate) + len(view.Source.FromGroups) +
		len(view.Source.FromGrant) + len(view.Source.FromApproved) +
		len(view.Source.FromDelegation) + len(view.Source.FromTemp)
	// 1 条基线（entitlement:acc.ops1）+ 1 条未知来源
	if total != 2 {
		t.Fatalf("未知来源被静默丢弃（6 桶合计 %d 条，应为 2 = 1 基线 + 1 未知）", total)
	}
	found := false
	for _, s := range view.Source.FromGrant {
		if s == "unknown:SOMETHING_NEW:acc.x" {
			found = true
		}
	}
	if !found {
		t.Errorf("未知来源未显式标注: %v", view.Source.FromGrant)
	}
}

// ───────────── ⑦ 并集语义：重复申请同一维度不重复列示 ─────────────

func TestActivate_MergeDimensions_UnionNoDuplicates(t *testing.T) {
	target := baseEnt("acc.ops1")
	target.Dimensions = []authz.DimensionGrant{{Dim: "channel", Values: []string{"c1"}}}

	r := approvedReq("REQ-0008")
	r.Draft.Dimensions = []req.DimGrant{
		{Dim: "channel", Values: []string{"c1", "c2"}},
	}
	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	vals := target.Dimensions[0].Values
	if len(vals) != 2 {
		t.Fatalf("维度并集应为 {c1,c2}，实得 %v", vals)
	}
	seen := map[string]int{}
	for _, v := range vals {
		seen[v]++
	}
	if seen["c1"] != 1 {
		t.Errorf("维度取值重复: %v", vals)
	}
}

// 变体：新维度用「*」通配，应吞并而不是并列（避免出现 ["*","c9"] 这种矛盾集）。
func TestActivate_MergeDimensions_WildcardAbsorbs(t *testing.T) {
	target := baseEnt("acc.ops1")
	target.Dimensions = []authz.DimensionGrant{{Dim: "brand", Values: []string{"b1", "b2"}}}
	r := approvedReq("REQ-0009")
	r.Draft.Dimensions = []req.DimGrant{{Dim: "brand", Values: []string{"*"}}}

	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	if len(target.Dimensions[0].Values) != 1 || target.Dimensions[0].Values[0] != "*" {
		t.Errorf("通配未吞并: %v", target.Dimensions[0].Values)
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
