// Package chain —— 数据链与审批链解析（A2 / F9=A 单主属 / F10=C 混合）。
//
// 两条链路严格区分（docs/08 §3.3）：
//   - 数据链（DataChain）：一份数据归属谁、由谁负责 → 决定可见边界与「是否跨部门」
//   - 审批链（ApprovalChain）：一次申请由谁批、抄送给谁 → 「+1 审批 / +2 抄送」
//
// F9 = A 单主属（已拍板）：
//   - 每人**唯一** primaryDept 与**唯一** supervisor；审批链/数据链只走主属。
//   - **虚线汇报仅进抄送，不入审批**。
//   - 临时跨部门走**限期授权**，到期自动失效。
//
// F10 = C 混合（已拍板）：
//   - primaryDept 默认由组织架构**自动派生**；允许人工覆盖且**人工优先**；
//   - 人员离职自动回退为派生值。
package chain

import (
	"errors"
	"fmt"
	"time"
)

// ErrCycle 组织链存在环（配置错误，必须显式失败而非死循环）。
var ErrCycle = errors.New("org chain contains a cycle")

// DeptSource 部门来源（F10）。
type DeptSource string

const (
	Derived DeptSource = "DERIVED" // 组织架构自动派生（默认）
	Manual  DeptSource = "MANUAL"  // 人工覆盖（优先）
)

// OrgNode 组织节点（F9=A 单主属）。
type OrgNode struct {
	Account string
	// 主属上级（唯一）；顶层为 ""
	Supervisor string
	Tier       string // T1..T6
	// 主属部门（唯一）
	PrimaryDept string
	DeptSource  DeptSource // F10：DERIVED / MANUAL，人工优先
	// 虚线汇报上级（仅抄送，不入审批）
	DottedLineSupervisors []string
	CanApprove            bool
	Active                bool
}

// OrgDirectory 组织目录（内存实现；生产由 DB 提供）。
type OrgDirectory struct {
	Nodes     map[string]*OrgNode
	TierOrder map[string]int
}

// NewOrgDirectory 构造组织目录。
func NewOrgDirectory() *OrgDirectory {
	return &OrgDirectory{
		Nodes: map[string]*OrgNode{},
		// T1 最高 = 1
		TierOrder: map[string]int{"T1": 1, "T2": 2, "T3": 3, "T4": 4, "T5": 5, "T6": 6},
	}
}

// Put 写入节点。
func (d *OrgDirectory) Put(n *OrgNode) { d.Nodes[n.Account] = n }

// Get 读取节点。
func (d *OrgDirectory) Get(account string) (*OrgNode, bool) {
	n, ok := d.Nodes[account]
	return n, ok
}

// ResolveDept 解析某账号的主属部门（F10 混合口径）。
//
// 口径：人工覆盖（MANUAL）优先；离职或无覆盖则回退派生值。
func (d *OrgDirectory) ResolveDept(account string) (string, DeptSource, error) {
	n, ok := d.Nodes[account]
	if !ok {
		return "", "", fmt.Errorf("org: unknown account %q", account)
	}
	if n.DeptSource == Manual && n.PrimaryDept != "" {
		return n.PrimaryDept, Manual, nil
	}
	// 派生：取自身 primary_dept（由组织架构同步写入）
	if n.PrimaryDept == "" {
		return "", "", fmt.Errorf("org: account %q has empty primaryDept", account)
	}
	return n.PrimaryDept, Derived, nil
}

// SupervisorChain 返回 [self, supervisor, …, 顶层]；检测环。
func (d *OrgDirectory) SupervisorChain(account string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	cur := account
	for cur != "" {
		if seen[cur] {
			return nil, fmt.Errorf("%w at %q", ErrCycle, cur)
		}
		seen[cur] = true
		n, ok := d.Nodes[cur]
		if !ok {
			return nil, fmt.Errorf("org: unknown account %q in chain", cur)
		}
		out = append(out, cur)
		cur = n.Supervisor
	}
	return out, nil
}

// TopAccount 返回该账号链上的最高层（T1 兜底）。
func (d *OrgDirectory) TopAccount(account string) (string, error) {
	c, err := d.SupervisorChain(account)
	if err != nil {
		return "", err
	}
	return c[len(c)-1], nil
}

// ───────────────────────────── 数据链 ─────────────────────────────

// DataChainNode 数据责任节点。
type DataChainNode struct {
	Scope       string // channel:TK-TH / store:S123 / brand:KONVY
	Owner       string
	OwnerChain  []string
	Dept        string
}

