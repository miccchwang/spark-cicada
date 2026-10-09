// dr.go —— M-DR（备份下载 / 主备切换 / 回滚）的 HTTP 接口层。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本文件补的是 G12 四条判定函数的**生产调用点**。
//
//	`CheckMonthlyQuota` / `CheckFailoverFencing` / `CheckRollbackKeepsAudit`
//	此前全仓没有任何非测试调用点 —— 唯一「调用」它们的就是它们自己的测试。
//	`internal/dr` 把链路实现了，但若没有 HTTP 入口，判定函数仍然只在
//	测试里被调到 = 仍然恒真。本文件是那条「真实入口」。
//
// ══════════════════════════════════════════════════════════════════════════
// 三条纪律：
//
//  1. **Tier 与 Operator 一律由服务端判定填充，绝不接受客户端声明。**
//     这是 F8 轮的教训（「闸门测的是入参，不是链路」）：若把请求体里的
//     `tier` 直接透传给 `dr.PromoteRequest`，那么任何客户端只要写
//     `"tier":"T1"` 就能发起切换 —— 「仅 T1 可发起」立刻退化为
//     「谁都能声称自己是 T1」。故 `Tier` 只在接口层判定通过后**写死为 "T1"**，
//     `Operator` 只取自请求头。
//
//  2. **高风险操作（切换 / 回滚）仅 T1**，且必须能追到人。
//
//  3. **降级不撒谎**：服务未装配时返回 503，绝不挂一个假实现返回空结果。
//
// ══════════════════════════════════════════════════════════════════════════
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/dr"
)

// Downloader 备份下载（月度配额）能力 —— `*dr.DownloadService` 实现。
type Downloader interface {
	RequestDownload(ctx context.Context, account string, region dr.Region,
		archiveID string, now time.Time) (dr.DownloadResult, error)
}

// FailoverController 主备切换 / 回滚计划能力 —— `*dr.FailoverController` 实现。
type FailoverController interface {
	Promote(ctx context.Context, req dr.PromoteRequest, now time.Time) (dr.PromoteOutcome, error)
	RollbackPlan(ctx context.Context, req dr.RollbackRequest, now time.Time) ([]string, error)
	// Fence 隔离旧主（写通道阻断）。
	Fence(ctx context.Context) error
	// AcquireWitness 取得见证者租约 + 标记 LSN 已对账。
	AcquireWitness(ctx context.Context) error
}

// DrHandlers 备份下载 / 切换 / 回滚的 HTTP 处理器。
type DrHandlers struct {
	Downloads Downloader
	Failover  FailoverController
	// IsT1 高风险操作（切换 / 回滚）的门禁：按 (租户, 账号) 判定。
	// nil ⇒ 一律拒绝（fail-closed）。
	//
	// ★ 必须带 tenantID：判据走特权连接（绕过 RLS），不带租户会让
	//   「A 租户的 T1」命中「B 租户的同名 T1 行」（0012 之后账号不再全局唯一）。
	IsT1 func(tenantID, account string) bool
	// Now 可注入时钟；nil ⇒ time.Now().UTC()。
	Now func() time.Time
}

// Routes 注册 M-DR 路由。
func (h *DrHandlers) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/dr/download", h.handleDownload)
	mux.HandleFunc("POST /api/dr/failover/fence", h.handleFence)
	mux.HandleFunc("POST /api/dr/failover/witness", h.handleWitness)
	mux.HandleFunc("POST /api/dr/failover/promote", h.handlePromote)
	mux.HandleFunc("POST /api/dr/rollback/plan", h.handleRollbackPlan)
}

func (h *DrHandlers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now().UTC()
}

// ───────────────────────────── 备份下载（B3） ─────────────────────────────

type downloadBody struct {
	Region    string `json:"region"`
	ArchiveID string `json:"archiveId"`
}

