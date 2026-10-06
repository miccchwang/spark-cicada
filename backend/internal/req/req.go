// Package req —— M-REQ 权限申请流（D14）。
//
// 闭环：勾选数据权限 → 提交 → 校验 → 路由审批 → 审批 → 自动开通 → 到期回收。
//
// 约束（docs/02 M-REQ、contracts/permission-request.ts）：
//   - 审批人必须**自身拥有**被申请的全部权限（S ⊆ 权限(P)）
//   - 上级权限不足 → 自动升级到最近的权限超集上级
//   - 越级/跨枝 → 最近公共上级；无则 T1 兜底
//   - IT 不可申请业务数值（D7）
//   - 跨部门 → BLOCKED_CROSS_DEPT（需 T1 直接授予，不可自助申请）
package req

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
)

// Status 申请单状态。
type Status string

const (
	StatusDraft           Status = "DRAFT"
	StatusSubmitted       Status = "SUBMITTED"
	StatusApproving       Status = "APPROVING"
	StatusCosignPending   Status = "COSIGN_PENDING"
	StatusApproved        Status = "APPROVED"
	StatusRejected        Status = "REJECTED"
	StatusWithdrawn       Status = "WITHDRAWN"
	StatusBlockedCrossDep Status = "BLOCKED_CROSS_DEPT"
)

// Draft 申请草案（注意：不含 canViewBusinessValues —— D7）。
type Draft struct {
	DataUseGroups []authz.GroupScopeGrant `json:"dataUseGroups,omitempty"`
	Modules       []string                `json:"modules"`
	Dimensions    []DimGrant              `json:"dimensions"`
	MaxLevel      authz.Level             `json:"maxLevel"`
}

// DimGrant 维度申请。
type DimGrant struct {
	Dim                string   `json:"dim"`
	Values             []string `json:"values"`
	IncludeDescendants bool     `json:"includeDescendants"`
}

