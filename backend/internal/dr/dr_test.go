package dr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ───────────────────────────── 夹具 ─────────────────────────────

type fakeSigner struct {
	raw string // 非空则原样返回（用于注入含凭据的 URL）
}

func (f fakeSigner) SignOnce(_ context.Context, a Archive, account string,
	ttl time.Duration, _ time.Time) (string, error) {
	if f.raw != "" {
		return f.raw, nil
	}
	return fmt.Sprintf("https://backup.example.com/%s/%s?tok=onetime&ttl=%s",
		a.Region, a.ID, ttl), nil
}

type auditSpy struct {
	recs []AuditRecord
	err  error
}

func (a *auditSpy) sink(_ context.Context, r AuditRecord) error {
	a.recs = append(a.recs, r)
	return a.err
}

func (a *auditSpy) outcomes() []string {
	var out []string
	for _, r := range a.recs {
		out = append(out, r.Outcome)
	}
	return out
}

type witness struct{ ok map[Region]bool }

func (w witness) Acquired(r Region) bool { return w.ok[r] }

type lsnLedger struct{ ok map[Region]bool }

func (l lsnLedger) Reconciled(r Region) bool { return l.ok[r] }

type memRowStore struct{ rows []Row }

func (m *memRowStore) Upsert(_ context.Context, r Row) error {
	m.rows = append(m.rows, r)
	return nil
}

// now0 固定时钟：2026-10-06（+08 无关，内部一律 UTC）。
var now0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func seededStore(t *testing.T) *MemQuotaStore {
	t.Helper()
	s := NewMemQuotaStore()
	for _, r := range []Region{RegionAPSoutheast1, RegionUSEast1} {
		s.PutArchive(Archive{
			ID: "arch-" + string(r), Region: r, DataMonth: PrevDataMonth(now0),
			GeneratedAt: now0.Add(-24 * time.Hour), SizeBytes: 1024, SHA256: "sha-" + string(r),
		})
	}
	return s
}

func newSvc(s QuotaStore, aud AuditSink) *DownloadService {
	return &DownloadService{Store: s, Signer: fakeSigner{}, Audit: aud}
}

// ───────────────────────────── B3 · 下载配额 ─────────────────────────────

// TestQuota_PerRegionIndependent 分地域计数：新加坡下载**不消耗**美国配额。
func TestQuota_PerRegionIndependent(t *testing.T) {
	store := seededStore(t)
	svc := newSvc(store, nil)

	got, err := svc.RequestDownload(context.Background(), "u1", RegionAPSoutheast1,
		"arch-"+string(RegionAPSoutheast1), now0)
	if err != nil || !got.Allowed {
		t.Fatalf("SG 首次下载应放行：allowed=%v err=%v", got.Allowed, err)
	}
	// 美国地域仍应可下（配额各记各的）
	got2, err := svc.RequestDownload(context.Background(), "u1", RegionUSEast1,
		"arch-"+string(RegionUSEast1), now0)
	if err != nil || !got2.Allowed {
		t.Fatalf("US 首次下载应放行（分地域计数）：allowed=%v err=%v", got2.Allowed, err)
	}
	// 新加坡第二次应被拒
	got3, err := svc.RequestDownload(context.Background(), "u1", RegionAPSoutheast1,
		"arch-"+string(RegionAPSoutheast1), now0)
	if err != nil {
		t.Fatal(err)
	}
	if got3.Allowed || got3.Reason != ReasonQuotaExceeded {
		t.Fatalf("SG 同月第二次应被拒：allowed=%v reason=%s", got3.Allowed, got3.Reason)
	}
}

