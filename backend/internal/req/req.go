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
type ApprovalStep struct {
	Approver string
	Tier     string
	Action   string // APPROVE | REJECT | RETURN | PENDING
	Reason   string
	At       time.Time
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
	Message    string
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
	ac, err := s.Org.Route(chain.RouteInput{
		Applicant:   r.Applicant,
		DraftLevel:  string(r.Draft.MaxLevel),
		DraftGroups: groupIDs(r.Draft.DataUseGroups),
		CrossDept:   r.CrossDept,
	}, s.CcPolicy)
	if err != nil {
		return nil, err
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
	r.CCs = ac.CcRecords
	return v, nil
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
	r.Status = StatusApproving
	// 若非"知会"而是"会签"，则进入 COSIGN_PENDING
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign {
			r.Status = StatusCosignPending
			break
		}
	}
	return v, nil
}

// Approve 审批通过：写入授权（由调用方落库）并置 APPROVED。
func (s *Service) Approve(r *Request, approver, reason string) error {
	if r.Status != StatusApproving && r.Status != StatusCosignPending {
		return fmt.Errorf("req: cannot approve in status %s", r.Status)
	}
	// 会签未决前不得通过
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign && cc.Vetoed {
			r.Status = StatusRejected
			return errors.New("req: vetoed by cosigner")
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

func isIT(tpl string) bool {
	return tpl == "tpl.it" || (len(tpl) > 7 && tpl[:7] == "tpl.it")
}
