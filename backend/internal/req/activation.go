// activation.go —— M-REQ 闭环的最后一步：**获批申请生效**。
//
// 背景（这是一个真实的闭环缺口，不是补充装饰）：
//
//	docs/02 M-REQ 定义的闭环是
//	    勾选 → 提交 → 校验 → 路由审批 → **审批 → 自动开通** → 到期回收
//	而实现侧只做到了「审批」：req 包有 Validate / Submit / Approve / Cosign /
//	Reject / Withdraw / Reclaim，`Approve` 把状态置为 APPROVED 之后就**结束了**。
//	从「申请单 APPROVED」到「账号 Entitlement 真的多出这些权限」这一段，
//	**没有任何代码**（grep 全仓 `REQUEST_APPROVED` 只命中一处常量声明）。
//
//	后果：闸门 G7 明写「**申请未批不通** —— 未走完审批的 Entitlement 不生效（D14）」。
//	这条断言在实现侧是**恒真**的 —— 不是因为它被正确实现，而是因为**根本没有任何
//	生效路径**：批了也不通，没批当然也不通。一个永远为真的断言等于没有断言。
//
// 本文件补齐「自动开通」：把一份 **APPROVED** 的申请投影为 authz.Entitlement，
// 就地合并进目标账号的授权；并保证四条纪律：
//
//	1. **未批不通**：只有 Status == APPROVED 才生效，其余（含 APPROVING /
//	   COSIGN_PENDING / REJECTED / WITHDRAWN）一律返回错误且**不做任何修改**。
//	2. **幂等**：按申请单 ID 判重；同一单重复生效不会把权限叠两遍，也不会重复计数。
//	3. **D7 不可翻盘**：IT 账号无论批了什么，都不得因此获得业务数值可见性
//	   （canViewBusinessValues 恒 false）；且 IT 的生效申请直接拒绝。
//	4. **来源可溯**：生效时写入 GrantRecord（origin=REQUEST_APPROVED、授出者、
//	   申请单 ID、时间），使 EntitlementView.source.fromApprovedRequests 不再是空数组。
package req

import (
	"errors"
	"fmt"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
)

// ErrNotApproved —— 申请未走完审批，不得生效（G7「申请未批不通」，D14）。
var ErrNotApproved = errors.New("req: 申请未获批准，不得生效（D14）")

// ErrAlreadyApplied —— 该申请已生效过（幂等保护）。
var ErrAlreadyApplied = errors.New("req: 该申请已生效，跳过重复生效")

// ErrITCannotActivate —— IT 账号不得因申请获得业务数值权限（D7）。
var ErrITCannotActivate = errors.New("req: IT 账号不可因申请获得业务数值（D7）")

// ErrNotApplied —— 该申请从未生效过，无可回收（回收必须幂等且不误伤）。
var ErrNotApplied = errors.New("req: 该申请未生效过，无可回收")

// ErrNotReclaimable —— 不满足回收条件（未到期 / 长期申请）。
var ErrNotReclaimable = errors.New("req: 未到回收条件")

// Activate 把一份**已批准**的申请合并进目标账号的授权。
//
// 就地修改 target（调用方负责持久化；本函数不落库、不写审计，只算授权）。
// 返回 error 时 target **保证未被修改** —— 所有校验都在写入之前完成。
//
// 幂等：若 target.Grants 中已存在同 RequestID 的 REQUEST_APPROVED 记录，
// 返回 ErrAlreadyApplied 且不重复合并（调用方可据此判定为「已生效」而非失败）。
func Activate(r *Request, target *authz.Entitlement, now time.Time) error {
	if r == nil || target == nil {
		return errors.New("req: Activate 需要申请单与目标授权")
	}
	// ── 纪律 1：未批不通 ──
	if r.Status != StatusApproved {
		return fmt.Errorf("%w：当前状态 %s", ErrNotApproved, r.Status)
	}
	// ── 纪律 2：幂等（按申请单 ID 判重）──
	for _, g := range target.Grants {
		if g.Origin == authz.OriginRequest && g.RequestID != "" && g.RequestID == r.ID {
			return ErrAlreadyApplied
		}
	}
	// ── 纪律 3：D7 —— IT 不得获得业务数值 ──
	if isIT(target.BaseTemplate) || isIT(target.Account) {
		return fmt.Errorf("%w：账号 %s", ErrITCannotActivate, target.Account)
	}

	// ── 合并（并集语义，DENY 仍由 Resolve 统一兜底）──
	d := r.Draft

	// 模块：并入 enabled；若该模块当前在 disabled 中，保持 DENY（不翻案）。
	for _, m := range d.Modules {
		if contains(target.Modules.Disabled, m) {
			continue // DENY 优先：申请不能解除显式禁用
		}
		if !contains(target.Modules.Enabled, m) {
			target.Modules.Enabled = append(target.Modules.Enabled, m)
		}
	}

	// 维度：同维取并集（"*" 吞并其余）。
	for _, dg := range d.Dimensions {
		mergeDimension(target, dg)
	}

	// 勾选组：同组取并集；组依赖在 Resolve 中裁决（不在这里预判）。
	for _, gg := range d.DataUseGroups {
		mergeDataUseGroup(target, gg)
	}

	// 密级：取更宽松者（并集语义）。安全由「审批人自身覆盖」在上游把关。
	target.MaxLevel = authz.MaxLevel(target.MaxLevel, d.MaxLevel)

	// ── 纪律 4：来源可溯 + 时间盒 ──
	// 来源记录**自带到期时间** —— 这是「到期回收」能落地的前提：
	// 若来源记录里没有 ExpiresAt，回收器查不到任何到期信息，
	// 「到期自动回收」就会像此前的「自动开通」一样恒真（永远「没漏收」）。
	grantedBy := r.ApproverAccount()
	if grantedBy == "" {
		grantedBy = "system"
	}
	target.Grants = append(target.Grants, authz.GrantRecord{
		Origin:    authz.OriginRequest,
		GrantedBy: grantedBy,
		At:        now.UTC().Format(time.RFC3339),
		RequestID: r.ID,
		ExpiresAt: expiryOf(r),
		ModuleIDs: append([]string(nil), effectiveModules(d, target)...),
		GroupIDs:  groupIDsOf(d),
	})
	return nil
}

// effectiveModules 返回本单**实际落进 enabled 的**模块（DENY 掉的不算），
// 供回收时精确对账 —— 记「草案里写了什么」会让回收去撤一个从没生效过的模块。
func effectiveModules(d Draft, target *authz.Entitlement) []string {
	out := make([]string, 0, len(d.Modules))
	for _, m := range d.Modules {
		if contains(target.Modules.Disabled, m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func groupIDsOf(d Draft) []string {
	out := make([]string, 0, len(d.DataUseGroups))
	for _, g := range d.DataUseGroups {
		out = append(out, g.Group)
	}
	return out
}

// expiryOf 返回申请单的时间盒（RFC3339）；长期申请返回空串（= 永不到期）。
//
// ★ 「空串 = 永不到期」这条约定很关键：若把空串当作「立即到期」，
//   一次普通的长期申请会在下一秒被自己的回收器收走。
//   authz.isGrantExpired 用的是同一条约定（空 ⇒ false）。
func expiryOf(r *Request) string {
	if r == nil || r.RequestedExpiry == nil {
		return ""
	}
	return r.RequestedExpiry.UTC().Format(time.RFC3339)
}
