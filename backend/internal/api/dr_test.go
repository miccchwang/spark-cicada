package api

// dr_test.go —— M-DR（备份下载 / 主备切换 / 回滚）HTTP 层的**链路级**断言。
//
// ★ 本文件的重点是两条**接口层纪律**，而非状态码本身：
//   ① 高风险操作（隔离 / 见证 / 切换 / 回滚）的 Tier 一律由**服务端**判定填充，
//      绝不接受客户端声明 —— 否则「仅 T1 可发起」退化为「谁都能声称自己是 T1」；
//   ② 切换必须**先隔离、再取见证、最后提升**，跳过任一步必被拦下。
//
// ★ 每条用例都从 `Routes` 注册的真实路由进。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/dr"
)

// ───────────────────────────── 夹具 ─────────────────────────────

func drTestNow() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }

// stubSigner 固定返回一个**不含凭据**的签名 URL。
type stubSigner struct {
	url string
	err error
}

func (s stubSigner) SignOnce(_ context.Context, _ dr.Archive, _ string,
	_ time.Duration, _ time.Time) (string, error) {
	return s.url, s.err
}

// newDownloadService 造一个真装配的下载服务（内存配额 + 桩签名 + 无审计）。
func newDownloadService(signer dr.URLSigner) (*dr.DownloadService, *dr.MemQuotaStore) {
	store := dr.NewMemQuotaStore()
	return &dr.DownloadService{Store: store, Signer: signer, Limit: 1}, store
}

// newFailover 造一个真装配的切换控制器（进程内集群 + 见证者 + LSN 账本）。
func newFailover() *dr.FailoverController {
	cluster := &dr.Cluster{
		Region:  dr.RegionAPSoutheast1,
		Primary: &dr.Site{Region: dr.RegionAPSoutheast1, Role: dr.RolePrimary},
		Standby: &dr.Site{Region: dr.RegionAPSoutheast1, Role: dr.RoleStandby, ReadOnly: true},
	}
	return dr.NewFailoverController(cluster, dr.NewMemWitness(), dr.NewMemLSNLedger(), nil)
}