// DataChain 数据链。
type DataChain struct {
	Nodes     []DataChainNode
	Depts     []string
	CrossDept bool
}

// ResolveDataChain 解析一组数据范围的责任链与部门归属。
func (d *OrgDirectory) ResolveDataChain(owners map[string]string) (*DataChain, error) {
	dc := &DataChain{}
	deptSet := map[string]bool{}
	for scope, owner := range owners {
		if _, ok := d.Nodes[owner]; !ok {
			return nil, fmt.Errorf("org: unknown owner %q for scope %q", owner, scope)
		}
		chain, err := d.SupervisorChain(owner)
		if err != nil {
			return nil, err
		}
		dept, _, err := d.ResolveDept(owner)
		if err != nil {
			return nil, err
		}
		dc.Nodes = append(dc.Nodes, DataChainNode{
			Scope: scope, Owner: owner, OwnerChain: chain, Dept: dept,
		})
		deptSet[dept] = true
	}
	for dp := range deptSet {
		dc.Depts = append(dc.Depts, dp)
	}
	dc.CrossDept = len(dc.Depts) >= 2 // A1 阈值 = 2
	return dc, nil
}

// ───────────────────────────── 审批链 ─────────────────────────────

// CcMode 抄送模式（F8）。
type CcMode string

const (
	CcNotify CcMode = "notify" // 知会（默认，无决定权）
	CcCosign CcMode = "cosign" // 会签（有否决权）
)

// CcRecord 抄送记录。
//
// ★ JSON tag 必须与 contracts/permission-request.ts 的 CcRecord **逐字一致**：
//   本结构体会序列化进 fact_permission_request.ccs 这个 jsonb 列，
//   0006 的 v_cosign_pending 视图直接按 `cc->>'decidedAt'` 取键判定
//   「会签人是否已表态」。键名一旦被改成 PascalCase（如去掉 tag），
//   视图里的判定会永远拿到 NULL ⇒ 把「已表态」全判成「未表态」，
//   待办列表虚增；反向则更糟 —— 若判定写反就会漏掉真正卡住的单。
type CcRecord struct {
	Cc         string    `json:"cc"`
	Reason     string    `json:"reason"` // "+2" / "+1 的直属上级" / "虚线汇报"
	Mode       CcMode    `json:"mode"`
	NotifiedAt time.Time `json:"notifiedAt"`
	// 会签模式下：是否否决（F8）
	Vetoed bool `json:"vetoed"`
	// 会签模式下：表态时间。
	//
	// ★ 为什么必须有这个字段：只有 `Vetoed` 时无法区分
	//   「会签人**尚未**表态」与「会签人**已同意**」—— 两者 Vetoed 都是 false。
	//   而「尚未表态」时**不得放行**（否则会签形同虚设）。nil = 未表态。
	//
	//   omitempty 必须**去掉**：nil 时要显式序列化成 null，
	//   让「未表态」这个语义位在 jsonb 里可见；若省略该键，
	//   读取方无法区分「未表态」与「旧数据没这个字段」。
	DecidedAt *time.Time `json:"decidedAt"`
}

// ApprovalChain 审批链（+1 审批 / +2 抄送）。
type ApprovalChain struct {
	Applicant      string
	Approver       string // +1：主属直接上级（唯一决定权）
	ApproverTier   string
	CcList         []string
	DottedLineCc   []string // F9：虚线汇报上级，仅抄送
	CcRecords      []CcRecord
	CcRule         string // PLUS_TWO | PLUS_ONE_SUPERVISOR | NONE
	Fallback       bool   // 申请人已是顶层 ⇒ +1 = T1
	CrossDept      bool   // 跨部门 ⇒ approver 必须为 T1
}

// CrossDeptPolicy 跨部门规则（A1）。
type CrossDeptPolicy struct {
	DeptThreshold      int
	Allowed            bool
	RequireTopApproval bool
	SelfServiceable    bool
}

// DefaultCrossDeptPolicy 默认跨部门策略：
// 允许覆盖，但必须 T1 批准，且不可自助申请。
func DefaultCrossDeptPolicy() CrossDeptPolicy {
	return CrossDeptPolicy{
		DeptThreshold:      2,
		Allowed:            true,
		RequireTopApproval: true,
		SelfServiceable:    false,
	}
}

// CcPolicy 抄送策略（F8）。
type CcPolicy struct {
	DefaultMode  CcMode
	ExpiryDaysGt int      // 有效期 > 该天数 ⇒ 会签
	CosignGroups []string // 含这些勾选组 ⇒ 会签
	BatchGte     int      // 批量授予 ≥ 该数 ⇒ 会签
}

