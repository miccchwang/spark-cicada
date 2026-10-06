// failover.go —— 主备切换（F13 = B 半自动）与 180 天回滚（B2）。
//
// docs/04 §6.6 的两条硬红线，此前在实现侧**恒真**：
//
//	「切换必先 fencing」—— 没有切换路径 ⇒ 从不跳过 fencing；
//	「回滚不动审计」  —— 没有回滚路径 ⇒ 审计从不被回滚。
//
// 本文件把两条链路真正接上，并遵循 F8 轮的教训：
// **fencing 三项从站点真实状态推导，不接受调用方声明**。
// 若 fencing 由调用方传布尔值，闸门测的就还是入参 —— 生产路径「声明 true」
// 即可绕过真正的隔离动作，「切换必先 fencing」会再次退化为恒真。
package dr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/audit"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ───────────────────────────── 站点与集群（B1） ─────────────────────────────

// SiteRole 站点角色。
type SiteRole string

const (
	// RolePrimary 主库（唯一可写）。
	RolePrimary SiteRole = "PRIMARY"
	// RoleStandby 备库（只读，地域内异步同步）。
	RoleStandby SiteRole = "STANDBY"
)

// Site 一个地域内的一套站点（每地域：阿里云主 + AWS 备）。
type Site struct {
	Region Region
	Role   SiteRole
	// WriteBlocked 写通道是否已隔离 —— fencing 的第一项，**只由 Fence 置位**。
	WriteBlocked bool
	// ReadOnly 是否只读降级。
	ReadOnly bool
}

// Cluster 一个地域内的主备两站点。
//
// ★ 地域之间**互不接管**（docs/04 §6.6）：新加坡与美国各自一套主备，
// 因此 Cluster 天然按地域隔离，Promote 只允许操作本地域。
type Cluster struct {
	Region  Region
	Primary *Site
	Standby *Site
}

// Witness 地域内独立仲裁者（避免「双主都以为自己主」）。
type Witness interface {
	Acquired(region Region) bool
}

// LSNLedger 版本对账账本（切换后按 LSN/提交序号对账）。
type LSNLedger interface {
	Reconciled(region Region) bool
}

// ───────────────────────────── 半自动探测（只告警） ─────────────────────────────

// HealthFailureThreshold 连续健康探针失败次数阈值。
const HealthFailureThreshold = 3

// Probe 主库健康探测输入。
type Probe struct {
	Region            Region
	HealthFailures    int
	ReplicationLagSec int
	RegionProbeOK     bool
}

// Alert 一条「建议切换」告警。
type Alert struct {
	Region   Region
	Severity string
	Reasons  []string
	At       time.Time
}

// EvaluateProbe 判定是否需要「建议切换」。
//
// ★ 半自动纪律（F13 = B）：本函数**只产出告警，绝不触发切换**。
// 切换必须由 T1 显式调用 Promote。若此处顺手把备库提上来，那就是
// 「自动切」—— 用户明确拍板的是 B 半自动（不自动切、不自动切回）。
func EvaluateProbe(p Probe, lagThresholdSec int, now time.Time) (Alert, bool) {
	var reasons []string
	if p.HealthFailures >= HealthFailureThreshold {
		reasons = append(reasons, fmt.Sprintf("主库健康探针连续失败 %d 次", p.HealthFailures))
	}
	if lagThresholdSec > 0 && p.ReplicationLagSec > lagThresholdSec {
		reasons = append(reasons, fmt.Sprintf("复制延迟 %ds 超阈 %ds", p.ReplicationLagSec, lagThresholdSec))
	}
	if !p.RegionProbeOK {
		reasons = append(reasons, "地域探针异常")
	}
	if len(reasons) == 0 {
		return Alert{}, false
	}
	return Alert{Region: p.Region, Severity: "SUGGEST_FAILOVER", Reasons: reasons, At: now}, true
}

// ───────────────────────────── 切换（F13 = B 半自动） ─────────────────────────────

