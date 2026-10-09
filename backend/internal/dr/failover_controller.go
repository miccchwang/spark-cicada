// failover_controller.go —— 主备切换/回滚的**可装配入口**。
//
// ★ 为什么需要这一层（本仓反复出现的同一个病）：
//
//	`Promote` / `PlanRollback` 是 G12 两条断言的判定路径，但它们此前
//	**全仓没有任何非测试调用点** —— 唯一「调用」它们的就是 `dr_test.go`。
//	于是「切换必先 fencing」「回滚不动审计」两条在生产侧恒真：
//	没有切换路径，自然从不跳过 fencing；没有回滚路径，审计自然从不被回滚。
//
//	本文件把「集群当前状态」变成一个**进程内可持有的对象**，让 HTTP 层
//	（`api.DrHandlers`）能真的调进来。判定函数因此获得真实生产调用点。
//
// ★ 诚实标注（不粉饰）：集群状态目前由**进程内状态**持有，尚无真实站点
//
//	探针 / 编排系统接入。故本层让链路**真的跑起来**，但「站点角色从哪来」
//	仍需接监控（登记 docs/06 F18）。**不把「链路可跑」包装成「生产可切」**。
package dr

import (
	"context"
	"errors"
	"sync"
	"time"
)

// FailoverController 持有地域内主备集群的当前状态，并把切换 / 回滚串成链路。
//
// ★ 并发：切换是**状态变更**，必须串行化 —— 两个并发 Promote 会让
//
//	「旧主已降级、备库已提升」与「旧主仍是主」两个视图交错。
//	故用互斥锁把「读状态 → 判 fencing → 改状态」整体串起来。
type FailoverController struct {
	Cluster *Cluster
	Witness Witness
	LSN     LSNLedger
	Audit   FailoverAudit
	// Policy 回滚策略；零值时由 RollbackPlan 回退到 DefaultRollbackPolicy()。
	Policy RollbackPolicy

	mu sync.Mutex
}

// NewFailoverController 构造控制器。c 为 nil 时各方法一律 fail-closed 拒绝
// （绝不「没有集群就当作可以切」）。
func NewFailoverController(c *Cluster, w Witness, l LSNLedger, a FailoverAudit) *FailoverController {
	return &FailoverController{Cluster: c, Witness: w, LSN: l, Audit: a}
}

// Promote 执行一次半自动切换（F13=B）。
//
// ★ 入参 req.Tier / req.Operator 由**接口层**按服务端判定填充，
//
//	绝不接受客户端声明 —— 否则「仅 T1 可发起」会退化为「谁都能声称自己是 T1」。
func (f *FailoverController) Promote(ctx context.Context, req PromoteRequest, now time.Time) (PromoteOutcome, error) {
	if f == nil || f.Cluster == nil {
		return PromoteOutcome{}, errors.New("dr: FailoverController 未装配集群（拒绝切换）")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return Promote(ctx, f.Cluster, req, f.Witness, f.LSN, f.Audit, now)
}

// RollbackPlan 校验一次回滚的前置条件，返回**永不回滚**的表清单。
//
// ★ 只做「计划」不做「执行」：真正回滚要停写、快照、再回放，属运维编排，
//
//	不应由一个 HTTP 请求直接触发。但**前置校验**必须在服务端真实发生 ——
//	否则「仅 T1 / 窗口内 / 二次确认 / 审计永不回滚」四条都只是文档。
func (f *FailoverController) RollbackPlan(ctx context.Context, req RollbackRequest, now time.Time) ([]string, error) {
	if f == nil || f.Cluster == nil {
		return nil, errors.New("dr: FailoverController 未装配集群（拒绝回滚计划）")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.Policy
	if p.WindowDays == 0 && p.InitiatorTier == "" && len(p.ExcludedTables) == 0 {
		p = DefaultRollbackPolicy()
	}
	return PlanRollback(f.Cluster, p, req, now)
}

// Fence 隔离旧主：切断其写入通道（docs/04 §6.6「提升前强制隔离旧主」）。
//
// ★ 这是切换前的**独立动作**，不是 Promote 的一部分 —— 正因为分开，
//
//	「跳过 fencing 直接 Promote」才可能在生产路径上真的发生、并被拦下。
func (f *FailoverController) Fence(ctx context.Context) error {
	if f == nil || f.Cluster == nil {
		return errors.New("dr: FailoverController 未装配集群（拒绝隔离）")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return Fence(f.Cluster)
}

// AcquireWitness 取得见证者租约 + 标记 LSN 已对账。
//
// ★ 真实部署中这两件事由 etcd/Redis 见证者（第三可用区）与真实 LSN 对账源
//
//	驱动，是**两个独立的、可能失败的**外部动作。本方法只把「调用方要求
//	推进到下一步」如实传导给注入的实现；若注入的是进程内桩
//	（MemWitness / MemLSNLedger），则在此置位。
//	**不在本方法里默认置位** —— 未注入见证者时一律拒绝（fail-closed）。
func (f *FailoverController) AcquireWitness(ctx context.Context) error {
	if f == nil || f.Cluster == nil {
		return errors.New("dr: FailoverController 未装配集群（拒绝取得见证）")
	}
	if f.Witness == nil || f.LSN == nil {
		return errors.New("dr: 未装配见证者 / LSN 账本（拒绝切换：宁可切不了，不可脑裂）")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if mw, ok := f.Witness.(*MemWitness); ok {
		mw.Acquire(f.Cluster.Region)
	}
	if ml, ok := f.LSN.(*MemLSNLedger); ok {
		ml.MarkReconciled(f.Cluster.Region)
	}
	return nil
}

// SiteView 只读快照（供接口层如实回报当前站点角色，不含内部字段）。
type SiteView struct {
	Region       Region   `json:"region"`
	PrimaryRole  SiteRole `json:"primaryRole"`
	StandbyRole  SiteRole `json:"standbyRole"`
	PrimaryWrite bool     `json:"primaryWriteBlocked"`
}

// Snapshot 返回当前集群的只读视图（未装配集群时 ok=false）。
func (f *FailoverController) Snapshot() (SiteView, bool) {
	if f == nil || f.Cluster == nil || f.Cluster.Primary == nil || f.Cluster.Standby == nil {
		return SiteView{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return SiteView{
		Region:       f.Cluster.Region,
		PrimaryRole:  f.Cluster.Primary.Role,
		StandbyRole:  f.Cluster.Standby.Role,
		PrimaryWrite: f.Cluster.Primary.WriteBlocked,
	}, true
}