// TestQuota_MonthlyResetAndNextAvailable 拒绝时给出下一次可下载时间，且次月重置。
func TestQuota_MonthlyResetAndNextAvailable(t *testing.T) {
	store := seededStore(t)
	svc := newSvc(store, nil)
	ctx := context.Background()
	id := "arch-" + string(RegionAPSoutheast1)

	if _, err := svc.RequestDownload(ctx, "u1", RegionAPSoutheast1, id, now0); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.RequestDownload(ctx, "u1", RegionAPSoutheast1, id, now0)
	if got.Allowed {
		t.Fatal("同月第二次应被拒")
	}
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if !got.NextAvailableAt.Equal(want) {
		t.Fatalf("nextAvailableAt=%s want=%s", got.NextAvailableAt, want)
	}
	if got.Detail == "" {
		t.Fatal("拒绝应给出人类可读原因")
	}

	// 次月：配额重置，可再下（归档需重新登记为 11 月可见）
	nextMonth := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	store.PutArchive(Archive{
		ID: "arch-nov", Region: RegionAPSoutheast1, DataMonth: PrevDataMonth(nextMonth),
		GeneratedAt: nextMonth.Add(-24 * time.Hour), SHA256: "sha-nov",
	})
	got2, err := svc.RequestDownload(ctx, "u1", RegionAPSoutheast1, "arch-nov", nextMonth)
	if err != nil || !got2.Allowed {
		t.Fatalf("次月应重置配额：allowed=%v err=%v", got2.Allowed, err)
	}
}

// TestQuota_RejectionsDoNotConsume 所有拒绝路径**不消耗配额**。
func TestQuota_RejectionsDoNotConsume(t *testing.T) {
	store := seededStore(t)
	svc := newSvc(store, nil)
	ctx := context.Background()
	id := "arch-" + string(RegionAPSoutheast1)

	// 归档不存在
	if r, _ := svc.RequestDownload(ctx, "u1", RegionAPSoutheast1, "nope", now0); r.Reason != ReasonArchiveNotFound {
		t.Fatalf("reason=%s want ARCHIVE_NOT_FOUND", r.Reason)
	}
	// 无 actor
	if r, _ := svc.RequestDownload(ctx, "", RegionAPSoutheast1, id, now0); r.Reason != ReasonNotAuthorized {
		t.Fatalf("reason=%s want NOT_AUTHORIZED", r.Reason)
	}
	// 非法地域
	if r, _ := svc.RequestDownload(ctx, "u1", Region("ap-southeast-2"), id, now0); r.Reason != ReasonNotAuthorized {
		t.Fatalf("非法地域应拒绝：reason=%s", r.Reason)
	}
	// 未生成
	store.PutArchive(Archive{ID: "pending", Region: RegionAPSoutheast1,
		DataMonth: PrevDataMonth(now0), SHA256: "x"})
	if r, _ := svc.RequestDownload(ctx, "u1", RegionAPSoutheast1, "pending", now0); r.Reason != ReasonNotYetAvailable {
		t.Fatalf("未生成应 NOT_YET_AVAILABLE：reason=%s", r.Reason)
	}

	used, _ := store.Used(ctx, "u1", RegionAPSoutheast1, PeriodOf(now0))
	if used != 0 {
		t.Fatalf("拒绝路径不应消耗配额，used=%d", used)
	}
	// 且此时真实下载仍应放行
	if r, _ := svc.RequestDownload(ctx, "u1", RegionAPSoutheast1, id, now0); !r.Allowed {
		t.Fatal("配额应仍可用")
	}
}