// ApprovalStep 审批步骤。
//
// ★ JSON tag 必须与 contracts/permission-request.ts 的 ApprovalStep **逐字一致**。
//   这些结构体会被序列化进 fact_permission_request.approvals 这个 jsonb 列，
//   而前端按契约的 camelCase 读取。若去掉 tag，Go 会输出 PascalCase，
//   前端拿不到 approver/action ⇒ 待办列表空白，且**不会报错**（字段就是 undefined）。
//   同样重要的是：0006 的 jsonb containment 检索也依赖这对键名。
type ApprovalStep struct {
	Approver string    `json:"approver"`
	Tier     string    `json:"tier"`
	Action   string    `json:"action"` // APPROVE | REJECT | RETURN | PENDING
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

// Request 申请单。
type Request struct {
	ID              string
	Applicant       string
	Draft           Draft
	Purpose         string
	RequestedExpiry *time.Time
	Status          Status
	Approvals       []ApprovalStep
	CCs             []chain.CcRecord
	CrossDept       bool
	CreatedAt       time.Time
	ResolvedAt      *time.Time
}

// Validation 预校验结果。
type Validation struct {
	OK         bool
	OutOfScope []string
	Duplicates []string
	Route      []ApprovalStep
	// CCs 与 Route **同一次**路由产出的抄送记录。
	// ★ 单列为字段而不是让 Submit 再调一次 Org.Route：Route 内含 time.Now()，
	//   两次调用的 NotifiedAt 会不同，且任何对路由的改动都可能在两次调用间漂移。
	CCs     []chain.CcRecord
	Message string
}

// Service 申请流服务。
type Service struct {
	Org      *chain.OrgDirectory
	Resolver *authz.Resolver
	// Entitlements 查询某账号当前授权（用于判重与上界校验）。
	Entitlements func(account string) (*authz.Entitlement, []*authz.Entitlement)
	CcPolicy     chain.CcPolicy
	CrossPolicy  chain.CrossDeptPolicy
	// Now 注入时钟。
	Now func() time.Time
}

// NewService 构造。
func NewService(org *chain.OrgDirectory, res *authz.Resolver,
	ents func(string) (*authz.Entitlement, []*authz.Entitlement)) *Service {
	return &Service{
		Org:          org,
		Resolver:     res,
		Entitlements: ents,
		CcPolicy:     chain.DefaultCcPolicy(),
		CrossPolicy:  chain.DefaultCrossDeptPolicy(),
		Now:          time.Now,
	}
}

// Validate 提交前校验：
//  1. IT 不可申请业务数值（D7，且草案本身无该字段，此处兜底）
//  2. 与已有权限判重
//  3. 跨部门 → BLOCKED_CROSS_DEPT（不可自助申请）
//  4. 路由审批人（须自身拥有被申请权限）
func (s *Service) Validate(r *Request, dataChain *chain.DataChain) (*Validation, error) {
	v := &Validation{OK: true}

	// 1) D7：IT 不得申请业务数据组
	myEnt, myGroups := s.Entitlements(r.Applicant)
	myView := s.Resolver.Resolve(myEnt, myGroups)
	if isIT(myEnt.BaseTemplate) {
		for _, g := range r.Draft.DataUseGroups {
			v.OutOfScope = append(v.OutOfScope, "dataUseGroup:"+g.Group)
		}
		// 兜底：IT 若试图申请 L3/L4（涉及业务数值）也拒绝
		if authz.MaxLevel(r.Draft.MaxLevel, authz.L1) == authz.L4 ||
			r.Draft.MaxLevel == authz.L3 {
			v.OutOfScope = append(v.OutOfScope, "level:"+string(r.Draft.MaxLevel))
		}
	}
	if len(v.OutOfScope) > 0 {
		v.OK = false
		v.Message = "IT 不可申请业务数值（D7）"
		return v, nil
	}

	// 2) 判重：已拥有的模块/维度
	haveMod := map[string]bool{}
	for _, m := range myView.Modules {
		haveMod[m] = true
	}
	for _, m := range r.Draft.Modules {
		if haveMod[m] {
			v.Duplicates = append(v.Duplicates, "module:"+m)
		}
	}

	// 3) 跨部门：不可自助申请
	if dataChain != nil && dataChain.CrossDept {
		r.CrossDept = true
		if err := chain.ValidateCrossDeptSelfService(dataChain, s.CrossPolicy); err != nil {
			v.OK = false
			v.Message = err.Error()
			return v, nil
		}
	}

	// 4) 路由：+1 审批 / +2 抄送（F9=A）；须自身拥有被申请权限
	//    BatchSize 由维度被授范围推导 —— 见 grantedAccountScope。
	ac, err := s.Org.Route(chain.RouteInput{
		Applicant:   r.Applicant,
		DraftLevel:  string(r.Draft.MaxLevel),
		DraftGroups: groupIDs(r.Draft.DataUseGroups),
		ExpiryDays:  expiryDaysFrom(r),
		BatchSize:   grantedAccountScope(r.Draft),
		CrossDept:   r.CrossDept,
	}, s.CcPolicy)
	if err != nil {
		return nil, err
	}
	r.CCs = ac.CcRecords

	// 5) 会签补判（F8）：L4 / 跨部门 / 长期 / 敏感组已在 Route 内判定；
	//    「批量授予 ≥ 阈值」的规模随 RouteInput.BatchSize 传入（见 grantedAccountScope），
	//    故此处无需再补判，只做一次自证：名单里若仍有 cosign 而未命中任一条件，
	//    说明 Route 与 CosignTrigger 已经漂移（有人在 Route 里私自塞了会签）。
	//    ★ 这条自证的价值：会签一旦被"多触发"，申请会永远卡在 COSIGN_PENDING，
	//    而表象只是"审批很久没动"，极难定位。
	for i := range ac.CcRecords {
		if ac.CcRecords[i].Mode != chain.CcCosign {
			continue
		}
		if ok, _ := chain.CosignTrigger(chain.RouteInput{
			Applicant:   r.Applicant,
			DraftLevel:  string(r.Draft.MaxLevel),
			DraftGroups: groupIDs(r.Draft.DataUseGroups),
			ExpiryDays:  expiryDaysFrom(r),
			BatchSize:   grantedAccountScope(r.Draft),
			CrossDept:   r.CrossDept,
		}, s.CcPolicy); !ok {
			return nil, errors.New("req: 会签标记与触发条件不一致（Route 与会签判定漂移）")
		}
	}

	approver := ac.Approver
	// 校验审批人自身权限是否覆盖被申请内容；不足则沿链上溯找权限超集者
	approver = s.escalateToSuperset(approver, r.Draft)
	apEnt, apGroups := s.Entitlements(approver)
	apView := s.Resolver.Resolve(apEnt, apGroups)
	if !covers(apView, r.Draft) {
		return nil, errors.New("req: no approver with sufficient permissions found (escalate to T1)")
	}
	apNode, _ := s.Org.Get(approver)

	v.Route = []ApprovalStep{{
		Approver: approver,
		Tier:     apNode.Tier,
		Action:   "PENDING",
	}}
	v.CCs = ac.CcRecords
	// CCs 由本函数连同 Route 一起返回（同源），供 Submit 直接落单。
	return v, nil
}

// expiryDaysFrom 由申请单推导有效期天数（F8「有效期 > 90 天 ⇒ 会签」）。
//
// ★ 为什么必须有：此前 RouteInput 从未填 ExpiryDays，于是
//   「有效期>90天 ⇒ 会签」这条触发条件在**生产路径上永远不成立** ——
//   闸门单测直接构造 RouteInput{ExpiryDays: 91} 能过，真实提交却恒为 0。
//   这是"闸门测的是入参、不是链路"的典型空转。
//
// 语义：
//   - RequestedExpiry 为空 ⇒ 0（= 非长期授权，不触发长期会签）
//   - 已过期的时间点 ⇒ 0（不因一个无效时间点升级为会签；
//     有效性由提交校验负责，不在这里兜底）
//   - 向上取整到天：23 小时也算 1 天，绝不向下取整 ——
//     向下取整会让"90 天零几小时"仍被当成 90 天而漏掉会签。
func expiryDaysFrom(r *Request) int {
	if r == nil || r.RequestedExpiry == nil {
		return 0
	}
	d := r.RequestedExpiry.Sub(r.CreatedAt)
	if d <= 0 {
		return 0
	}
	const day = 24 * time.Hour
	days := int(d / day)
	if d%day != 0 {
		days++
	}
	return days
}

// grantedAccountScope 估计本次申请的**被授账号规模**（F8「批量授予 ≥10 ⇒ 会签」）。
//
// ★ 为什么必须有：此前 RouteInput.BatchSize 从未被填，恒为 0 ⇒
//   「批量≥10 ⇒ 会签」在生产路径上永远不成立。闸门用裸 RouteInput 测得到，
//   真实提交永远不触发。
//
// 口径：按维度的**被授范围基数**求和。
//   - IncludeDescendants=true ⇒ 至少按 2 估（该节点 + 其下至少一个后代）。
//     这是保守**高估**还是低估？—— 高估：宁可多触发会签，不可漏。
//     「批量授权」的风险来自覆盖面，宁可误报一次会签也不能漏一次。
//   - 无维度项 ⇒ 0（不是"全量"）：全量范围由 MaxLevel/模块表达，
//     不在本函数的职责内，凭空记一个巨大数字会让所有申请都被判批量。
func grantedAccountScope(d Draft) int {
	n := 0
	for _, dg := range d.Dimensions {
		if dg.IncludeDescendants {
			if len(dg.Values) == 0 {
				n += 2 // 整个维度下所有取值 + 其后代 ⇒ 至少 2
				continue
			}
			n += len(dg.Values) * 2
			continue
		}
		n += len(dg.Values)
	}
	return n
}

// escalateToSuperset 沿主属链向上找第一个权限覆盖草案的审批人；无则 T1。
func (s *Service) escalateToSuperset(from string, d Draft) string {
	cur := from
	for cur != "" {
		e, gs := s.Entitlements(cur)
		if covers(s.Resolver.Resolve(e, gs), d) {
			return cur
		}
		n, ok := s.Org.Get(cur)
		if !ok || n.Supervisor == "" {
			break
		}
		cur = n.Supervisor
	}
	// T1 兜底：返回申请人的最高层
	if top, err := s.Org.TopAccount(from); err == nil {
		return top
	}
	return from
}

// Submit 提交申请并置状态。
func (s *Service) Submit(r *Request, dataChain *chain.DataChain) (*Validation, error) {
	v, err := s.Validate(r, dataChain)
	if err != nil {
		return nil, err
	}
	if !v.OK {
		if r.CrossDept {
			r.Status = StatusBlockedCrossDep
		}
		return v, nil
	}
	r.Approvals = v.Route
	// CCs 与 Route 同源（v.CCs 由 Validate 内那次唯一的 Route 产出）。
	// ★ 若这里改用另一次 Org.Route 的结果，就会出现"审批人来自一次路由、
	//   抄送来自另一次路由"的错位 —— Route 内含 time.Now()，两次结果不保证一致。
	r.CCs = v.CCs
	// ★ 抄送模式必须在**确定状态之前**落定到 r.CCs：状态由"是否存在会签抄送"决定。
	//   曾经此处先置 APPROVING 再检查模式，若检查的是另一次路由结果，
	//   就会出现"状态 = APPROVING 但名单里全是 notify"——会签被静默跳过，
	//   且没有任何断言会红（G7「会签触发」测的是 Route 的输出，不是申请单的状态）。
	r.Status = StatusApproving
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign {
			r.Status = StatusCosignPending
			break
		}
	}
	return v, nil
}

