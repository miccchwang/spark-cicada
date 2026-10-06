package gate_test

// G12 · 容灾与备份下载闭环 ——**链路级**闸门。
//
// ★ 为什么单列一个文件（第五个变种，同一个病）：
//
//	`gate.CheckMonthlyQuota` / `CheckNoCrossRegionWrite` / `CheckFailoverFencing` /
//	`CheckRollbackKeepsAudit` 四个函数，**全仓没有任何非测试调用点** ——
//	唯一「调用」它们的就是 `gate_test.go` 里那几条断言。于是：
//
//	  没有下载路径 ⇒ 「同月第二次下载被拒」恒真；
//	  没有跨地域写路径 ⇒ 「禁止跨地域写同一行」恒真；
//	  没有切换路径 ⇒ 「切换必先 fencing」恒真；
//	  没有回滚路径 ⇒ 「回滚不动审计」恒真。
//
//	四条**永远为真**的断言，而 docs/05 标着「✅ 已实现」。
//
// 本文件补的是**链路断言**：从 internal/dr 的真实入口进，以「业务结果」出 ——
// 闸门若被摘掉/被改坏，链路行为立刻变化，断言即失败。
//
// docs/04 §6.1.1 / §6.3 / §6.4 / §6.6；docs/05 G12。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/dr"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

var g12Now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

type g12Signer struct{ raw string }

func (s g12Signer) SignOnce(_ context.Context, a dr.Archive, _ string,
	ttl time.Duration, _ time.Time) (string, error) {
	if s.raw != "" {
		return s.raw, nil
	}
	return "https://backup.example.com/" + string(a.Region) + "/" + a.ID + "?tok=once", nil
}

func g12Store() *dr.MemQuotaStore {
	s := dr.NewMemQuotaStore()
	for _, r := range []dr.Region{dr.RegionAPSoutheast1, dr.RegionUSEast1} {
		s.PutArchive(dr.Archive{
			ID: "a-" + string(r), Region: r, DataMonth: dr.PrevDataMonth(g12Now),
			GeneratedAt: g12Now.Add(-24 * time.Hour), SHA256: "sum-" + string(r),
		})
	}
	return s
}

// ───────────── ① 下载配额链路（§6.4 / B3） ─────────────

// TestG12_DownloadQuotaChain 从 RequestDownload 入口进：
// 同月同域第二次必须被拒 —— 证明 CheckMonthlyQuota 真的在生产路径上。
func TestG12_DownloadQuotaChain(t *testing.T) {
	store := g12Store()
	svc := &dr.DownloadService{Store: store, Signer: g12Signer{}}
	ctx := context.Background()
	sg := "a-" + string(dr.RegionAPSoutheast1)

	// 前置锚点：闸门本身确实会拒（防「把闸门改成恒 true」的破坏）
	if allowed, _ := gate.CheckMonthlyQuota(1); allowed {
		t.Fatal("锚点失效：CheckMonthlyQuota(1) 应拒绝")
	}

	first, err := svc.RequestDownload(ctx, "u1", dr.RegionAPSoutheast1, sg, g12Now)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Allowed || first.SignedURL == "" {
		t.Fatalf("首次下载应放行并给签名 URL：%+v", first)
	}
	second, err := svc.RequestDownload(ctx, "u1", dr.RegionAPSoutheast1, sg, g12Now)
	if err != nil {
		t.Fatal(err)
	}
	if second.Allowed || second.Reason != dr.ReasonQuotaExceeded {
		t.Fatalf("同月同域第二次必须被拒：allowed=%v reason=%s", second.Allowed, second.Reason)
	}
	// ★ 拒绝必须携带闸门（CheckMonthlyQuota）给出的人类可读原因 ——
	// 这条断言让「生产路径把 CheckMonthlyQuota 摘掉」这件事**可被检出**
	// （否则只靠原子兜底，闸门被摘掉也不会有任何测试变红）。
	if !strings.Contains(second.Detail, "配额") {
		t.Fatalf("拒绝必须给出人类可读的配额原因：%q", second.Detail)
	}
	if !second.NextAvailableAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("应告知下次可下载时间：%s", second.NextAvailableAt)
	}
	// 分地域：美国地域配额不受新加坡影响
	us, err := svc.RequestDownload(ctx, "u1", dr.RegionUSEast1, "a-"+string(dr.RegionUSEast1), g12Now)
	if err != nil || !us.Allowed {
		t.Fatalf("分地域计数：US 应仍可下：allowed=%v err=%v", us.Allowed, err)
	}
}