// 切换相关错误。
var (
	// ErrNotT1 高风险操作（切换 / 回滚）仅 T1 可发起。
	ErrNotT1 = errors.New("dr: 高风险操作仅 T1 可发起")
	// ErrFencingIncomplete fencing 未完成。
	ErrFencingIncomplete = errors.New("dr: fencing 未完成，禁止提升备库")
	// ErrRegionMismatch 地域不匹配（地域之间互不接管）。
	ErrRegionMismatch = errors.New("dr: 地域不匹配（地域之间互不接管）")
	// ErrAlreadyPrimary 目标已是主库。
	ErrAlreadyPrimary = errors.New("dr: 目标已是主库")
	// ErrNotPrimary 当前主站点角色异常。
	ErrNotPrimary = errors.New("dr: 当前主站点角色异常")
)

// FailoverRecord 一条切换审计（docs/04 §6.6：谁/何时/原因/前后状态）。
type FailoverRecord struct {
	Region     Region
	Operator   string
	Reason     string
	OldPrimary SiteRole
	NewPrimary SiteRole
	Fenced     bool
	At         time.Time
}

// FailoverAudit 切换审计回调。
type FailoverAudit func(ctx context.Context, rec FailoverRecord) error

// FencingFrom 从**真实站点状态**推导 fencing 三项。
//
// ★ 只认状态，不认声明 —— 见文件头注释。
func FencingFrom(c *Cluster, w Witness, l LSNLedger) gate.Fencing {
	f := gate.Fencing{}
	if c == nil {
		return f
	}
	if c.Primary != nil {
		f.OldPrimaryWriteBlocked = c.Primary.WriteBlocked
	}
	if w != nil {
		f.WitnessAcquired = w.Acquired(c.Region)
	}
	if l != nil {
		f.LSNReconciled = l.Reconciled(c.Region)
	}
	return f
}

// Fence 切断旧主写入通道（撤销写角色 / 置 read-only / 断开连接）。
//
// 必须在 Promote 之前调用；Promote 会从站点真实状态复核，未隔离即拒绝。
// 因此「跳过 Fence 直接 Promote」在生产路径上必然失败。
func Fence(c *Cluster) error {
	if c == nil || c.Primary == nil {
		return errors.New("dr: Fence 需要主站点")
	}
	if c.Primary.Role != RolePrimary {
		return fmt.Errorf("%w：当前主站点角色=%s", ErrNotPrimary, c.Primary.Role)
	}
	c.Primary.WriteBlocked = true
	c.Primary.ReadOnly = true
	return nil
}

// PromoteRequest T1 一键切换请求。
type PromoteRequest struct {
	Region   Region
	Operator string
	Tier     string
	Reason   string
}

// PromoteOutcome 切换结果（供审计与通知业务方）。
type PromoteOutcome struct {
	Region     Region
	Operator   string
	Reason     string
	OldPrimary SiteRole
	NewPrimary SiteRole
	Fencing    gate.Fencing
	PromotedAt time.Time
}

