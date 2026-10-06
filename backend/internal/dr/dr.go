// Package dr —— 容灾与备份下载的**真实生产链路**（G12）。
//
// 背景（第五个变种，同一个病）：
//
//	docs/05 的 G12 有四条断言，其依赖的判定函数写在 internal/gate/gate.go：
//	CheckMonthlyQuota / CheckNoCrossRegionWrite / CheckFailoverFencing /
//	CheckRollbackKeepsAudit。这四个函数**全仓没有任何非测试调用点** ——
//	唯一「调用」它们的就是它们自己的测试。
//
// 于是和 M-REQ「未批不通」「到期回收」、F8 会签发送侧、G7 代授一模一样：
//
//	没有下载路径 ⇒ 「同月第二次下载被拒」恒真；
//	没有跨地域写路径 ⇒ 「禁止跨地域写同一行」恒真；
//	没有切换路径 ⇒ 「切换必先 fencing」恒真；
//	没有回滚路径 ⇒ 「回滚不动审计」恒真。
//
// 四条**永远为真**的断言，而 docs/05 标着「✅ 已实现」。
// 一个永远为真的断言等于没有断言。
//
// 本包把 G12 的链路真正接上（纯 Go，不依赖数据库），对应 docs/04 §6：
//
//	1. 备份下载配额（§6.4 / B3）—— **分地域**、按自然月计数、配额 1 次/月；
//	2. 跨地域写护栏（§6.1.1）—— 跨地域只允许读汇总，绝不写同一行；
//	3. 主备切换（§6.6 / F13 = B 半自动）—— 提升备库前**强制** fencing；
//	4. 180 天回滚（§6.3 / B2）—— 回滚**永不触碰**审计表。
//
// ★ 关键设计：fencing 三项**从站点真实状态推导**，不接受调用方声明。
// 若 fencing 由调用方传布尔值，闸门测的就还是入参，生产路径「声明 true」
// 即可绕过 —— 「切换必先 fencing」会再次退化为恒真（F8 轮的教训）。
package dr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ───────────────────────────── 地域（B1） ─────────────────────────────

// Region 部署地域（对齐 contracts/backup-quota.ts: Region）。
type Region string

const (
	// RegionAPSoutheast1 新加坡 · 东南亚业务。
	RegionAPSoutheast1 Region = "ap-southeast-1"
	// RegionUSEast1 美国 · 美区业务。
	RegionUSEast1 Region = "us-east-1"
)

// Valid 地域是否合法。
//
// ★ 地域是分区的第一道门：一个拼错的地域名会让「分地域计数」与
// 「禁止跨地域写」同时失效（计数各写各的键、写护栏判等失败）。
// 因此非法地域一律**拒绝**，而不是「当作合法值继续」。
func (r Region) Valid() bool {
	return r == RegionAPSoutheast1 || r == RegionUSEast1
}

// Business 该地域服务的业务范围。
func (r Region) Business() string {
	switch r {
	case RegionAPSoutheast1:
		return "东南亚"
	case RegionUSEast1:
		return "美区"
	}
	return ""
}

// ───────────────────────────── 自然月工具（B3） ─────────────────────────────

// PeriodOf 自然月（YYYY-MM，UTC）。
func PeriodOf(t time.Time) string { return t.UTC().Format("2006-01") }

// NextPeriodStart 下一自然月 1 日 00:00 UTC —— 即配额重置时刻，
// 也就是拒绝下载时要告知用户的「下一次可下载时间」。
func NextPeriodStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
}

// PrevDataMonth 上一个自然月（YYYY-MM）。
//
// docs/04 §6.4：**每月 1 号**自动备份**上一个月**的数据。
func PrevDataMonth(t time.Time) string {
	u := t.UTC()
	first := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return first.AddDate(0, -1, 0).Format("2006-01")
}

