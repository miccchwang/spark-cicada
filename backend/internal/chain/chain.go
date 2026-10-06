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

// EmptyCcBehavior 「+2 不存在」时的行为（F8 / docs/11 配置项 `empty_cc_behavior`）。
type EmptyCcBehavior string

const (
	EmptyCcNotify EmptyCcBehavior = "notify" // 降级为 +1 的直属上级知会（默认）
	EmptyCcSkip   EmptyCcBehavior = "skip"   // 无可抄送人 ⇒ 不产生抄送记录
)

const (
	// CosignReasonL4 / 跨部门 / 长期 / 敏感组 / 批量 —— 会签触发原因，
	// 会写进 CcRecord.Reason 供审计追溯"为什么这单要会签"。
	CosignReasonL4        = "L4"
	CosignReasonCrossDept = "跨部门"
	CosignReasonLongTerm  = "有效期>90天"
	CosignReasonSensitive = "含投资/成本利润组"
	CosignReasonBatch     = "批量≥10"

	// CcRulePlusTwo / PlusOneSupervisor / None —— 抄送层级规则的机器可读取值。
	//
	// ★ 必须是常量而不是散落各处的字符串字面量：gate.CheckPlusOnePlusTwo
	//   按这些取值判定"抄送层级是否符合 F9"，拼错一个字母就会让断言恒真。
	//   历史教训：本仓已有多个闸门因"字面量在两处各写一遍"而长期空转。
	CcRulePlusTwo          = "PLUS_TWO"
	CcRulePlusOneSupervisor = "PLUS_ONE_SUPERVISOR"
	CcRuleNone             = "NONE"
)

// CcPolicy 抄送策略（F8）。
type CcPolicy struct {
	DefaultMode  CcMode
	ExpiryDaysGt int      // 有效期 > 该天数 ⇒ 会签
	CosignGroups []string // 含这些勾选组 ⇒ 会签
	BatchGte     int      // 批量授予 ≥ 该数 ⇒ 会签
	// EmptyCcBehavior：+2（审批人的上级）不存在时的降级行为。
	// 默认 notify（降为 +1 的直属上级知会）；skip 表示无可抄送人时不留记录。
	EmptyCcBehavior EmptyCcBehavior
}