// Promote 半自动切换：T1 一键提升**本地域**备库为主。
//
// 纪律（顺序即优先级）：
//
//	1. **地域隔离**：只能切本地域（地域之间互不接管）。
//	2. **半自动**：仅 T1 可发起，且必须有操作人；
//	   自动探测（EvaluateProbe）只告警，**绝不**调用本函数。
//	3. **状态**：当前主站点须为主、目标须仍为备（幂等保护）。
//	4. **★ fencing 强制**：从站点真实状态推导，任一未满足即拒绝，
//	   且拒绝时集群**纹丝不动**（不能出现「旧主已被降级但备库没提升」的中间态）。
//	5. **禁止自动切回**：旧主降级为备且写通道**保持隔离**，需人工确认才重建复制。
//	6. **留痕**：谁 / 何时 / 原因 / 前后状态。
func Promote(ctx context.Context, c *Cluster, req PromoteRequest, w Witness, l LSNLedger,
	audit FailoverAudit, now time.Time) (PromoteOutcome, error) {

	if c == nil || c.Primary == nil || c.Standby == nil {
		return PromoteOutcome{}, errors.New("dr: Promote 需要完整的主备集群")
	}
	// 纪律 1：地域隔离
	if !c.Region.Valid() || req.Region != c.Region {
		return PromoteOutcome{}, fmt.Errorf("%w：请求=%q 集群=%q", ErrRegionMismatch, req.Region, c.Region)
	}
	// 纪律 2：半自动（仅 T1 + 必须有操作人）
	if strings.ToUpper(strings.TrimSpace(req.Tier)) != "T1" {
		return PromoteOutcome{}, fmt.Errorf("%w：tier=%q", ErrNotT1, req.Tier)
	}
	if strings.TrimSpace(req.Operator) == "" {
		return PromoteOutcome{}, errors.New("dr: 切换必须有操作人（追不到人的切换不放行）")
	}
	// 纪律 3：状态
	if c.Primary.Role != RolePrimary {
		return PromoteOutcome{}, fmt.Errorf("%w：主站点角色=%s", ErrNotPrimary, c.Primary.Role)
	}
	if c.Standby.Role == RolePrimary {
		return PromoteOutcome{}, ErrAlreadyPrimary
	}

	// 纪律 4：fencing（★ 从真实状态推导；拒绝时集群不动）
	f := FencingFrom(c, w, l)
	if v := gate.CheckFailoverFencing(f); len(v) > 0 {
		return PromoteOutcome{}, fmt.Errorf("%w：%s", ErrFencingIncomplete, strings.Join(v, "；"))
	}

	// 纪律 5：执行（旧主降级且写通道保持隔离）
	old := c.Primary.Role
	c.Primary.Role = RoleStandby
	c.Primary.WriteBlocked = true
	c.Primary.ReadOnly = true
	c.Standby.Role = RolePrimary
	c.Standby.WriteBlocked = false
	c.Standby.ReadOnly = false

	out := PromoteOutcome{
		Region: c.Region, Operator: req.Operator, Reason: req.Reason,
		OldPrimary: old, NewPrimary: RolePrimary, Fencing: f, PromotedAt: now,
	}
	// 纪律 6：留痕
	if audit != nil {
		_ = audit(ctx, FailoverRecord{
			Region: c.Region, Operator: req.Operator, Reason: req.Reason,
			OldPrimary: old, NewPrimary: RolePrimary, Fenced: true, At: now,
		})
	}
	return out, nil
}

// ───────────────────────────── 180 天回滚（B2） ─────────────────────────────

// RollbackPolicy 回滚策略（对齐 contracts/backup-quota.ts: DEFAULT_ROLLBACK_POLICY）。
type RollbackPolicy struct {
	WindowDays             int
	InitiatorTier          string
	RequireSecondConfirm   bool
	SnapshotBeforeRollback bool
	// ExcludedTables **永不回滚**的对象（审计表）。
	ExcludedTables []string
}

// DefaultRollbackPolicy 默认策略：180 天 / 仅 T1 / 需二次确认 / 回滚前先快照 / 审计永不回滚。
func DefaultRollbackPolicy() RollbackPolicy {
	return RollbackPolicy{
		WindowDays:             180,
		InitiatorTier:          "T1",
		RequireSecondConfirm:   true,
		SnapshotBeforeRollback: true,
		ExcludedTables:         []string{"audit_log"},
	}
}

// RollbackRequest 回滚请求。
type RollbackRequest struct {
	Region        Region
	Target        time.Time
	Operator      string
	Tier          string
	Confirmed     bool
	SnapshotTaken bool
}