// quotaKey 下载配额计数键。
//
// **地域 + 自然月** 由 gate.QuotaKey 统一（G12 的唯一键口径），
// 账号维度由本包追加 —— 配额口径是「每个账号、每个地域、每个自然月 1 次」。
//
// ★ 复用 gate.QuotaKey 而不是另起一套拼接：键一旦分叉，
// 「分地域计数」就会出现两套互不可见的计数（与 CI DSN 变量名分叉同型）。
func quotaKey(account string, region Region, period string) string {
	return gate.QuotaKey(string(region), period) + ":" + account
}

// ───────────────────────────── 备份归档（B3） ─────────────────────────────

// Archive 一份可下载的备份归档（对齐 contracts/backup-quota.ts: BackupArchive）。
type Archive struct {
	ID       string
	Region   Region
	DataMonth string // 如 2026-09
	// GeneratedAt 生成时间。**零值 = 尚未生成**（1 号定时任务还没跑）。
	GeneratedAt time.Time
	SizeBytes   int64
	SHA256      string
}

// Available 归档是否已生成且可下载（生成时间非零且不晚于 now）。
func (a Archive) Available(now time.Time) bool {
	return !a.GeneratedAt.IsZero() && !a.GeneratedAt.After(now)
}

// ───────────────────────────── 配额存储 ─────────────────────────────

// QuotaStore 下载配额与归档的持久化抽象。
type QuotaStore interface {
	// Used 返回 (account, region, period) 已用下载次数。
	Used(ctx context.Context, account string, region Region, period string) (int, error)

	// ConsumeOnce **原子地**「若未达上限则 +1」，返回 (是否占用成功, 占用后计数)。
	//
	// ★ 必须原子：check-then-act 会让两个并发请求同时读到 used=0 而都通过，
	// 于是「每月 1 次」在并发下变成 2 次。上限判定最终以本方法为准。
	ConsumeOnce(ctx context.Context, account string, region Region, period string, limit int) (bool, int, error)

	// Archive 按 ID 取归档；第二个返回值为是否存在。
	Archive(ctx context.Context, region Region, id string) (Archive, bool, error)
}

// AuditRecord 一条下载审计。
//
// docs/04 §6.4 要求留痕：**谁、何时、哪个月、哪个地域、文件校验和**。
type AuditRecord struct {
	Account   string
	Region    Region
	DataMonth string
	ArchiveID string
	SHA256    string
	At        time.Time
	Outcome   string
}

// AuditSink 审计落库回调（装配层接到 append-only 的 audit_log 实现）。
type AuditSink func(ctx context.Context, rec AuditRecord) error

// URLSigner 生成**一次性签名 URL**。
//
// docs/04 §6.4 / docs/09：短时效、**URL 中不携带任何凭据**。
type URLSigner interface {
	SignOnce(ctx context.Context, a Archive, account string, ttl time.Duration, now time.Time) (string, error)
}

// ───────────────────────────── 下载服务（B3） ─────────────────────────────

// MaxURLTTL 签名 URL 的最长有效期（docs/04 §6.4「短时效」）。
const MaxURLTTL = 15 * time.Minute

// DefaultQuotaLimit 默认月度下载上限。
const DefaultQuotaLimit = 1

// 拒绝原因（对齐 contracts/backup-quota.ts: DownloadCheckResult.reason）。
const (
	ReasonOK              = ""
	ReasonQuotaExceeded   = "QUOTA_EXCEEDED"
	ReasonNotAuthorized   = "NOT_AUTHORIZED"
	ReasonArchiveNotFound = "ARCHIVE_NOT_FOUND"
	ReasonNotYetAvailable = "NOT_YET_AVAILABLE"
)

// DownloadResult 一次下载请求的结果。
type DownloadResult struct {
	Allowed         bool
	Reason          string
	SignedURL       string
	URLExpiresAt    time.Time
	NextAvailableAt time.Time
	SHA256          string
	// Detail 人类可读的拒绝原因（Reason 是给程序看的码）。
	Detail string
	// Audited=false 表示变更已放行但**未留痕**，调用方必须显式提示。
	Audited bool
	Warn    string
}