// TestG12_DownloadURLNoCredentials 签名 URL 必须过 docs/09 铁律（G11 呼应）。
func TestG12_DownloadURLNoCredentials(t *testing.T) {
	if !gate.URLHasCredentials("https://u:p@h/x") {
		t.Fatal("锚点失效：凭据形态 URL 应被识别")
	}
	svc := &dr.DownloadService{Store: g12Store(),
		Signer: g12Signer{raw: "https://u:p@backup.example.com/x"}}
	_, err := svc.RequestDownload(context.Background(), "u1", dr.RegionAPSoutheast1,
		"a-"+string(dr.RegionAPSoutheast1), g12Now)
	if err == nil || !strings.Contains(err.Error(), "凭据") {
		t.Fatalf("含凭据的签名 URL 必须被拒：%v", err)
	}
}

// ───────────── ② 跨地域写链路（§6.1.1） ─────────────

type g12RowStore struct{ n int }

func (s *g12RowStore) Upsert(_ context.Context, _ dr.Row) error { s.n++; return nil }

func TestG12_CrossRegionWriteChain(t *testing.T) {
	if len(gate.CheckNoCrossRegionWrite("a", "b")) == 0 {
		t.Fatal("锚点失效：跨地域写应被闸门拦下")
	}
	down := &g12RowStore{}
	w := &dr.RowWriter{Store: down, WriterRegion: dr.RegionAPSoutheast1}
	ctx := context.Background()

	if err := w.Upsert(ctx, dr.Row{Table: "t", Key: "k", Region: dr.RegionAPSoutheast1}); err != nil {
		t.Fatalf("本地域写应通过：%v", err)
	}
	err := w.Upsert(ctx, dr.Row{Table: "t", Key: "k2", Region: dr.RegionUSEast1})
	if !errors.Is(err, dr.ErrCrossRegionWrite) {
		t.Fatalf("跨地域写必须被拒：%v", err)
	}
	if down.n != 1 {
		t.Fatalf("被拒的写不得到达下游存储：n=%d", down.n)
	}
}

// ───────────── ③ 切换 fencing 链路（§6.6 / F13=B） ─────────────

func g12Cluster() *dr.Cluster {
	return &dr.Cluster{
		Region:  dr.RegionAPSoutheast1,
		Primary: &dr.Site{Region: dr.RegionAPSoutheast1, Role: dr.RolePrimary},
		Standby: &dr.Site{Region: dr.RegionAPSoutheast1, Role: dr.RoleStandby},
	}
}

type g12Witness struct{ ok bool }

func (w g12Witness) Acquired(dr.Region) bool { return w.ok }

type g12LSN struct{ ok bool }

func (l g12LSN) Reconciled(dr.Region) bool { return l.ok }

// TestG12_FailoverRequiresFencing 从 Promote 入口进：
// **跳过 Fence 直接切换必须失败，且集群纹丝不动**。
func TestG12_FailoverRequiresFencing(t *testing.T) {
	// 锚点：fencing 未完成时闸门确实报违规
	if len(gate.CheckFailoverFencing(gate.Fencing{})) == 0 {
		t.Fatal("锚点失效：空 fencing 应报违规")
	}

	c := g12Cluster()
	_, err := dr.Promote(context.Background(), c,
		dr.PromoteRequest{Region: c.Region, Operator: "t1", Tier: "T1"},
		g12Witness{ok: true}, g12LSN{ok: true}, nil, g12Now)
	if !errors.Is(err, dr.ErrFencingIncomplete) {
		t.Fatalf("未 fencing 必须被拒：%v", err)
	}
	if c.Primary.Role != dr.RolePrimary || c.Standby.Role != dr.RoleStandby {
		t.Fatalf("拒绝时集群必须纹丝不动：primary=%s standby=%s", c.Primary.Role, c.Standby.Role)
	}

	// 补上 Fence + 见证 + LSN 对账 ⇒ 通过，且旧主降级后写通道保持隔离
	if err := dr.Fence(c); err != nil {
		t.Fatal(err)
	}
	out, err := dr.Promote(context.Background(), c,
		dr.PromoteRequest{Region: c.Region, Operator: "t1", Tier: "T1", Reason: "演练"},
		g12Witness{ok: true}, g12LSN{ok: true}, nil, g12Now)
	if err != nil {
		t.Fatalf("fencing 齐备应通过：%v", err)
	}
	if !out.Fencing.OldPrimaryWriteBlocked || !out.Fencing.WitnessAcquired || !out.Fencing.LSNReconciled {
		t.Fatal("outcome 应记录已 fencing 三项齐备")
	}
	if c.Primary.Role != dr.RoleStandby || !c.Primary.WriteBlocked || !c.Primary.ReadOnly {
		t.Fatalf("旧主应降级为备且保持隔离（禁止自动切回）：%+v", c.Primary)
	}
	if c.Standby.Role != dr.RolePrimary {
		t.Fatalf("备库应提升为主：%+v", c.Standby)
	}
}