// downloadView 响应体（字段名对齐 contracts/backup-quota.ts 的
// BackupArchive + DownloadCheckResult）。
type downloadView struct {
	Allowed         bool   `json:"allowed"`
	Reason          string `json:"reason,omitempty"`
	Detail          string `json:"detail,omitempty"`
	SignedURL       string `json:"signedUrl,omitempty"`
	URLExpiresAt    string `json:"urlExpiresAt,omitempty"`
	NextAvailableAt string `json:"nextAvailableAt,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	Audited         bool   `json:"audited"`
	Warn            string `json:"warn,omitempty"`
}

func (h *DrHandlers) handleDownload(w http.ResponseWriter, r *http.Request) {
	account := r.Header.Get("X-Spark-Account")
	if account == "" {
		http.Error(w, "missing identity", http.StatusUnauthorized)
		return
	}
	if h.Downloads == nil {
		http.Error(w, "download unavailable", http.StatusServiceUnavailable)
		return
	}
	var in downloadBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid download request: "+err.Error(), http.StatusBadRequest)
		return
	}
	region := dr.Region(strings.TrimSpace(in.Region))
	if !region.Valid() {
		http.Error(w, "invalid region（仅 ap-southeast-1 / us-east-1）", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.ArchiveID) == "" {
		http.Error(w, "missing archiveId", http.StatusBadRequest)
		return
	}

	res, err := h.Downloads.RequestDownload(r.Context(), account, region, in.ArchiveID, h.now())
	if err != nil {
		http.Error(w, "download failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// ★ 拒绝是**业务结论**（配额用尽 / 归档未生成），不是 HTTP 错误 ⇒ 200 + allowed=false。
	out := downloadView{
		Allowed: res.Allowed, Reason: res.Reason, Detail: res.Detail,
		SHA256: res.SHA256, Audited: res.Audited, Warn: res.Warn,
	}
	if !res.URLExpiresAt.IsZero() {
		out.URLExpiresAt = res.URLExpiresAt.Format(time.RFC3339)
	}
	if !res.NextAvailableAt.IsZero() {
		out.NextAvailableAt = res.NextAvailableAt.Format(time.RFC3339)
	}
	if res.Allowed {
		out.SignedURL = res.SignedURL
	}
	writeJSON(w, out)
}

// ───────────────────────────── 主备切换（F13=B） ─────────────────────────────

// handleFence 隔离旧主（切换前的**独立**动作）。
func (h *DrHandlers) handleFence(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireT1(w, r); !ok {
		return
	}
	if h.Failover == nil {
		http.Error(w, "failover unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := h.Failover.Fence(r.Context()); err != nil {
		http.Error(w, "fence failed: "+err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"fenced": true, "note": "旧主写通道已阻断"})
}

// handleWitness 取得见证者租约 + LSN 对账（切换前的**独立**动作）。
func (h *DrHandlers) handleWitness(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireT1(w, r); !ok {
		return
	}
	if h.Failover == nil {
		http.Error(w, "failover unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := h.Failover.AcquireWitness(r.Context()); err != nil {
		http.Error(w, "witness failed: "+err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"witnessAcquired": true, "lsnReconciled": true})
}

type promoteBody struct {
	Region string `json:"region"`
	Reason string `json:"reason"`
	// ★ 刻意**没有** tier / operator 字段：二者由服务端判定填充。
}

func (h *DrHandlers) handlePromote(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requireT1(w, r)
	if !ok {
		return
	}
	if h.Failover == nil {
		http.Error(w, "failover unavailable", http.StatusServiceUnavailable)
		return
	}
	var in promoteBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid promote request: "+err.Error(), http.StatusBadRequest)
		return
	}
	region := dr.Region(strings.TrimSpace(in.Region))
	if !region.Valid() {
		http.Error(w, "invalid region（仅 ap-southeast-1 / us-east-1）", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.Reason) == "" {
		http.Error(w, "切换必须说明原因（reason 不得为空）", http.StatusBadRequest)
		return
	}

	// ★ Tier/Operator 一律由服务端填充：见文件头纪律 1。
	req := dr.PromoteRequest{
		Region:   region,
		Operator: actor,
		Tier:     "T1",
		Reason:   strings.TrimSpace(in.Reason),
	}
	out, err := h.Failover.Promote(r.Context(), req, h.now())
	if err != nil {
		http.Error(w, "promote failed: "+err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, out)
}

// ───────────────────────────── 回滚计划（B2） ─────────────────────────────

type rollbackBody struct {
	Region        string `json:"region"`
	Target        string `json:"target"`
	Confirmed     bool   `json:"confirmed"`
	SnapshotTaken bool   `json:"snapshotTaken"`
	// ★ 同样没有 operator / tier 字段。
}

func (h *DrHandlers) handleRollbackPlan(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requireT1(w, r)
	if !ok {
		return
	}
	if h.Failover == nil {
		http.Error(w, "rollback unavailable", http.StatusServiceUnavailable)
		return
	}
	var in rollbackBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid rollback request: "+err.Error(), http.StatusBadRequest)
		return
	}
	region := dr.Region(strings.TrimSpace(in.Region))
	if !region.Valid() {
		http.Error(w, "invalid region（仅 ap-southeast-1 / us-east-1）", http.StatusBadRequest)
		return
	}
	target, err := time.Parse(time.RFC3339, in.Target)
	if err != nil {
		http.Error(w, "target 必须是 RFC3339 时间："+err.Error(), http.StatusBadRequest)
		return
	}

	req := dr.RollbackRequest{
		Region:        region,
		Target:        target,
		Operator:      actor,
		Tier:          "T1",
		Confirmed:     in.Confirmed,
		SnapshotTaken: in.SnapshotTaken,
	}
	tables, err := h.Failover.RollbackPlan(r.Context(), req, h.now())
	if err != nil {
		http.Error(w, "rollback plan rejected: "+err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{
		"excludedTables": tables,
		"note":           "仅前置校验通过；执行回滚需运维编排（停写→快照→回放）",
	})
}

// requireT1 高风险操作门禁：身份取自请求头，且必须是 T1。
//
// ★ IsT1 为 nil 时**一律拒绝**（fail-closed）—— 绝不「没有门禁就当作放行」。
func (h *DrHandlers) requireT1(w http.ResponseWriter, r *http.Request) (string, bool) {
	actor := r.Header.Get("X-Spark-Account")
	if actor == "" {
		http.Error(w, "missing identity", http.StatusUnauthorized)
		return "", false
	}
	if h.IsT1 == nil || !h.IsT1(r.Header.Get(tenantHeader), actor) {
		http.Error(w, "forbidden: 高风险操作仅 T1 可发起", http.StatusForbidden)
		return "", false
	}
	return actor, true
}