// DownloadService 备份下载（月度配额）。
type DownloadService struct {
	Store  QuotaStore
	Signer URLSigner
	Audit  AuditSink
	// Limit 月度上限；<=0 时取 DefaultQuotaLimit。
	Limit int
	// URLTTL 签名有效期；<=0 时取 MaxURLTTL，且**不得超过** MaxURLTTL。
	URLTTL time.Duration
}

func (s *DownloadService) limit() int {
	if s == nil || s.Limit <= 0 {
		return DefaultQuotaLimit
	}
	return s.Limit
}

func (s *DownloadService) urlTTL() time.Duration {
	if s == nil || s.URLTTL <= 0 {
		return MaxURLTTL
	}
	if s.URLTTL > MaxURLTTL {
		return MaxURLTTL
	}
	return s.URLTTL
}

// RequestDownload 请求下载一份备份归档。
//
// 纪律（顺序即优先级）：
//
//	1. **无 actor 不放行**：追不到人的下载不放行（与审计纪律同源）。
//	2. **地域必须合法**：非法地域一律拒绝 —— 否则分地域计数与写护栏同时失效。
//	3. **归档必须属于本地域**：拿 A 地域的配额去下 B 地域的归档一律拒绝
//	   （docs/04 §6.1.1 地域业务分区；地域之间不互相备份）。
//	4. **归档必须已生成**：1 号定时任务未跑完 ⇒ NOT_YET_AVAILABLE，不消耗配额。
//	5. **配额**：gate.CheckMonthlyQuota 先给出人类可读的拒绝原因，
//	   再由 Store.ConsumeOnce **原子**兜底（并发下只有一个能过）。
//	6. **签名 URL**：先签名后占用配额 —— 签名失败不应白白吃掉一次配额；
//	   且 URL 必须过 gate.CheckNoCredentialsInURL（docs/09 铁律）。
//	7. **留痕**：拒绝也留痕（谁在什么时候想下哪个地域哪个月的归档）。
//
// ★ 拒绝路径**不消耗配额**；只有真正放行的那一次才计入。
func (s *DownloadService) RequestDownload(ctx context.Context, account string, region Region,
	archiveID string, now time.Time) (DownloadResult, error) {

	if s == nil || s.Store == nil {
		return DownloadResult{}, errors.New("dr: DownloadService 未装配 Store")
	}

	period := PeriodOf(now)
	next := NextPeriodStart(now)

	// 纪律 1：无 actor 不放行
	if strings.TrimSpace(account) == "" {
		return DownloadResult{Reason: ReasonNotAuthorized, NextAvailableAt: next}, nil
	}
	// 纪律 2：地域必须合法
	if !region.Valid() {
		return DownloadResult{Reason: ReasonNotAuthorized, NextAvailableAt: next}, nil
	}

	// 纪律 3：归档必须属于本地域
	a, ok, err := s.Store.Archive(ctx, region, archiveID)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("dr: 读取归档失败: %w", err)
	}
	if !ok || a.Region != region {
		return DownloadResult{Reason: ReasonArchiveNotFound, NextAvailableAt: next}, nil
	}
	// 纪律 4：归档必须已生成
	if !a.Available(now) {
		return DownloadResult{Reason: ReasonNotYetAvailable, NextAvailableAt: next}, nil
	}

	// 纪律 5：配额（先给人类可读原因）
	used, err := s.Store.Used(ctx, account, region, period)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("dr: 读取配额失败: %w", err)
	}
	if allowed, why := gate.CheckMonthlyQuota(used); !allowed {
		_ = s.record(ctx, AuditRecord{
			Account: account, Region: region, DataMonth: a.DataMonth,
			ArchiveID: a.ID, SHA256: a.SHA256, At: now, Outcome: ReasonQuotaExceeded,
		})
		return DownloadResult{
			Reason: ReasonQuotaExceeded, Detail: why,
			NextAvailableAt: next, SHA256: a.SHA256,
		}, nil
	}

	// 纪律 6：先签名（失败不消耗配额）
	if s.Signer == nil {
		return DownloadResult{}, errors.New("dr: DownloadService 未装配 Signer")
	}
	ttl := s.urlTTL()
	signed, err := s.Signer.SignOnce(ctx, a, account, ttl, now)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("dr: 签名失败: %w", err)
	}
	// docs/09 铁律：签名 URL 不得携带凭据
	if v := gate.CheckNoCredentialsInURL(signed); len(v) > 0 {
		return DownloadResult{}, fmt.Errorf("dr: 签名 URL 含凭据形态，拒绝下发（docs/09）：%s", v[0])
	}

	// 纪律 5（兜底）：原子占用
	limit := s.limit()
	consumed, after, err := s.Store.ConsumeOnce(ctx, account, region, period, limit)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("dr: 占用配额失败: %w", err)
	}
	if !consumed {
		_ = s.record(ctx, AuditRecord{
			Account: account, Region: region, DataMonth: a.DataMonth,
			ArchiveID: a.ID, SHA256: a.SHA256, At: now, Outcome: ReasonQuotaExceeded,
		})
		return DownloadResult{
			Reason: ReasonQuotaExceeded,
			Detail: fmt.Sprintf("并发占用失败：本月本域已用 %d 次（上限 %d）", after, limit),
			NextAvailableAt: next, SHA256: a.SHA256,
		}, nil
	}

	// 纪律 7：留痕（失败不阻断下载，但必须显式暴露 audited=false）
	res := DownloadResult{
		Allowed:      true,
		SignedURL:    signed,
		URLExpiresAt: now.Add(ttl),
		SHA256:       a.SHA256,
	}
	if err := s.record(ctx, AuditRecord{
		Account: account, Region: region, DataMonth: a.DataMonth,
		ArchiveID: a.ID, SHA256: a.SHA256, At: now, Outcome: "OK",
	}); err != nil {
		res.Audited = false
		res.Warn = "审计写入失败（下载已放行，配额已计入）：" + err.Error()
		return res, nil
	}
	res.Audited = true
	return res, nil
}