// DefaultCcPolicy 默认：知会；高风险升级会签。
func DefaultCcPolicy() CcPolicy {
	return CcPolicy{
		DefaultMode:  CcNotify,
		ExpiryDaysGt: 90,
		CosignGroups: []string{"grp.roi", "grp.cost_profit"},
		BatchGte:     10,
	}
}

// RouteInput 路由输入。
type RouteInput struct {
	Applicant     string
	DraftLevel    string
	DraftGroups   []string
	ExpiryDays    int
	BatchSize     int
	CrossDept     bool
}

// Route 计算审批链（F9=A 单主属 + F8 会签升级）。
func (d *OrgDirectory) Route(in RouteInput, ccp CcPolicy) (*ApprovalChain, error) {
	me, ok := d.Nodes[in.Applicant]
	if !ok {
		return nil, fmt.Errorf("chain: unknown applicant %q", in.Applicant)
	}
	ac := &ApprovalChain{Applicant: in.Applicant, CrossDept: in.CrossDept}

	// +1 = 主属直接上级；不存在 ⇒ T1 兜底
	approver := me.Supervisor
	if approver == "" {
		top, err := d.TopAccount(in.Applicant)
		if err != nil {
			return nil, err
		}
		approver = top
		ac.Fallback = true
	}
	// 跨部门 ⇒ approver 强制为 T1
	if in.CrossDept {
		top, err := d.TopAccount(in.Applicant)
		if err != nil {
			return nil, err
		}
		approver = top
	}
	apNode, ok := d.Nodes[approver]
	if !ok {
		return nil, fmt.Errorf("chain: approver %q not found", approver)
	}
	ac.Approver = approver
	ac.ApproverTier = apNode.Tier

	// 抄送：+2（approver 的上级）；不存在 ⇒ +1 的直属上级；再无 ⇒ NONE
	supOfApprover := apNode.Supervisor
	if supOfApprover != "" {
		ac.CcList = append(ac.CcList, supOfApprover)
		ac.CcRule = "PLUS_TWO"
	} else if me.Supervisor != "" && me.Supervisor != approver {
		ac.CcList = append(ac.CcList, me.Supervisor)
		ac.CcRule = "PLUS_ONE_SUPERVISOR"
	} else {
		ac.CcRule = "NONE"
	}

	// F9：虚线汇报上级仅抄送
	for _, dl := range me.DottedLineSupervisors {
		if dl == "" || dl == ac.Approver {
			continue
		}
		ac.DottedLineCc = append(ac.DottedLineCc, dl)
	}

	// 决定抄送模式（F8）
	mode := ccp.DefaultMode
	reason := "+2"
	if in.DraftLevel == "L4" {
		mode = CcCosign
		reason = "L4"
	} else if in.CrossDept {
		mode = CcCosign
		reason = "跨部门"
	} else if in.ExpiryDays > ccp.ExpiryDaysGt {
		mode = CcCosign
		reason = "有效期>90天"
	} else if intersects(in.DraftGroups, ccp.CosignGroups) {
		mode = CcCosign
		reason = "含投资/成本利润组"
	} else if in.BatchSize >= ccp.BatchGte {
		mode = CcCosign
		reason = "批量≥10"
	}

	now := time.Now()
	for _, cc := range ac.CcList {
		ac.CcRecords = append(ac.CcRecords, CcRecord{
			Cc: cc, Reason: reason, Mode: mode, NotifiedAt: now,
		})
	}
	for _, cc := range ac.DottedLineCc {
		ac.CcRecords = append(ac.CcRecords, CcRecord{
			Cc: cc, Reason: "虚线汇报", Mode: CcNotify, NotifiedAt: now,
		})
	}
	return ac, nil
}

func intersects(a, b []string) bool {
	set := map[string]bool{}
	for _, x := range a {
		set[x] = true
	}
	for _, y := range b {
		if set[y] {
			return true
		}
	}
	return false
}

// ValidateCrossDeptSelfService：跨部门不可自助申请（A1 硬约束）。
func ValidateCrossDeptSelfService(dc *DataChain, policy CrossDeptPolicy) error {
	if dc != nil && dc.CrossDept {
		if !policy.Allowed {
			return errors.New("chain: cross-dept not allowed")
		}
		if !policy.SelfServiceable {
			return errors.New("BLOCKED_CROSS_DEPT: 跨部门需 T1 直接授予，不可自助申请")
		}
	}
	return nil
}