// DefaultCcPolicy 默认：知会；高风险升级会签。
func DefaultCcPolicy() CcPolicy {
	return CcPolicy{
		DefaultMode:     CcNotify,
		ExpiryDaysGt:    90,
		CosignGroups:    []string{"grp.roi", "grp.cost_profit"},
		BatchGte:        10,
		EmptyCcBehavior: EmptyCcNotify,
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

// CosignTrigger 评估会签触发（F8），返回 (是否触发, 原因)。
//
// 触发条件（任一命中）：
//   - 密级 = L4（核心数据变更）
//   - 跨部门
//   - 有效期 > ExpiryDaysGt
//   - 涉及 CosignGroups（grp.roi / grp.cost_profit）
//   - 批量授予 ≥ BatchGte
//
// ★ 单列成函数而非内联在 Route 里：闸门（gate.CheckCosignTrigger）要能
//   对"触发判定"本身单独断言，而不是只能看 Route 的整体输出。
//   内联时一旦有人改动 Route 的顺序或删掉某条分支，没有任何断言会红。
func CosignTrigger(in RouteInput, ccp CcPolicy) (bool, string) {
	switch {
	case in.DraftLevel == "L4":
		return true, CosignReasonL4
	case in.CrossDept:
		return true, CosignReasonCrossDept
	case ccp.ExpiryDaysGt > 0 && in.ExpiryDays > ccp.ExpiryDaysGt:
		return true, CosignReasonLongTerm
	case intersects(in.DraftGroups, ccp.CosignGroups):
		return true, CosignReasonSensitive
	case ccp.BatchGte > 0 && in.BatchSize >= ccp.BatchGte:
		return true, CosignReasonBatch
	}
	return false, ""
}

// rejectCosign 在不触发会签时拒绝已被标记为会签的抄送记录（F8）。
//
// ★ 必须显式清回 notify 而不是"不处理"：一旦某条 cc 是 cosign，
//   req.Approve 会因为「会签人未表态」而永远卡住该单 —— 而它本来
//   就不该是会签。这属于 fail-closed 的收紧方向（不会误开通权限），
//   但会让申请永远走不完，因此必须当场纠正而不是静默放行。
func rejectCosign(recs []CcRecord) {
	for i := range recs {
		if recs[i].Mode == CcCosign {
			recs[i].Mode = CcNotify
			recs[i].Reason = "降级为知会(未命中会签条件)"
		}
	}
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

	// 抄送：+2（approver 的上级）；不存在 ⇒ 按 empty_cc_behavior 降级。
	//
	// ★ 两条硬约束（F8 / F9），历史实现曾在此处静默出错：
	//   1. 降级目标**必须跳过 approver 本人** —— 否则 "+2 抄送" 会退化成
	//      "抄送给审批人自己"，等于既无制衡也无知会，而 CcRule 还写着
	//      PLUS_ONE_SUPERVISOR，看上去完全正常。
	//   2. 抄送人**不得**与虚线汇报重复 —— 同一人在名单里出现两次会让
	//      待办列表计数虚增，且会签时"两个人"其实是同一个人。
	supOfApprover := apNode.Supervisor
	switch {
	case supOfApprover != "":
		ac.CcList = append(ac.CcList, supOfApprover)
		ac.CcRule = CcRulePlusTwo
	case ccp.EmptyCcBehavior == EmptyCcSkip:
		ac.CcRule = CcRuleNone
	case me.Supervisor != "" && me.Supervisor != approver:
		ac.CcList = append(ac.CcList, me.Supervisor)
		ac.CcRule = CcRulePlusOneSupervisor
	default:
		ac.CcRule = CcRuleNone
	}

	// F9：虚线汇报上级仅抄送。
	// 去重纪律：与 +2/降级抄送人重复的、以及等于审批人的，一律不进名单。
	seen := map[string]bool{ac.Approver: true}
	for _, existing := range ac.CcList {
		seen[existing] = true
	}
	for _, dl := range me.DottedLineSupervisors {
		if dl == "" || seen[dl] {
			continue
		}
		seen[dl] = true
		ac.DottedLineCc = append(ac.DottedLineCc, dl)
	}

	// 决定抄送模式（F8）
	mode, reason := CcNotify, ""
	if trigger, why := CosignTrigger(in, ccp); trigger {
		mode, reason = CcCosign, why
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

	// 收口：不触发会签时，名单里不得残留任何 cosign 记录。
	if mode != CcCosign {
		rejectCosign(ac.CcRecords)
	}
	return ac, nil
}

// SetCosignMode 就地覆盖**指定抄送人**的抄送模式（F8）。
//
// 用途：路由发生在提交时，而 RouteInput 无法预知全部触发条件
// （最典型的是「批量授予 ≥10 个账号」—— 被授范围来自 Draft.Dimensions，
// 而 Route 只拿到 DraftGroups）。因此调用方在拿到完整草案后，
// 需要能在**不改动抄送名单**的前提下补一次会签评估。
//
// 语义（严格）：
//   - 只改 mode/reason，**绝不增删抄送人** —— 抄送名单由 Route 独占决定，
//     事后追加抄送人会让"为什么这个人被抄送"失去唯一解释。
//   - account 只命中精确账号名；不匹配时返回 false（不报错），
//     因为"该人不在名单"是调用方的正常分叉而非异常。
//   - 升级为 cosign 时才重写 Reason；降级不改 Reason ——
//     Reason 记录的事由会进 jsonb 与审计，把它改成"降级"等于抹掉
//     当初的触发依据，事后无法复盘"这单为什么曾被判高风险"。
func SetCosignMode(recs []CcRecord, account string, cosign bool, reason string) bool {
	for i := range recs {
		if recs[i].Cc != account {
			continue
		}
		if recs[i].Mode == CcCosign && !cosign {
			return true // 已是最严模式，不降级
		}
		if cosign {
			recs[i].Mode = CcCosign
			if reason != "" {
				recs[i].Reason = reason
			}
		}
		return true
	}
	return false
}

// AnyCosign 名单中是否存在会签记录。
func AnyCosign(recs []CcRecord) bool {
	for i := range recs {
		if recs[i].Mode == CcCosign {
			return true
		}
	}
	return false
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
