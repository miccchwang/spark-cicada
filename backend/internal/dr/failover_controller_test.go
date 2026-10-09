package dr

// failover_controller_test.go —— 控制器的**链路级**断言。
//
// 控制器存在的意义：把「集群当前状态」变成一个进程内可持有的对象，
// 让 HTTP 层能真的调进 `Promote` / `PlanRollback` —— 从而让
// `gate.CheckFailoverFencing` / `CheckRollbackKeepsAudit` 不再是「只在测试里被调到」。
//
// 故本文件的重点是：**三条 fencing 条件都必须真的被独立满足**，
// 而不是「有调用点就算数」。

import (
	"context"
	"strings"
	"testing"
	"time"
)

func fcNow() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }

func fcCluster() *Cluster {
	return &Cluster{
		Region:  RegionAPSoutheast1,
		Primary: &Site{Region: RegionAPSoutheast1, Role: RolePrimary},
		Standby: &Site{Region: RegionAPSoutheast1, Role: RoleStandby, ReadOnly: true},
	}
}

func fcController() *FailoverController {
	return NewFailoverController(fcCluster(), NewMemWitness(), NewMemLSNLedger(), nil)
}

func fcReq() PromoteRequest {
	return PromoteRequest{Region: RegionAPSoutheast1, Operator: "ceo", Tier: "T1", Reason: "主库宕机"}
}

// ★ 三步全做完才放行。
func TestFailoverController_FenceThenWitnessThenPromote(t *testing.T) {
	f := fcController()
	ctx := context.Background()

	// ① 未隔离 ⇒ 拒。
	if _, err := f.Promote(ctx, fcReq(), fcNow()); err == nil {
		t.Fatalf("未隔离即提升应被拒")
	} else if !strings.Contains(err.Error(), "隔离") {
		t.Fatalf("拒绝原因应指出未隔离，实际 %v", err)
	}
	// 集群纹丝不动。
	if f.Cluster.Primary.Role != RolePrimary {
		t.Fatalf("被拒后主站点角色不应变化，实际 %s", f.Cluster.Primary.Role)
	}

	// ② 隔离后仍未取见证 ⇒ 拒。
	if err := f.Fence(ctx); err != nil {
		t.Fatalf("隔离失败：%v", err)
	}
	if _, err := f.Promote(ctx, fcReq(), fcNow()); err == nil {
		t.Fatalf("未取见证即提升应被拒")
	} else if !strings.Contains(err.Error(), "见证") {
		t.Fatalf("拒绝原因应指出未取得见证，实际 %v", err)
	}

	// ③ 取见证 + 对账 ⇒ 放行。
	if err := f.AcquireWitness(ctx); err != nil {
		t.Fatalf("取见证失败：%v", err)
	}
	out, err := f.Promote(ctx, fcReq(), fcNow())
	if err != nil {
		t.Fatalf("三步齐备后应放行，实际 %v", err)
	}
	if out.NewPrimary != RolePrimary {
		t.Fatalf("提升后新主应为 PRIMARY，实际 %s", out.NewPrimary)
	}
	if !out.Fencing.OldPrimaryWriteBlocked || !out.Fencing.WitnessAcquired || !out.Fencing.LSNReconciled {
		t.Fatalf("fencing 三项应全部满足，实际 %+v", out.Fencing)
	}
	// 旧主降级且写通道保持隔离（禁止自动切回）。
	if f.Cluster.Primary.Role != RoleStandby || !f.Cluster.Primary.WriteBlocked {
		t.Fatalf("旧主应降级为备且保持隔离，实际 role=%s blocked=%v",
			f.Cluster.Primary.Role, f.Cluster.Primary.WriteBlocked)
	}
}

// ★ 未注入见证者/LSN 时一律拒绝（fail-closed）—— 宁可切不了，不可脑裂。
func TestFailoverController_AcquireWitnessWithoutLedgersIsRejected(t *testing.T) {
	f := NewFailoverController(fcCluster(), nil, nil, nil)
	if err := f.AcquireWitness(context.Background()); err == nil {
		t.Fatalf("未装配见证者/LSN 时应拒绝")
	}
}

// ★ 未装配集群时一切操作 fail-closed。
func TestFailoverController_NilClusterIsFailClosed(t *testing.T) {
	f := NewFailoverController(nil, NewMemWitness(), NewMemLSNLedger(), nil)
	ctx := context.Background()
	if err := f.Fence(ctx); err == nil {
		t.Fatalf("无集群应拒绝隔离")
	}
	if _, err := f.Promote(ctx, fcReq(), fcNow()); err == nil {
		t.Fatalf("无集群应拒绝切换")
	}
	if _, err := f.RollbackPlan(ctx, RollbackRequest{Region: RegionAPSoutheast1}, fcNow()); err == nil {
		t.Fatalf("无集群应拒绝回滚计划")
	}
	if _, ok := f.Snapshot(); ok {
		t.Fatalf("无集群时 Snapshot 应返回 ok=false")
	}
}

// ★ 地域隔离：请求地域与集群不一致 ⇒ 拒（地域之间互不接管）。
func TestFailoverController_RegionMismatchRejected(t *testing.T) {
	f := fcController()
	ctx := context.Background()
	_ = f.Fence(ctx)
	_ = f.AcquireWitness(ctx)
	req := fcReq()
	req.Region = RegionUSEast1
	if _, err := f.Promote(ctx, req, fcNow()); err == nil {
		t.Fatalf("跨地域切换应被拒")
	}
}

// 回滚计划：排除清单必须含审计表（G12「回滚不动审计」）。
func TestFailoverController_RollbackPlanKeepsAudit(t *testing.T) {
	f := fcController()
	req := RollbackRequest{
		Region: RegionAPSoutheast1, Target: fcNow().AddDate(0, -1, 0),
		Operator: "ceo", Tier: "T1", Confirmed: true, SnapshotTaken: true,
	}
	tables, err := f.RollbackPlan(context.Background(), req, fcNow())
	if err != nil {
		t.Fatalf("回滚计划应通过，实际 %v", err)
	}
	found := false
	for _, tb := range tables {
		if tb == "audit_log" {
			found = true
		}
	}
	if !found {
		t.Fatalf("排除清单必须含 audit_log，实际 %v", tables)
	}
}

// ★ 见证者默认**未取得**（若默认 true，fencing 两项会退化为装饰）。
func TestMemWitness_DefaultsToNotAcquired(t *testing.T) {
	w := NewMemWitness()
	if w.Acquired(RegionAPSoutheast1) {
		t.Fatalf("见证者默认应为未取得（否则 fencing 退化为装饰）")
	}
	w.Acquire(RegionAPSoutheast1)
	if !w.Acquired(RegionAPSoutheast1) {
		t.Fatalf("Acquire 后应已取得")
	}
}

// ★ LSN 账本默认**未对账**。
func TestMemLSNLedger_DefaultsToNotReconciled(t *testing.T) {
	l := NewMemLSNLedger()
	if l.Reconciled(RegionAPSoutheast1) {
		t.Fatalf("LSN 账本默认应为未对账")
	}
	l.MarkReconciled(RegionAPSoutheast1)
	if !l.Reconciled(RegionAPSoutheast1) {
		t.Fatalf("MarkReconciled 后应已对账")
	}
}