// TestQuota_ArchiveMustBelongToRegion 拿 A 地域的配额下 B 地域的归档一律拒绝。
func TestQuota_ArchiveMustBelongToRegion(t *testing.T) {
	store := seededStore(t)
	svc := newSvc(store, nil)
	// 用 SG 地域去取 US 的归档 ID
	r, err := svc.RequestDownload(context.Background(), "u1", RegionAPSoutheast1,
		"arch-"+string(RegionUSEast1), now0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Allowed || r.Reason != ReasonArchiveNotFound {
		t.Fatalf("跨地域取归档应拒：allowed=%v reason=%s", r.Allowed, r.Reason)
	}
}

// TestQuota_SignedURLMustNotCarryCredentials docs/09：签名 URL 不得含凭据。
func TestQuota_SignedURLMustNotCarryCredentials(t *testing.T) {
	store := seededStore(t)
	svc := &DownloadService{Store: store,
		Signer: fakeSigner{raw: "https://user:pw@backup.example.com/a"}, Audit: nil}
	_, err := svc.RequestDownload(context.Background(), "u1", RegionAPSoutheast1,
		"arch-"+string(RegionAPSoutheast1), now0)
	if err == nil || !strings.Contains(err.Error(), "凭据") {
		t.Fatalf("含凭据的 URL 必须被拒：err=%v", err)
	}
	// 且不消耗配额
	used, _ := store.Used(context.Background(), "u1", RegionAPSoutheast1, PeriodOf(now0))
	if used != 0 {
		t.Fatalf("签名失败不应消耗配额，used=%d", used)
	}
}

// TestQuota_AuditRecordsWhoWhenMonthRegionChecksum 留痕含「谁/何时/哪月/哪地域/校验和」。
func TestQuota_AuditRecordsWhoWhenMonthRegionChecksum(t *testing.T) {
	store := seededStore(t)
	spy := &auditSpy{}
	svc := newSvc(store, spy.sink)
	ctx := context.Background()
	id := "arch-" + string(RegionAPSoutheast1)

	if _, err := svc.RequestDownload(ctx, "u1", RegionAPSoutheast1, id, now0); err != nil {
		t.Fatal(err)
	}
	if len(spy.recs) != 1 {
		t.Fatalf("应留痕 1 条，实际 %d", len(spy.recs))
	}
	r := spy.recs[0]
	if r.Account != "u1" || !r.At.Equal(now0) || r.Region != RegionAPSoutheast1 ||
		r.DataMonth != PrevDataMonth(now0) || r.SHA256 != "sha-"+string(RegionAPSoutheast1) {
		t.Fatalf("审计字段不完整：%+v", r)
	}
	if r.Outcome != "OK" {
		t.Fatalf("outcome=%s", r.Outcome)
	}
	// 第二次被拒也应留痕（谁在何时想下哪个地域哪个月的归档）
	_, _ = svc.RequestDownload(ctx, "u1", RegionAPSoutheast1, id, now0)
	if len(spy.recs) != 2 || spy.recs[1].Outcome != ReasonQuotaExceeded {
		t.Fatalf("拒绝也应留痕：%+v", spy.outcomes())
	}
}

// TestQuota_AuditFailureDoesNotBlockButIsVisible 审计失败不阻断下载，但必须显式暴露。
func TestQuota_AuditFailureDoesNotBlockButIsVisible(t *testing.T) {
	store := seededStore(t)
	spy := &auditSpy{err: errors.New("db down")}
	svc := newSvc(store, spy.sink)
	r, err := svc.RequestDownload(context.Background(), "u1", RegionAPSoutheast1,
		"arch-"+string(RegionAPSoutheast1), now0)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Allowed {
		t.Fatal("审计失败不应阻断下载")
	}
	if r.Audited || r.Warn == "" {
		t.Fatalf("必须显式暴露未留痕：audited=%v warn=%q", r.Audited, r.Warn)
	}
}

// TestQuota_ConcurrentConsumeOnceIsAtomic 并发下「每月 1 次」不被突破。
func TestQuota_ConcurrentConsumeOnceIsAtomic(t *testing.T) {
	store := NewMemQuotaStore()
	store.PutArchive(Archive{ID: "a", Region: RegionUSEast1,
		DataMonth: PrevDataMonth(now0), GeneratedAt: now0.Add(-time.Hour), SHA256: "s"})
	svc := newSvc(store, nil)

	const n = 16
	okCh := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() {
			r, err := svc.RequestDownload(context.Background(), "u1", RegionUSEast1, "a", now0)
			okCh <- err == nil && r.Allowed
		}()
	}
	allowed := 0
	for i := 0; i < n; i++ {
		if <-okCh {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("并发下应恰好放行 1 次，实际 %d", allowed)
	}
}

// ───────────────────────────── §6.1.1 · 跨地域写护栏 ─────────────────────────────

func TestRowWriter_CrossRegionWriteBlocked(t *testing.T) {
	store := &memRowStore{}
	w := &RowWriter{Store: store, WriterRegion: RegionAPSoutheast1}
	ctx := context.Background()

	// 本地域写：通过
	if err := w.Upsert(ctx, Row{Table: "fact_sales_daily", Key: "k1", Region: RegionAPSoutheast1}); err != nil {
		t.Fatalf("本地域写应通过：%v", err)
	}
	// 跨地域写：拒绝，且下游 Store **根本不被调用**
	err := w.Upsert(ctx, Row{Table: "fact_sales_daily", Key: "k2", Region: RegionUSEast1})
	if !errors.Is(err, ErrCrossRegionWrite) {
		t.Fatalf("跨地域写应被拒：%v", err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("被拒的写不得到达下游存储，rows=%d", len(store.rows))
	}
	// 非法地域：拒绝
	w2 := &RowWriter{Store: store, WriterRegion: Region("bogus")}
	if err := w2.Upsert(ctx, Row{Region: Region("bogus")}); err == nil {
		t.Fatal("非法写入方地域应被拒")
	}
}

// ───────────────────────────── F13 · 半自动切换 ─────────────────────────────

func newCluster() *Cluster {
	return &Cluster{
		Region:  RegionAPSoutheast1,
		Primary: &Site{Region: RegionAPSoutheast1, Role: RolePrimary},
		Standby: &Site{Region: RegionAPSoutheast1, Role: RoleStandby},
	}
}

func allGood(r Region) (Witness, LSNLedger) {
	return witness{ok: map[Region]bool{r: true}}, lsnLedger{ok: map[Region]bool{r: true}}
}

// TestPromote_SkippingFenceIsRejectedAndClusterUntouched
// ★ 核心：不调 Fence 直接 Promote ⇒ 拒绝，且集群**纹丝不动**。
func TestPromote_SkippingFenceIsRejectedAndClusterUntouched(t *testing.T) {
	c := newCluster()
	w, l := allGood(c.Region)
	ctx := context.Background()

	_, err := Promote(ctx, c, PromoteRequest{Region: c.Region, Operator: "t1", Tier: "T1"}, w, l, nil, now0)
	if !errors.Is(err, ErrFencingIncomplete) {
		t.Fatalf("未 fencing 应被拒：%v", err)
	}
	if c.Primary.Role != RolePrimary || c.Standby.Role != RoleStandby {
		t.Fatalf("拒绝时集群必须纹丝不动：primary=%s standby=%s", c.Primary.Role, c.Standby.Role)
	}
}

// TestPromote_FencingDerivedFromStateNotDeclaration
// ★ 闸门测的是**站点真实状态**，不是调用方声明 —— 无法「声明 true」绕过。
func TestPromote_FencingDerivedFromStateNotDeclaration(t *testing.T) {
	c := newCluster()
	// 只 Fence，不取见证 ⇒ 仍应被拒（fencing 三项缺一不可）
	if err := Fence(c); err != nil {
		t.Fatal(err)
	}
	wNo, lNo := witness{ok: map[Region]bool{}}, lsnLedger{ok: map[Region]bool{c.Region: true}}
	if _, err := Promote(context.Background(), c,
		PromoteRequest{Region: c.Region, Operator: "t1", Tier: "T1"}, wNo, lNo, nil, now0); !errors.Is(err, ErrFencingIncomplete) {
		t.Fatalf("缺见证应被拒：%v", err)
	}
	// LSN 未对账 ⇒ 仍应被拒
	wYes, lBad := allGood(c.Region)
	lBad = lsnLedger{ok: map[Region]bool{}}
	if _, err := Promote(context.Background(), c,
		PromoteRequest{Region: c.Region, Operator: "t1", Tier: "T1"}, wYes, lBad, nil, now0); !errors.Is(err, ErrFencingIncomplete) {
		t.Fatalf("LSN 未对账应被拒：%v", err)
	}
	// 三项齐备 ⇒ 通过
	if _, err := Promote(context.Background(), c,
		PromoteRequest{Region: c.Region, Operator: "t1", Tier: "T1"}, wYes, lYes(c.Region), nil, now0); err != nil {
		t.Fatalf("三项齐备应通过：%v", err)
	}
}

func lYes(r Region) LSNLedger { return lsnLedger{ok: map[Region]bool{r: true}} }

// TestPromote_SemiAutoDiscipline 仅 T1、必须有操作人、地域隔离、状态幂等。
func TestPromote_SemiAutoDiscipline(t *testing.T) {
	ctx := context.Background()
	mk := func() (*Cluster, Witness, LSNLedger) {
		c := newCluster()
		if err := Fence(c); err != nil {
			t.Fatal(err)
		}
		w, l := allGood(c.Region)
		return c, w, l
	}

	c, w, l := mk()
	if _, err := Promote(ctx, c, PromoteRequest{Region: c.Region, Operator: "x", Tier: "T2"}, w, l, nil, now0); !errors.Is(err, ErrNotT1) {
		t.Fatalf("非 T1 应被拒：%v", err)
	}
	c, w, l = mk()
	if _, err := Promote(ctx, c, PromoteRequest{Region: c.Region, Operator: "", Tier: "T1"}, w, l, nil, now0); err == nil {
		t.Fatal("无操作人应被拒")
	}
	c, w, l = mk()
	if _, err := Promote(ctx, c, PromoteRequest{Region: RegionUSEast1, Operator: "x", Tier: "T1"}, w, l, nil, now0); !errors.Is(err, ErrRegionMismatch) {
		t.Fatalf("跨地域接管应被拒：%v", err)
	}
	// 成功一次后，旧主降级为备且写通道**保持隔离**（禁止自动切回）
	c, w, l = mk()
	out, err := Promote(ctx, c, PromoteRequest{Region: c.Region, Operator: "x", Tier: "T1", Reason: "演练"}, w, l, nil, now0)
	if err != nil {
		t.Fatal(err)
	}
	if out.OldPrimary != RolePrimary || out.NewPrimary != RolePrimary {
		t.Fatalf("outcome 前后状态不对：%+v", out)
	}
	if c.Primary.Role != RoleStandby || !c.Primary.WriteBlocked || !c.Primary.ReadOnly {
		t.Fatalf("旧主应降级为备且保持隔离：%+v", c.Primary)
	}
	if c.Standby.Role != RolePrimary || c.Standby.WriteBlocked {
		t.Fatalf("备库应提升为主且可写：%+v", c.Standby)
	}
}

// TestEvaluateProbe_OnlyAlertsNeverPromotes 半自动：自动探测只告警，绝不切换。
func TestEvaluateProbe_OnlyAlertsNeverPromotes(t *testing.T) {
	c := newCluster()
	before := *c.Primary
	p := Probe{Region: c.Region, HealthFailures: HealthFailureThreshold, RegionProbeOK: true}
	al, need := EvaluateProbe(p, 10, now0)
	if !need || al.Severity != "SUGGEST_FAILOVER" || len(al.Reasons) == 0 {
		t.Fatalf("应产出建议切换告警：%+v need=%v", al, need)
	}
	// 探测不得改动集群（切换必须由 T1 显式发起）
	if *c.Primary != before {
		t.Fatalf("探测不得改动集群：%+v", c.Primary)
	}
	// 健康时无告警
	if _, need := EvaluateProbe(Probe{Region: c.Region, RegionProbeOK: true}, 10, now0); need {
		t.Fatal("健康时不应告警")
	}
}

// ───────────────────────────── B2 · 180 天回滚 ─────────────────────────────

func TestPlanRollback_Guards(t *testing.T) {
	c := newCluster()
	p := DefaultRollbackPolicy()
	base := RollbackRequest{Region: c.Region, Operator: "t1", Tier: "T1",
		Confirmed: true, SnapshotTaken: true, Target: now0.AddDate(0, 0, -30)}

	excl, err := PlanRollback(c, p, base, now0)
	if err != nil {
		t.Fatalf("合法回滚应通过：%v", err)
	}
	if len(excl) != 1 || excl[0] != "audit_log" {
		t.Fatalf("必须排除审计表：%v", excl)
	}

	// 超出 180 天窗口
	bad := base
	bad.Target = now0.AddDate(0, 0, -181)
	if _, err := PlanRollback(c, p, bad, now0); !errors.Is(err, ErrRollbackWindowExceeded) {
		t.Fatalf("超窗应被拒：%v", err)
	}
	// 未来
	bad = base
	bad.Target = now0.AddDate(0, 0, 1)
	if _, err := PlanRollback(c, p, bad, now0); !errors.Is(err, ErrRollbackWindowExceeded) {
		t.Fatalf("未来回滚点应被拒：%v", err)
	}
	// 未二次确认
	bad = base
	bad.Confirmed = false
	if _, err := PlanRollback(c, p, bad, now0); !errors.Is(err, ErrNeedSecondConfirm) {
		t.Fatalf("未二次确认应被拒：%v", err)
	}
	// 未快照
	bad = base
	bad.SnapshotTaken = false
	if _, err := PlanRollback(c, p, bad, now0); !errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("未快照应被拒：%v", err)
	}
	// 跨地域
	bad = base
	bad.Region = RegionUSEast1
	if _, err := PlanRollback(c, p, bad, now0); !errors.Is(err, ErrCrossRegionRollback) {
		t.Fatalf("跨地域回滚应被拒：%v", err)
	}
	// 非 T1
	bad = base
	bad.Tier = "T2"
	if _, err := PlanRollback(c, p, bad, now0); !errors.Is(err, ErrNotT1) {
		t.Fatalf("非 T1 应被拒：%v", err)
	}
}

// TestVerifyRollbackAudit_AuditIsAppendOnly G12「回滚不动审计」。
func TestVerifyRollbackAudit_AuditIsAppendOnly(t *testing.T) {
	if err := VerifyRollbackAudit(100, 100); err != nil {
		t.Fatalf("行数不变应通过：%v", err)
	}
	if err := VerifyRollbackAudit(100, 101); err != nil {
		t.Fatalf("行数增加应通过：%v", err)
	}
	if err := VerifyRollbackAudit(100, 99); !errors.Is(err, ErrAuditTouched) {
		t.Fatalf("行数减少应被拒：%v", err)
	}
}

// ───────────────────────────── 时间工具 ─────────────────────────────

func TestMonthHelpers(t *testing.T) {
	if PeriodOf(now0) != "2026-10" {
		t.Fatalf("PeriodOf=%s", PeriodOf(now0))
	}
	if PrevDataMonth(now0) != "2026-09" {
		t.Fatalf("PrevDataMonth=%s", PrevDataMonth(now0))
	}
	if got := NextPeriodStart(now0); !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextPeriodStart=%s", got)
	}
	// 1 号：上月 = 前一个自然月
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if PrevDataMonth(first) != "2026-09" {
		t.Fatalf("1 号的上月=%s", PrevDataMonth(first))
	}
}

// TestRegionValid 地域白名单：拼错的地域必须被拒。
func TestRegionValid(t *testing.T) {
	if !RegionAPSoutheast1.Valid() || !RegionUSEast1.Valid() {
		t.Fatal("合法地域被判非法")
	}
	for _, bad := range []Region{"", "sg", "ap-southeast-2", "US-EAST-1", " ap-southeast-1"} {
		if bad.Valid() {
			t.Fatalf("非法地域 %q 被判合法", bad)
		}
	}
	if RegionAPSoutheast1.Business() != "东南亚" || RegionUSEast1.Business() != "美区" {
		t.Fatal("业务范围映射错误")
	}
}

// 确保 gate 包确实被链路调用（编译期锚点，防未来重构把 gate 摘掉）。
var _ = gate.CheckMonthlyQuota