// 回滚相关错误。
var (
	// ErrRollbackWindowExceeded 回滚点超出保留窗口（或落在未来）。
	ErrRollbackWindowExceeded = errors.New("dr: 回滚点超出保留窗口")
	// ErrNeedSecondConfirm 回滚需 T1 二次确认。
	ErrNeedSecondConfirm = errors.New("dr: 回滚需 T1 二次确认")
	// ErrSnapshotRequired 回滚前必须先快照当前状态。
	ErrSnapshotRequired = errors.New("dr: 回滚前必须先快照当前状态")
	// ErrCrossRegionRollback 禁止跨地域回滚。
	ErrCrossRegionRollback = errors.New("dr: 禁止跨地域回滚")
	// ErrAuditTouched 回滚触动了审计表。
	ErrAuditTouched = errors.New("dr: 回滚触动了审计表（append-only 被破坏）")
	// ErrAuditNotExcluded 回滚策略的排除清单里没有审计表。
	//
	// ★ 为什么这条错误必须存在：PlanRollback 接受**调用方传入**的策略。
	//
	//	默认策略排除了 audit_log，但策略可从配置加载 —— 漏掉 audit_log 时，
	//	回滚会把审计表一起带走，而 Go 侧原本没有任何地方会拦（只有 DB 触发器兜底）。
	//	「回滚不动审计」若只靠 VerifyRollbackAudit（事后比对行数），
	//	等到发现时审计已经被回滚了。
	ErrAuditNotExcluded = errors.New("dr: 回滚策略必须排除审计表（append-only 永不回滚）")
)

// PlanRollback 校验回滚前置条件，返回**永不回滚**的表清单。
//
// 纪律：仅 T1、本地域、窗口内（180 天且不落在未来）、二次确认、回滚前先快照，
// **且排除清单必须包含审计表**（audit.Table）。
//
// ★ 最后一条是 2026-10-07 补的：策略由调用方传入，此前只把 p.ExcludedTables
// 原样返回、**不检查**它是否真的含 audit_log。配置漏一项 ⇒ 回滚带走审计表。
// 现在缺了就拒（ErrAuditNotExcluded），把「回滚不动审计」从**事后比对**
// 提到**事前拒绝**。
func PlanRollback(c *Cluster, p RollbackPolicy, r RollbackRequest, now time.Time) ([]string, error) {
	if c == nil || c.Primary == nil {
		return nil, errors.New("dr: PlanRollback 需要集群")
	}
	if !c.Region.Valid() || r.Region != c.Region {
		return nil, fmt.Errorf("%w：请求=%q 集群=%q", ErrCrossRegionRollback, r.Region, c.Region)
	}
	if strings.ToUpper(strings.TrimSpace(r.Tier)) != strings.ToUpper(p.InitiatorTier) {
		return nil, fmt.Errorf("%w：tier=%q", ErrNotT1, r.Tier)
	}
	if strings.TrimSpace(r.Operator) == "" {
		return nil, errors.New("dr: 回滚必须有操作人")
	}
	if p.RequireSecondConfirm && !r.Confirmed {
		return nil, ErrNeedSecondConfirm
	}
	if p.SnapshotBeforeRollback && !r.SnapshotTaken {
		return nil, ErrSnapshotRequired
	}
	earliest := now.UTC().AddDate(0, 0, -p.WindowDays)
	target := r.Target.UTC()
	if target.After(now.UTC()) {
		return nil, fmt.Errorf("%w：回滚点在未来（%s）", ErrRollbackWindowExceeded, target.Format("2006-01-02"))
	}
	if target.Before(earliest) {
		return nil, fmt.Errorf("%w：target=%s earliest=%s",
			ErrRollbackWindowExceeded, target.Format("2006-01-02"), earliest.Format("2006-01-02"))
	}
	// ★ 事前拒绝：排除清单必须含审计表，否则回滚会把 append-only 的审计一起带走。
	if err := audit.EnsureExcludedFromRollback(p.ExcludedTables); err != nil {
		return nil, fmt.Errorf("%w：%v", ErrAuditNotExcluded, err)
	}
	return append([]string(nil), p.ExcludedTables...), nil
}

// VerifyRollbackAudit 回滚后校验审计表**未被触碰**（G12「回滚不动审计」）。
//
// 行数减少即视为破坏 —— 审计 append-only，回滚不得带走任何一行。
func VerifyRollbackAudit(before, after int) error {
	if v := gate.CheckRollbackKeepsAudit(before, after); len(v) > 0 {
		return fmt.Errorf("%w：%s", ErrAuditTouched, v[0])
	}
	return nil
}
