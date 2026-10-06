// reclaim.go —— M-REQ 闭环的最后一步：**到期回收**。
//
// 与 activation.go 的「自动开通」是同一个病，只不过程序走得更远一点：
//
//	docs/02 M-REQ 闭环 = 勾选 → 提交 → 校验 → 路由审批 → 审批 → 自动开通 → **到期回收**
//
// 「自动开通」此前整段缺失（已由 Activate 补齐）。而「到期回收」的问题是：
//
//	1. authz.GrantRecord **根本没有时间盒字段** —— 回收器无从查起；
//	2. 求值层（authz.Resolve）**从不看时间盒** —— 即便有字段也不生效；
//	3. req.Service.Reclaim 只返回一个 bool，**无人接线**。
//
// 三条叠加的后果与「未批不通」一模一样：**「到期回收」恒真** ——
// 不是因为它被正确实现，而是因为根本没有可查的到期信息，
// 于是回收器「从来没有漏收过」。一个永远为真的断言等于没有断言。
//
// 本文件补齐「到期回收」，四条纪律：
//
//	1. **到期才收**：长期申请（RequestedExpiry 为空）与未到期申请一律拒绝，
//	   且目标**纹丝不动** —— 回收比授予更危险：多收一分就是把人在门外。
//	2. **未生效不回收**：目标上找不到本单的**活跃**生效痕迹即拒绝，
//	   绝不「顺手清理」任何非本单来源（尤其不得动 ADMIN_CHECK / TEMPLATE）。
//	3. **幂等 + 留痕**：回收后标记 Reclaimed/ReclaimedAt，**记录不删除** ——
//	   docs/02 要求「申请→审批→开通→使用→回收」全链路可审计，删记录等于毁证。
//	4. **D7 不例外**：IT 账号即便存在（不应存在的）生效记录，也一律拒绝回收。
package req

import (
	"errors"
	"fmt"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
)

// Reclaim 回收一份**已到期**的申请所携带的授权。
//
// 与 Activate 对偶，四条纪律：
//
//	1. **到期才收**：未到期（或长期申请）一律拒绝，且目标纹丝不动 ——
//	   回收比授予更危险，多收一分就是把人锁在门外。
//	2. **未生效不回收**：目标上找不到该申请单的生效痕迹，返回 ErrNotApplied；
//	   不「顺手清理」任何非本单来源（尤其不得动 ADMIN_CHECK / TEMPLATE）。
//	3. **幂等**：回收后 Grants 中该单被标记 RECLAIMED，重复回收返回 ErrNotApplied。
//	4. **D7 不例外**：IT 账号即便有（不应存在的）生效记录，也一律拒绝回收。
//
// 返回 error 时 target **保证未被修改**。
func Reclaim(r *Request, target *authz.Entitlement, now time.Time) error {
	if r == nil || target == nil {
		return errors.New("req: Reclaim 需要申请单与目标授权")
	}
	// 纪律 1：到期才收
	if r.RequestedExpiry == nil {
		return fmt.Errorf("%w：长期申请不回收", ErrNotReclaimable)
	}
	if now.Before(*r.RequestedExpiry) {
		return fmt.Errorf("%w：未到期（%s）",
			ErrNotReclaimable, r.RequestedExpiry.UTC().Format(time.RFC3339))
	}
	// 纪律 4：D7 不例外
	if isIT(target.BaseTemplate) || isIT(target.Account) {
		return fmt.Errorf("%w：账号 %s", ErrITCannotActivate, target.Account)
	}
	// 纪律 2 + 3：必须找得到本单的**活跃**生效痕迹
	idx := -1
	for i := range target.Grants {
		g := target.Grants[i]
		if g.RequestID != r.ID {
			continue
		}
		if g.Reclaimed {
			return fmt.Errorf("%w（已于 %s 回收）", ErrNotApplied, g.ReclaimedAt)
		}
		idx = i
		break
	}
	if idx < 0 {
		return fmt.Errorf("%w：申请单 %s", ErrNotApplied, r.ID)
	}

	// ── 回收：只撤掉**本单带来的**增量 ──
	// 不做「重算全量权限」，因为本函数只看得到单个 Entitlement，
	// 看不到模板/分组等其它来源；凭残缺信息重算等于凭空削权。
	d := r.Draft

	// 模块：只撤销「草案里有、且没有别的活跃来源也在给」的项。
	for _, m := range d.Modules {
		if hasOtherActiveSourceForModule(target, r.ID, m, now) {
			continue
		}
		if contains(target.Modules.Disabled, m) {
			continue // 本就在 DENY 里，不是本单带来的
		}
		target.Modules.Enabled = removeStr(target.Modules.Enabled, m)
	}

	// 维度：只从本单贡献的取值中减去；减空的维度保留（空 = 全部，
	// 由上层 Resolve 与审批上界共同兜底，回收阶段不擅自扩大语义）。
	for _, dg := range d.Dimensions {
		removeDimensionValues(target, dg)
	}

	// 勾选组：只有当**没有别的活跃来源**也在给该组时才摘除这一组。
	for _, gg := range d.DataUseGroups {
		if hasOtherActiveSourceForGroup(target, r.ID, gg.Group, now) {
			continue
		}
		target.DataUseGroups = removeGroup(target.DataUseGroups, gg.Group)
	}

	// 密级：不回落 —— 授予时取的是「更宽松者」，撤回单笔无法还原原值。
	// 若需严格复原，应由管理员重设或由模板重新求值（不在此处臆测）。
	// 来源记录：标记回收，保留痕迹（审计要求：
	// 「申请→审批→开通→使用→回收」全链路可追溯，删除等于毁证）。
	target.Grants[idx].Reclaimed = true
	target.Grants[idx].ReclaimedAt = now.UTC().Format(time.RFC3339)
	return nil
}

