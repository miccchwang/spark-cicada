package req_test

import (
	"errors"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

// ─────────────────────────────────────────────────────────────────────────────
// 闸门：G7「到期自动回收」（docs/02 M-REQ 闭环最后一步）
//
// 这一段的处境与「自动开通」一模一样：
//   docs/02 写「审批 → 自动开通 → **到期回收**」，
//   但 GrantRecord 里**根本没有时间盒字段**，Resolve 也从不看时间盒。
//   于是「到期回收」在实现侧恒真 —— 回收器无从查起，自然「从没漏收」。
//   一个永远为真的断言等于没有断言（与 activation.go 开头同一个病）。
//
// 本闸门钉死四件事：
//   ① 时间盒写进来源记录（没有它，回收无从谈起）
//   ② 未到期绝不回收（回收比授予更危险：多收一分就是把人在门外）
//   ③ 只撤本单增量，不误伤其它来源 / 不越权削权
//   ④ 回收幂等且留痕（全链路可审计，删除等于毁证）
//
// 每条正向断言配一条负向自测（_Negative_），证明闸门在实现被改坏时会响。
// ─────────────────────────────────────────────────────────────────────────────

func expiryReq(id string, exp time.Time) *req.Request {
	r := approvedReq(id)
	e := exp
	r.RequestedExpiry = &e
	return r
}

// ───────────── ① 时间盒必须写进来源记录 ─────────────

func TestActivate_RecordsTimeBoxInGrant(t *testing.T) {
	exp := time.Now().Add(24 * time.Hour)
	target := baseEnt("acc.ops1")
	r := expiryReq("REQ-EXP-1", exp)

	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	if len(target.Grants) != 1 {
		t.Fatalf("应写 1 条来源记录，实得 %d", len(target.Grants))
	}
	got := target.Grants[0].ExpiresAt
	want := exp.UTC().Format(time.RFC3339)
	if got != want {
		t.Errorf("来源记录未携带时间盒: got %q want %q", got, want)
	}
}

// 负向自测：长期申请（RequestedExpiry=nil）**不得**被写成「立即到期」，
// 否则一次普通申请会在下一秒被自己回收掉。
func TestActivate_LongTermRequest_NoExpiry_Negative(t *testing.T) {
	target := baseEnt("acc.ops1")
	r := approvedReq("REQ-LONG-1") // RequestedExpiry == nil
	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	if target.Grants[0].ExpiresAt != "" {
		t.Errorf("长期申请被误写了到期时间: %q", target.Grants[0].ExpiresAt)
	}
}

// ───────────── ② 未到期绝不回收 ─────────────

func TestReclaim_NotYetExpired_RefusedAndUntouched(t *testing.T) {
	exp := time.Now().Add(1 * time.Hour)
	target := baseEnt("acc.ops1")
	r := expiryReq("REQ-NOTDUE", exp)
	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	snapshot := snapshotEnt(target)

	err := req.Reclaim(r, target, time.Now())
	if !errors.Is(err, req.ErrNotReclaimable) {
		t.Fatalf("未到期应拒绝回收（ErrNotReclaimable），实得: %v", err)
	}
	assertSameEnt(t, snapshot, target, "未到期却改动了授权")
}

// 负向自测：恰好等于到期时刻 = **已到期**（与 authz.isGrantExpired 同口径）。
// 若实现写成 now.After(expiry)，边界那一秒会漏收。
func TestReclaim_ExactlyAtExpiry_IsReclaimable_Negative(t *testing.T) {
	exp := time.Now().Add(-1 * time.Second)
	target := baseEnt("acc.ops1")
	r := expiryReq("REQ-EXACT", exp)
	if err := req.Activate(r, target, exp.Add(-time.Minute)); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	if err := req.Reclaim(r, target, exp); err != nil {
		t.Fatalf("到期时刻应可回收，实得: %v", err)
	}
	if !target.Grants[0].Reclaimed {
		t.Error("回收未留痕")
	}
}

// 长期申请永不回收。
func TestReclaim_LongTermNeverReclaimed(t *testing.T) {
	target := baseEnt("acc.ops1")
	r := approvedReq("REQ-LONG-2")
	if err := req.Activate(r, target, time.Now()); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	err := req.Reclaim(r, target, time.Now().AddDate(5, 0, 0))
	if !errors.Is(err, req.ErrNotReclaimable) {
		t.Fatalf("长期申请不应被回收，实得: %v", err)
	}
}

// ───────────── ③ 只撤本单增量（不误伤、不越权） ─────────────

func TestReclaim_RemovesOnlyThisRequestDelta(t *testing.T) {
	exp := time.Now().Add(-time.Hour)
	target := baseEnt("acc.ops1")
	// 管理员手工授予的模块 + 显式 DENY，都不属于任何申请单
	target.Modules.Enabled = []string{"module.dashboard"}
	target.Modules.Disabled = []string{"module.pnl"}

	r := expiryReq("REQ-DELTA", exp)
	r.Draft.Modules = []string{"module.report", "module.pnl"} // pnl 被 DENY，本就不生效
	if err := req.Activate(r, target, exp.Add(-time.Hour)); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	if err := req.Reclaim(r, target, exp); err != nil {
		t.Fatalf("回收失败: %v", err)
	}

	// 本单带来的 module.report 应被撤
	if contains(target.Modules.Enabled, "module.report") {
		t.Error("本单授予的 module.report 未被回收")
	}
	// 管理员手工授予的不得被顺手削掉
	if !contains(target.Modules.Enabled, "module.dashboard") {
		t.Error("回收越权：管理员手工授予的 module.dashboard 被削掉")
	}
	// DENY 不得被回收逻辑解除
	if !contains(target.Modules.Disabled, "module.pnl") {
		t.Error("回收改动了 DENY 清单")
	}
}

// 负向自测：同一模块若还有**其它活跃来源**在给，回收本单不得把模块摘掉。
func TestReclaim_ModuleKeptWhenOtherSourceGrantsIt_Negative(t *testing.T) {
	exp := time.Now().Add(-time.Hour)
	target := baseEnt("acc.ops1")
	r := expiryReq("REQ-OTHER", exp)
	r.Draft.Modules = []string{"module.report"}
	if err := req.Activate(r, target, exp.Add(-time.Hour)); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	// 另一条未回收、未限模块的来源也在给 module.report
	target.Grants = append(target.Grants, authz.GrantRecord{
		Origin:    authz.OriginSupervisor,
		GrantedBy: "acc.dir",
		ModuleIDs: []string{"module.report"},
	})
	if err := req.Reclaim(r, target, exp); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if !contains(target.Modules.Enabled, "module.report") {
		t.Error("回收误伤：仍有其它活跃来源在给 module.report，却被摘掉")
	}
}

// 维度：只减本单带来的取值。
func TestReclaim_DimensionValuesSubtractedNotWiped(t *testing.T) {
	exp := time.Now().Add(-time.Hour)
	target := baseEnt("acc.ops1")
	target.Dimensions = []authz.DimensionGrant{
		{Dim: "channel", Values: []string{"c0"}}, // 管理员原有
	}
	r := expiryReq("REQ-DIM", exp)
	r.Draft.Dimensions = []req.DimGrant{{Dim: "channel", Values: []string{"c1", "c2"}}}
	if err := req.Activate(r, target, exp.Add(-time.Hour)); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	if len(target.Dimensions[0].Values) != 3 {
		t.Fatalf("生效后应有 3 个取值，实得 %v", target.Dimensions[0].Values)
	}
	if err := req.Reclaim(r, target, exp); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	vals := target.Dimensions[0].Values
	if len(vals) != 1 || vals[0] != "c0" {
		t.Errorf("回收应只减去本单的 c1/c2，实得 %v", vals)
	}
}

// 勾选组：无其它来源时摘除该组；有其它来源时保留。
func TestReclaim_GroupRemovedAndKept(t *testing.T) {
	exp := time.Now().Add(-time.Hour)

	// 情形 A：本单独有 ⇒ 摘除
	a := baseEnt("acc.ops1")
	ra := expiryReq("REQ-GA", exp)
	if err := req.Activate(ra, a, exp.Add(-time.Hour)); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	if err := req.Reclaim(ra, a, exp); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if len(a.DataUseGroups) != 0 {
		t.Errorf("本单独有的勾选组未被回收: %+v", a.DataUseGroups)
	}

	// 情形 B：另有来源记录也给同一组 ⇒ 保留
	b := baseEnt("acc.ops1")
	rb := expiryReq("REQ-GB", exp)
	if err := req.Activate(rb, b, exp.Add(-time.Hour)); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	b.Grants = append(b.Grants, authz.GrantRecord{
		Origin:    authz.OriginSupervisor,
		GrantedBy: "acc.dir",
		GroupIDs:  []string{string(authz.GrpOps)},
	})
	if err := req.Reclaim(rb, b, exp); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	found := false
	for _, g := range b.DataUseGroups {
		if string(g.Group) == string(authz.GrpOps) {
			found = true
		}
	}
	if !found {
		t.Error("回收误伤：另有来源也在给 grp.ops，却被摘掉")
	}
}

// ───────────── ④ 幂等 + 留痕 + 未生效不回收 ─────────────

func TestReclaim_Idempotent(t *testing.T) {
	exp := time.Now().Add(-time.Hour)
	target := baseEnt("acc.ops1")
	r := expiryReq("REQ-IDEM", exp)
	if err := req.Activate(r, target, exp.Add(-time.Hour)); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	if err := req.Reclaim(r, target, exp); err != nil {
		t.Fatalf("首次回收失败: %v", err)
	}
	err := req.Reclaim(r, target, exp.Add(time.Minute))
	if !errors.Is(err, req.ErrNotApplied) {
		t.Fatalf("重复回收应返回 ErrNotApplied，实得: %v", err)
	}
	// 留痕：记录**不得**被删除（审计要求「申请→审批→开通→使用→回收」全链路可溯）
	if len(target.Grants) != 1 {
		t.Fatalf("回收删除了来源记录（毁证）: %d 条", len(target.Grants))
	}
	if !target.Grants[0].Reclaimed || target.Grants[0].ReclaimedAt == "" {
		t.Error("回收未留痕（Reclaimed/ReclaimedAt 为空）")
	}
}

// 负向自测：从没生效过的申请不得被「回收」—— 否则等于凭空削权。
func TestReclaim_NeverApplied_Refused_Negative(t *testing.T) {
	target := baseEnt("acc.ops1")
	target.Modules.Enabled = []string{"module.report"}
	snap := snapshotEnt(target)

	r := expiryReq("REQ-NEVER", time.Now().Add(-time.Hour))
	err := req.Reclaim(r, target, time.Now())
	if !errors.Is(err, req.ErrNotApplied) {
		t.Fatalf("未生效过应返回 ErrNotApplied，实得: %v", err)
	}
	assertSameEnt(t, snap, target, "未生效的申请却改动了授权")
}

// 回收后 Resolve 必须不再给权限，但来源分层仍看得见这条已被收回的记录。
func TestReclaim_AfterResolve_PermissionGoneButAuditable(t *testing.T) {
	exp := time.Now().Add(-time.Hour)
	target := baseEnt("acc.ops1")
	r := expiryReq("REQ-AUD", exp)
	if err := req.Activate(r, target, exp.Add(-time.Hour)); err != nil {
		t.Fatalf("生效失败: %v", err)
	}
	if err := req.Reclaim(r, target, exp); err != nil {
		t.Fatalf("回收失败: %v", err)
	}

	view := authz.NewResolver(nil).Resolve(target, nil)
	// 权限确实没了（DENY 之外的路径也不给）
	for _, m := range []string{"module.report", "module.pnl"} {
		if contains(view.Modules, m) {
			t.Errorf("回收后仍可见 %s —— 回收未生效", m)
		}
	}
	// 但审计痕迹必须留存且被显式标注
	hit := false
	for _, s := range view.Source.ExpiredGrants {
		if len(s) > len("request:") && s[:len("request:")] == "request:" {
			hit = true
		}
	}
	if !hit {
		t.Errorf("回收记录未进入 ExpiredGrants（不可审计）: %v", view.Source.ExpiredGrants)
	}
}

// 时间盒过期（未显式回收）也必须在 Resolve 层失效 —— 两条路径都要堵住。
func TestResolve_ExpiredGrantInactive(t *testing.T) {
	e := baseEnt("acc.it.z") // 非 IT 名，避免 D7 干扰
	e.Grants = []authz.GrantRecord{{
		Origin:    authz.OriginRequest,
		GrantedBy: "acc.mgr",
		At:        "2026-01-01T00:00:00Z",
		ExpiresAt: "2026-01-02T00:00:00Z", // 早已过期
		RequestID: "REQ-PAST",
	}}
	view := authz.NewResolver(nil).Resolve(e, nil)
	if len(view.Source.ExpiredGrants) == 0 {
		t.Error("过期来源未进入 ExpiredGrants")
	}
}

// 负向自测：到期时间**不可解析**必须 fail-closed（当作已过期）。
// 若实现把解析失败当作「永不过期」，一个格式写错的到期时间会变成永久授权。
func TestResolve_MalformedExpiry_FailsClosed_Negative(t *testing.T) {
	e := baseEnt("acc.ops1")
	e.Grants = []authz.GrantRecord{{
		Origin:    authz.OriginRequest,
		GrantedBy: "acc.mgr",
		ExpiresAt: "not-a-timestamp",
		RequestID: "REQ-BAD",
	}}
	view := authz.NewResolver(nil).Resolve(e, nil)
	if len(view.Source.ExpiredGrants) == 0 {
		t.Error("不可解析的到期时间被当作永不过期（应 fail-closed）")
	}
}

// ───────────────────────── 辅助 ─────────────────────────

type entSnap struct {
	mods   []string
	dis    []string
	dims   []authz.DimensionGrant
	groups []authz.GroupScopeGrant
	level  authz.Level
	grants int
}

func snapshotEnt(e *authz.Entitlement) entSnap {
	return entSnap{
		mods:   append([]string(nil), e.Modules.Enabled...),
		dis:    append([]string(nil), e.Modules.Disabled...),
		dims:   append([]authz.DimensionGrant(nil), e.Dimensions...),
		groups: append([]authz.GroupScopeGrant(nil), e.DataUseGroups...),
		level:  e.MaxLevel,
		grants: len(e.Grants),
	}
}

func assertSameEnt(t *testing.T, s entSnap, e *authz.Entitlement, msg string) {
	t.Helper()
	if len(e.Modules.Enabled) != len(s.mods) {
		t.Errorf("%s: enabled 变了 %v → %v", msg, s.mods, e.Modules.Enabled)
	}
	if len(e.Modules.Disabled) != len(s.dis) {
		t.Errorf("%s: disabled 变了 %v → %v", msg, s.dis, e.Modules.Disabled)
	}
	if len(e.Dimensions) != len(s.dims) {
		t.Errorf("%s: dimensions 变了", msg)
	}
	if len(e.DataUseGroups) != len(s.groups) {
		t.Errorf("%s: dataUseGroups 变了", msg)
	}
	if e.MaxLevel != s.level {
		t.Errorf("%s: maxLevel 变了 %s → %s", msg, s.level, e.MaxLevel)
	}
	if len(e.Grants) != s.grants {
		t.Errorf("%s: grants 变了 %d → %d", msg, s.grants, len(e.Grants))
	}
}