// TestG12_FencingDerivedFromStateNotDeclaration
// ★ 闸门测的是**站点真实状态**，不是调用方声明 —— 声明无法绕过。
func TestG12_FencingDerivedFromStateNotDeclaration(t *testing.T) {
	c := g12Cluster()
	// 站点状态：旧主写通道未隔离
	f := dr.FencingFrom(c, g12Witness{ok: true}, g12LSN{ok: true})
	if f.OldPrimaryWriteBlocked {
		t.Fatal("FencingFrom 不得凭空认定旧主已隔离")
	}
	if len(gate.CheckFailoverFencing(f)) == 0 {
		t.Fatal("未隔离状态必须被闸门判违规")
	}
	// Fence 之后，状态驱动 fencing 通过（见证/LSN 也齐备）
	if err := dr.Fence(c); err != nil {
		t.Fatal(err)
	}
	f2 := dr.FencingFrom(c, g12Witness{ok: true}, g12LSN{ok: true})
	if !f2.OldPrimaryWriteBlocked || !f2.WitnessAcquired || !f2.LSNReconciled {
		t.Fatalf("三项齐备应全为 true：%+v", f2)
	}
	if v := gate.CheckFailoverFencing(f2); len(v) != 0 {
		t.Fatalf("三项齐备不应报违规：%v", v)
	}
}

// TestG12_SemiAutoNeverAutoPromotes 半自动：探测只告警，绝不自动切换。
func TestG12_SemiAutoNeverAutoPromotes(t *testing.T) {
	c := g12Cluster()
	before := *c.Primary
	al, need := dr.EvaluateProbe(dr.Probe{
		Region: c.Region, HealthFailures: dr.HealthFailureThreshold, RegionProbeOK: true,
	}, 10, g12Now)
	if !need || al.Severity != "SUGGEST_FAILOVER" {
		t.Fatalf("应产出「建议切换」告警：%+v need=%v", al, need)
	}
	if *c.Primary != before {
		t.Fatal("自动探测不得改动集群（不自动切）")
	}
	// 非 T1 无法一键切换（越权即拒）
	if _, err := dr.Promote(context.Background(), c,
		dr.PromoteRequest{Region: c.Region, Operator: "x", Tier: "T2"},
		g12Witness{ok: true}, g12LSN{ok: true}, nil, g12Now); !errors.Is(err, dr.ErrNotT1) {
		t.Fatalf("非 T1 应被拒：%v", err)
	}
}

// ───────────── ④ 回滚不动审计（§6.3 / B2） ─────────────

func TestG12_RollbackChainKeepsAudit(t *testing.T) {
	if len(gate.CheckRollbackKeepsAudit(10, 9)) == 0 {
		t.Fatal("锚点失效：审计行数减少应报违规")
	}
	c := g12Cluster()
	p := dr.DefaultRollbackPolicy()
	ok := dr.RollbackRequest{Region: c.Region, Operator: "t1", Tier: "T1",
		Confirmed: true, SnapshotTaken: true, Target: g12Now.AddDate(0, 0, -30)}

	excl, err := dr.PlanRollback(c, p, ok, g12Now)
	if err != nil {
		t.Fatalf("合法回滚应通过：%v", err)
	}
	found := false
	for _, e := range excl {
		if e == "audit_log" {
			found = true
		}
	}
	if !found {
		t.Fatalf("回滚必须排除审计表：%v", excl)
	}
	// 回滚后审计行数减少 ⇒ 必须报错
	if err := dr.VerifyRollbackAudit(100, 99); !errors.Is(err, dr.ErrAuditTouched) {
		t.Fatalf("回滚带走审计行必须报错：%v", err)
	}
	if err := dr.VerifyRollbackAudit(100, 100); err != nil {
		t.Fatalf("审计行数不变应通过：%v", err)
	}
	// 超窗 / 跨地域 / 未二次确认
	bad := ok
	bad.Target = g12Now.AddDate(0, 0, -181)
	if _, err := dr.PlanRollback(c, p, bad, g12Now); !errors.Is(err, dr.ErrRollbackWindowExceeded) {
		t.Fatalf("超 180 天应被拒：%v", err)
	}
	bad = ok
	bad.Region = dr.RegionUSEast1
	if _, err := dr.PlanRollback(c, p, bad, g12Now); !errors.Is(err, dr.ErrCrossRegionRollback) {
		t.Fatalf("跨地域回滚应被拒：%v", err)
	}
	bad = ok
	bad.Confirmed = false
	if _, err := dr.PlanRollback(c, p, bad, g12Now); !errors.Is(err, dr.ErrNeedSecondConfirm) {
		t.Fatalf("未二次确认应被拒：%v", err)
	}
}