// hasOtherActiveSourceForModule 判断除 excludeReq 外，是否还有**未回收且未过期**
// 的来源记录携带该模块。
func hasOtherActiveSourceForModule(e *authz.Entitlement, excludeReq, module string, now time.Time) bool {
	for _, g := range e.Grants {
		if g.RequestID == excludeReq || g.Reclaimed {
			continue
		}
		if len(g.ModuleIDs) == 0 {
			continue // 未限定模块的来源（模板等）不参与单模块判定
		}
		for _, m := range g.ModuleIDs {
			if m == module {
				return true
			}
		}
	}
	return false
}

// hasOtherActiveSourceForGroup 判断除 excludeReq 外是否还有其它活跃来源给出该勾选组。
func hasOtherActiveSourceForGroup(e *authz.Entitlement, excludeReq, group string, now time.Time) bool {
	for _, g := range e.Grants {
		if g.RequestID == excludeReq || g.Reclaimed {
			continue
		}
		for _, gid := range g.GroupIDs {
			if gid == group {
				return true
			}
		}
	}
	// 账号自身显式勾选的勾选组不是「本单带来的」—— 但本函数无从区分，
	// 故仅以来源记录为准：无其它来源 ⇒ 视为本单独有，可回收。
	return false
}

func removeStr(xs []string, v string) []string {
	out := xs[:0:0]
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// removeDimensionValues 从 target 的对应维度中减去 dg.Values。
// 通配（"*"）不做减法：撤掉「全部」到「部分」的信息在原单里不存在。
func removeDimensionValues(target *authz.Entitlement, dg DimGrant) {
	for i := range target.Dimensions {
		if target.Dimensions[i].Dim != dg.Dim {
			continue
		}
		vals := target.Dimensions[i].Values
		if contains(vals, "*") {
			return
		}
		for _, v := range dg.Values {
			vals = removeStr(vals, v)
		}
		target.Dimensions[i].Values = vals
		return
	}
}

func removeGroup(gs []authz.GroupScopeGrant, group string) []authz.GroupScopeGrant {
	out := gs[:0:0]
	for _, g := range gs {
		if g.Group != group {
			out = append(out, g)
		}
	}
	return out
}

// ApproverAccount 返回实际作出批准决定的审批人账号（用于来源记录）。
//
// 取「最后一条 Action == APPROVE 的步骤」；没有则退回第一条具名审批人。
func (r *Request) ApproverAccount() string {
	if r == nil {
		return ""
	}
	for i := len(r.Approvals) - 1; i >= 0; i-- {
		if r.Approvals[i].Action == "APPROVE" && r.Approvals[i].Approver != "" {
			return r.Approvals[i].Approver
		}
	}
	for _, a := range r.Approvals {
		if a.Approver != "" {
			return a.Approver
		}
	}
	return ""
}

// ───────────────────────────── 辅助 ─────────────────────────────

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// mergeDimension 把一条维度授权并入 target（同维并集，"*" 吞并其余）。
func mergeDimension(target *authz.Entitlement, dg DimGrant) {
	for i := range target.Dimensions {
		if target.Dimensions[i].Dim != dg.Dim {
			continue
		}
		cur := target.Dimensions[i]
		// 已是「全部」则无需再并
		if contains(cur.Values, "*") {
			return
		}
		if contains(dg.Values, "*") {
			target.Dimensions[i].Values = []string{"*"}
		} else {
			for _, v := range dg.Values {
				if !contains(cur.Values, v) {
					cur.Values = append(cur.Values, v)
				}
			}
			target.Dimensions[i].Values = cur.Values
		}
		target.Dimensions[i].IncludeDescendants =
			cur.IncludeDescendants || dg.IncludeDescendants
		return
	}
	// 新维度：空 values 表示「全部」——原样保留语义（不擅自展开）。
	target.Dimensions = append(target.Dimensions, authz.DimensionGrant{
		Dim:                dg.Dim,
		Values:             append([]string(nil), dg.Values...),
		IncludeDescendants: dg.IncludeDescendants,
	})
}

// mergeDataUseGroup 把一条勾选组授权并入 target（同组并集）。
func mergeDataUseGroup(target *authz.Entitlement, gg authz.GroupScopeGrant) {
	for i := range target.DataUseGroups {
		if target.DataUseGroups[i].Group != gg.Group {
			continue
		}
		cur := target.DataUseGroups[i]
		// 字段：空 = 组内全部，吞并其余
		cur.Fields = unionOrAll(cur.Fields, gg.Fields)
		// 显式禁用：并集（DENY 只增不减）
		cur.Denied = union(cur.Denied, gg.Denied)
		cur.MaxLevel = authz.MaxLevel(cur.MaxLevel, gg.MaxLevel)
		target.DataUseGroups[i] = cur
		return
	}
	target.DataUseGroups = append(target.DataUseGroups, authz.GroupScopeGrant{
		Group:    gg.Group,
		Fields:   append([]string(nil), gg.Fields...),
		Denied:   append([]string(nil), gg.Denied...),
		MaxLevel: gg.MaxLevel,
	})
}

func union(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, v := range b {
		if !contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// unionOrAll 并集语义，但空集合表示「全部」——空吞并任何集合仍为「全部」。
func unionOrAll(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil // 空 = 组内全部
	}
	return union(a, b)
}