// Approve 审批通过：写入授权（由调用方落库）并置 APPROVED。
//
// ★ 会签纪律（F8）：只要存在 **cosign 模式且尚未决策** 的抄送人，
// 就**不得**通过 —— 否则「会签」等同虚设（等于默认知会）。
// 曾经的实现只在 `cc.Vetoed` 为真时拒绝，看起来对，实则漏了
// 「会签人还没表态」这一最常见的情形：那时 vetoed=false，于是一路放行。
func (s *Service) Approve(r *Request, approver, reason string) error {
	if r.Status != StatusApproving && r.Status != StatusCosignPending {
		return fmt.Errorf("req: cannot approve in status %s", r.Status)
	}
	// 会签未决 ⇒ 不得通过；已否决 ⇒ 直接驳回。
	for _, cc := range r.CCs {
		if cc.Mode != chain.CcCosign {
			continue
		}
		if cc.Vetoed {
			r.Status = StatusRejected
			now := s.now()
			r.ResolvedAt = &now
			return errors.New("req: vetoed by cosigner")
		}
		if cc.DecidedAt == nil {
			r.Status = StatusCosignPending
			return fmt.Errorf("req: cosign pending for %s —— 会签人未表态前不得通过（F8）", cc.Cc)
		}
	}
	now := s.now()
	for i := range r.Approvals {
		if r.Approvals[i].Approver == approver {
			r.Approvals[i].Action = "APPROVE"
			r.Approvals[i].Reason = reason
			r.Approvals[i].At = now
		}
	}
	r.Status = StatusApproved
	r.ResolvedAt = &now
	return nil
}