func (s *DownloadService) record(ctx context.Context, rec AuditRecord) error {
	if s.Audit == nil {
		return nil
	}
	return s.Audit(ctx, rec)
}

// ───────────────────────────── 跨地域写护栏（§6.1.1 / §6.6） ─────────────────────────────

// ErrCrossRegionWrite 跨地域写同一行。
var ErrCrossRegionWrite = errors.New("dr: 禁止跨地域写同一行")

// Row 一行待写入的业务数据（带**归属地域**）。
type Row struct {
	Table  string
	Key    string
	Region Region
	Value  map[string]any
}

// RowStore 行写入的下游存储。
type RowStore interface {
	Upsert(ctx context.Context, row Row) error
}

// GuardRowWrite 跨地域写护栏。
//
// 写入方地域与行归属地域不一致 ⇒ 拒绝（跨地域只允许读汇总）。
func GuardRowWrite(writerRegion, rowRegion Region) error {
	if v := gate.CheckNoCrossRegionWrite(string(writerRegion), string(rowRegion)); len(v) > 0 {
		return fmt.Errorf("%w：%s", ErrCrossRegionWrite, v[0])
	}
	return nil
}

// RowWriter 带地域护栏的行写入器。
//
// ★ 生产写入路径必须经此，否则「禁止跨地域写同一行」又变成一条没人走的断言。
type RowWriter struct {
	Store        RowStore
	WriterRegion Region
}

// Upsert 写入一行；跨地域写在此被拦下（下游 Store 根本不会被调用）。
func (w *RowWriter) Upsert(ctx context.Context, row Row) error {
	if w == nil || w.Store == nil {
		return errors.New("dr: RowWriter 未装配 Store")
	}
	if !w.WriterRegion.Valid() {
		return fmt.Errorf("dr: 写入方地域非法：%q", w.WriterRegion)
	}
	if err := GuardRowWrite(w.WriterRegion, row.Region); err != nil {
		return err
	}
	return w.Store.Upsert(ctx, row)
}