func drRequest(t *testing.T, h *DrHandlers, path, actor, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if actor != "" {
		req.Header.Set("X-Spark-Account", actor)
	}
	if tenant != "" {
		req.Header.Set(tenantHeader, tenant)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ───────────────────────────── 备份下载（G12 配额） ─────────────────────────────

func TestDrDownload_MissingIdentityIs401(t *testing.T) {
	svc, _ := newDownloadService(stubSigner{url: "https://backup.example/x"})
	h := &DrHandlers{Downloads: svc, Now: drTestNow}
	rec := drRequest(t, h, "/api/dr/download", "", "", `{"region":"ap-southeast-1","archiveId":"a1"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无身份应 401，实际 %d", rec.Code)
	}
}

func TestDrDownload_InvalidRegionIs400(t *testing.T) {
	svc, _ := newDownloadService(stubSigner{url: "https://backup.example/x"})
	h := &DrHandlers{Downloads: svc, Now: drTestNow}
	rec := drRequest(t, h, "/api/dr/download", "ops.sea", "", `{"region":"eu-west-9","archiveId":"a1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法地域应 400，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

func TestDrDownload_ServiceAbsentIs503(t *testing.T) {
	h := &DrHandlers{Now: drTestNow}
	rec := drRequest(t, h, "/api/dr/download", "ops.sea", "", `{"region":"ap-southeast-1","archiveId":"a1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配服务应 503，实际 %d", rec.Code)
	}
}

// ★ 链路：首次放行 → 同月第二次被拒（G12 配额闸门**真的**在生效）。
func TestDrDownload_QuotaChain_SecondCallRejected(t *testing.T) {
	svc, store := newDownloadService(stubSigner{url: "https://backup.example/x?exp=1&sig=ab"})
	store.PutArchive(dr.Archive{
		ID: "2026-09", Region: dr.RegionAPSoutheast1, DataMonth: "2026-09",
		GeneratedAt: drTestNow().Add(-24 * time.Hour), SHA256: "deadbeef",
	})
	h := &DrHandlers{Downloads: svc, Now: drTestNow}

	rec := drRequest(t, h, "/api/dr/download", "ops.sea", "",
		`{"region":"ap-southeast-1","archiveId":"2026-09"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("首次应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var first struct {
		Allowed   bool   `json:"allowed"`
		SignedURL string `json:"signedUrl"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if !first.Allowed || first.SignedURL == "" {
		t.Fatalf("首次应放行并下发签名 URL，实际 %+v", first)
	}

	rec2 := drRequest(t, h, "/api/dr/download", "ops.sea", "",
		`{"region":"ap-southeast-1","archiveId":"2026-09"}`)
	var second struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
		Detail  string `json:"detail"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &second)
	if second.Allowed {
		t.Fatalf("同月第二次应被拒（G12 配额）")
	}
	if second.Reason != dr.ReasonQuotaExceeded {
		t.Fatalf("拒绝原因应为 %s，实际 %q", dr.ReasonQuotaExceeded, second.Reason)
	}
	// ★ 拒绝必须携带闸门给出的**人类可读原因** —— 否则「闸门被摘掉」不可检出。
	if strings.TrimSpace(second.Detail) == "" {
		t.Fatalf("拒绝必须携带闸门给出的人类可读原因（detail 不得为空）")
	}
}

// ★ docs/09 铁律：签名 URL 含凭据形态必须被拒（G12）。
func TestDrDownload_CredentialedURLRejected(t *testing.T) {
	svc, store := newDownloadService(stubSigner{url: "https://user:pass@backup.example/x"})
	store.PutArchive(dr.Archive{
		ID: "2026-09", Region: dr.RegionAPSoutheast1, DataMonth: "2026-09",
		GeneratedAt: drTestNow().Add(-24 * time.Hour), SHA256: "deadbeef",
	})
	h := &DrHandlers{Downloads: svc, Now: drTestNow}
	rec := drRequest(t, h, "/api/dr/download", "ops.sea", "",
		`{"region":"ap-southeast-1","archiveId":"2026-09"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("含凭据 URL 应被拒（500），实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

// ───────────────────────────── 切换（F13=B） ─────────────────────────────

func TestDrFailover_NonT1Is403(t *testing.T) {
	h := &DrHandlers{
		Failover: newFailover(),
		IsT1:     func(tenantID, account string) bool { return account == "ceo" },
		Now:      drTestNow,
	}
	rec := drRequest(t, h, "/api/dr/failover/promote", "ops.sea", "",
		`{"region":"ap-southeast-1","reason":"主库宕机"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非 T1 应 403，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

// ★★ 最关键的一条：客户端**不能**通过请求体声称自己是 T1。
func TestDrFailover_ClientDeclaredTierIsIgnored(t *testing.T) {
	h := &DrHandlers{
		Failover: newFailover(),
		IsT1:     func(tenantID, account string) bool { return false }, // 服务端判定：谁都还不是 T1
		Now:      drTestNow,
	}
	// 请求体里塞 tier=T1 —— 必须被忽略。
	rec := drRequest(t, h, "/api/dr/failover/promote", "ops.sea", "",
		`{"region":"ap-southeast-1","reason":"x","tier":"T1","operator":"ceo"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("客户端声明 tier 必须被忽略（应 403），实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

// ★ IsT1 为 nil 时一律拒绝（fail-closed）—— 绝不「没有门禁就当作放行」。
func TestDrFailover_NilGateIsFailClosed(t *testing.T) {
	h := &DrHandlers{Failover: newFailover(), Now: drTestNow}
	rec := drRequest(t, h, "/api/dr/failover/promote", "ceo", "",
		`{"region":"ap-southeast-1","reason":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无门禁应 fail-closed 403，实际 %d", rec.Code)
	}
}

// ★ 链路：必须先 fence、再 witness、最后 promote；跳过任一步必被拦下。
func TestDrFailover_FenceThenWitnessThenPromote(t *testing.T) {
	h := &DrHandlers{
		Failover: newFailover(),
		IsT1:     func(tenantID, account string) bool { return account == "ceo" },
		Now:      drTestNow,
	}
	body := `{"region":"ap-southeast-1","reason":"主库宕机"}`

	// ① 未隔离就提升 ⇒ 必拒（G12「切换必先 fencing」）。
	rec := drRequest(t, h, "/api/dr/failover/promote", "ceo", "", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("未隔离即提升应被拒，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "隔离") {
		t.Fatalf("拒绝原因应指出未隔离，实际 %q", rec.Body.String())
	}

	// ② 隔离后仍未取见证 ⇒ 仍必拒。
	if rec = drRequest(t, h, "/api/dr/failover/fence", "ceo", "", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("隔离应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	rec = drRequest(t, h, "/api/dr/failover/promote", "ceo", "", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("未取见证即提升应被拒，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}

	// ③ 取见证 + 对账后 ⇒ 放行。
	if rec = drRequest(t, h, "/api/dr/failover/witness", "ceo", "", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("取见证应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	rec = drRequest(t, h, "/api/dr/failover/promote", "ceo", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("fence+witness 后提升应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var out struct {
		Operator   string `json:"Operator"`
		NewPrimary string `json:"NewPrimary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应非 JSON：%v（%s）", err, rec.Body.String())
	}
	if out.Operator != "ceo" {
		t.Fatalf("操作人应取自请求头 ceo，实际 %q", out.Operator)
	}
}

func TestDrFailover_PromoteRequiresReason(t *testing.T) {
	h := &DrHandlers{
		Failover: newFailover(),
		IsT1:     func(tenantID, account string) bool { return true },
		Now:      drTestNow,
	}
	rec := drRequest(t, h, "/api/dr/failover/promote", "ceo", "",
		`{"region":"ap-southeast-1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 reason 应 400，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

// ───────────────────────────── 回滚计划（B2） ─────────────────────────────

func TestDrRollbackPlan_RequiresT1AndReturnsExcludedTables(t *testing.T) {
	h := &DrHandlers{
		Failover: newFailover(),
		IsT1:     func(tenantID, account string) bool { return account == "ceo" },
		Now:      drTestNow,
	}
	// 非 T1 ⇒ 403
	rec := drRequest(t, h, "/api/dr/rollback/plan", "ops.sea", "",
		`{"region":"ap-southeast-1","target":"2026-09-01T00:00:00Z","confirmed":true,"snapshotTaken":true}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非 T1 应 403，实际 %d", rec.Code)
	}
	// T1 ⇒ 通过，且排除清单必须含审计表。
	rec = drRequest(t, h, "/api/dr/rollback/plan", "ceo", "",
		`{"region":"ap-southeast-1","target":"2026-09-01T00:00:00Z","confirmed":true,"snapshotTaken":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("T1 回滚计划应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "audit_log") {
		t.Fatalf("排除清单必须含审计表，实际 %q", rec.Body.String())
	}
}