// Cosign 会签人表态：approve=true 记决策；approve=false 即否决（驳回）。
//
// 仅 cosign 模式可会签 —— notify（知会）模式的抄送人**无决定权**，
// 传入会返回错误，避免「知会」被误当成「会签」使用。
func (s *Service) Cosign(r *Request, ccAccount string, approve bool, reason string) error {
	now := s.now()
	found := false
	for i := range r.CCs {
		cc := &r.CCs[i]
		if cc.Cc != ccAccount {
			continue
		}
		found = true
		if cc.Mode != chain.CcCosign {
			return fmt.Errorf("req: %s 是知会（notify）而非会签，无决定权（F8）", ccAccount)
		}
		cc.DecidedAt = &now
		cc.Vetoed = !approve
		if !approve {
			r.Status = StatusRejected
			r.ResolvedAt = &now
			_ = reason
			return nil
		}
	}
	if !found {
		return fmt.Errorf("req: %s 不在本申请的抄送名单中", ccAccount)
	}
	// 全部会签人已表态 ⇒ 由 COSIGN_PENDING 回到 APPROVING，等待 +1 最终批准
	if r.Status == StatusCosignPending {
		if s.AllCosignsDecided(r) {
			r.Status = StatusApproving
		}
	}
	return nil
}

// AllCosignsDecided 判断所有 cosign 抄送人是否均已表态。
func (s *Service) AllCosignsDecided(r *Request) bool {
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign && cc.DecidedAt == nil {
			return false
		}
	}
	return true
}

// Withdraw 申请人撤回（仅在其自身申请且尚未终结时允许）。
func (s *Service) Withdraw(r *Request, by string) error {
	if by != r.Applicant {
		return errors.New("req: 只有申请人本人可以撤回")
	}
	switch r.Status {
	case StatusApproved, StatusRejected, StatusWithdrawn:
		return fmt.Errorf("req: 已终结（%s）的申请不可撤回", r.Status)
	}
	now := s.now()
	r.Status = StatusWithdrawn
	r.ResolvedAt = &now
	return nil
}

// Reject 审批人拒绝。
func (s *Service) Reject(r *Request, approver, reason string) error {
	if r.Status != StatusApproving && r.Status != StatusCosignPending {
		return fmt.Errorf("req: cannot reject in status %s", r.Status)
	}
	now := s.now()
	for i := range r.Approvals {
		if r.Approvals[i].Approver == approver {
			r.Approvals[i].Action = "REJECT"
			r.Approvals[i].Reason = reason
			r.Approvals[i].At = now
		}
	}
	r.Status = StatusRejected
	r.ResolvedAt = &now
	return nil
}

// Reclaim 到期回收判定：返回 true 表示该申请携带的授权应被回收。
//
// 时间盒纪律：长期申请（RequestedExpiry=nil）不回收；到期即回收。
// 调用方据此写回 Entitlement 并落审计。
func (s *Service) Reclaim(r *Request) bool {
	if r == nil || r.Status != StatusApproved {
		return false
	}
	if r.RequestedExpiry == nil {
		return false
	}
	return !s.now().Before(*r.RequestedExpiry)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ───────────────────────────── 辅助 ─────────────────────────────

func groupIDs(gs []authz.GroupScopeGrant) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Group)
	}
	return out
}

// covers 判断 view 是否覆盖草案（模块 ⊆、密级 ≥、维度 ⊆）。
func covers(view *authz.EntitlementView, d Draft) bool {
	if view == nil {
		return false
	}
	if levelRank(d.MaxLevel) > levelRank(view.MaxLevel) {
		return false
	}
	mods := map[string]bool{}
	for _, m := range view.Modules {
		mods[m] = true
	}
	for _, m := range d.Modules {
		if !mods[m] {
			return false
		}
	}
	for _, dg := range d.Dimensions {
		vals, ok := view.Dimensions[dg.Dim]
		if !ok {
			return false
		}
		allowed := map[string]bool{}
		for _, v := range vals {
			allowed[v] = true
		}
		if allowed["*"] {
			continue
		}
		for _, v := range dg.Values {
			if !allowed[v] {
				return false
			}
		}
	}
	return true
}

func levelRank(l authz.Level) int {
	switch l {
	case authz.L1:
		return 1
	case authz.L2:
		return 2
	case authz.L3:
		return 3
	case authz.L4:
		return 4
	}
	return 0
}

// isIT 判断基础模板是否为 IT 模板（D7 的判定入口）。
//
// ★ 必须按「层级段」比较，不能用前缀裸比：`tpl.item` 的前 6 字符同样是
// `tpl.it`，裸前缀会把「物料」模板误判成 IT，从而错误地禁掉一整批人。
// 规则：恰好等于 `tpl.it`，或以 `tpl.it.` 开头（允许 tpl.it.v2 这类版本）。
func isIT(tpl string) bool {
	return tpl == "tpl.it" || strings.HasPrefix(tpl, "tpl.it.")
}
