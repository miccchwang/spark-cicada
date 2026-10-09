// dr_wiring.go —— M-DR（备份下载 / 主备切换 / 回滚）的运行时装配。
//
// ★ 本文件把 `internal/dr` 的判定链路接到 HTTP 入口上，使
//
//	`gate.CheckMonthlyQuota` / `CheckNoCredentialsInURL` / `CheckFailoverFencing`
//	/ `CheckRollbackKeepsAudit` 四条判定函数获得**真实生产调用点**。
//
// ★ 诚实标注（不粉饰，务必与代码一起演进）：
//
//  1. **下载配额存储**当前用进程内实现（`dr.NewMemQuotaStore`）——
//     重启即清零。生产须替换为 Postgres 实现（`ON CONFLICT ... WHERE used < limit`
//     保证原子），见 docs/06 F18。故当前「每月 1 次」在**跨重启**意义上不成立。
//
//  2. **见证者 / LSN 账本**当前用进程内桩，须显式置位（fence/witness 两步），
//     真实部署须接第三可用区见证者与真实 LSN 对账源（docs/11 F13）。
//
//  3. **站点角色**当前由进程内集群对象持有，尚无真实站点探针接入。
//
//     ⇒ 本层让**链路真的跑起来**（判定函数不再恒真），但**不声称生产可切**。
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/dr"
	"github.com/miccchwang/spark-cicada/backend/internal/tenant"
)

// buildDownloadService 装配备份下载服务（配额 + 签名 + 审计）。
func (p *dataPlane) buildDownloadService() *dr.DownloadService {
	svc := &dr.DownloadService{
		Store:  dr.NewMemQuotaStore(),
		Signer: newURLSigner(),
		Limit:  dr.DefaultQuotaLimit,
	}
	// 审计：接 append-only 审计（G10）。库未就绪时 Audit 为 nil，
	// 此时 DownloadResult.Audited=false 会被如实回报 —— 不假装留痕。
	if p.admin != nil && p.admin.Audit != nil {
		auditFn := p.admin.Audit
		svc.Audit = func(ctx context.Context, rec dr.AuditRecord) error {
			return auditFn(ctx, rec.Account, "dr.backup.download", rec.ArchiveID, map[string]any{
				"region":    string(rec.Region),
				"dataMonth": rec.DataMonth,
				"sha256":    rec.SHA256,
				"outcome":   rec.Outcome,
			})
		}
	}
	return svc
}

// buildFailoverController 装配主备切换控制器（进程内集群状态）。
//
// ★ 默认主备各一站点：新加坡地域（东南亚业务）。切换只允许在**本地域**内发生，
//
//	地域之间互不接管（docs/04 §6.6）。
func (p *dataPlane) buildFailoverController() *dr.FailoverController {
	cluster := &dr.Cluster{
		Region:  dr.RegionAPSoutheast1,
		Primary: &dr.Site{Region: dr.RegionAPSoutheast1, Role: dr.RolePrimary},
		Standby: &dr.Site{Region: dr.RegionAPSoutheast1, Role: dr.RoleStandby, ReadOnly: true},
	}
	var auditSink dr.FailoverAudit
	if p.admin != nil && p.admin.Audit != nil {
		auditFn := p.admin.Audit
		auditSink = func(ctx context.Context, rec dr.FailoverRecord) error {
			return auditFn(ctx, rec.Operator, "dr.failover.promote", string(rec.Region), map[string]any{
				"reason":     rec.Reason,
				"oldPrimary": string(rec.OldPrimary),
				"newPrimary": string(rec.NewPrimary),
				"fenced":     rec.Fenced,
			})
		}
	}
	return dr.NewFailoverController(cluster, dr.NewMemWitness(), dr.NewMemLSNLedger(), auditSink)
}

// isT1Func 返回「某账号是否 T1」的判定函数。
//
// ★ 用途：M-DR 高风险操作（隔离 / 见证 / 切换 / 回滚）的门禁。
//
// ★ 为什么不能只信请求体里的 tier：`dr.Promote` 内部校验的是
// `PromoteRequest.Tier` 这个**入参**。若接口层把客户端传来的 tier 原样透传，
// 那么任何客户端写 `"tier":"T1"` 就能发起切换 —— 「仅 T1 可发起」退化为
// 「谁都能声称自己是 T1」（F8 轮的同型教训）。故接口层必须用自己的判据，
// 判定通过后才把 Tier 写死为 "T1"。
//
// ★ 判据（按优先级，与 isManagementFunc 同源纪律）：
//  1. 组织链路里 tier == "T1"（数据面就绪时查 dim_org，**必须带 tenant_id**）。
//  2. 显式启用 m.admin 模块（平台治理者，为将来放权留的显式开关）。
//
// ★ 库未就绪时**保守返回 false**：宁可切不了，不可在无判据时放行。
func (p *dataPlane) isT1Func() func(tenantID, account string) bool {
	return func(tenantID, account string) bool {
		if account == "" {
			return false
		}
		// ① 组织层级 T1
		if p.dbReady && p.pool != nil && tenant.IsUUID(tenantID) {
			// ★ 走特权 pool（绕过 RLS）⇒ 必须带 tenant_id 过滤，否则
			//   「A 租户的 T1」会命中「B 租户的同名 T1 行」（0012 之后
			//   dim_org 账号不再全局唯一）。
			var tier *string
			err := p.pool.QueryRow(context.Background(),
				`SELECT tier FROM dim_org WHERE tenant_id = $1 AND account = $2`,
				tenantID, account).Scan(&tier)
			if err == nil && tier != nil && *tier == "T1" {
				return true
			}
		}
		// ② 显式启用 m.admin 模块
		if p.resolver != nil && p.ents != nil {
			if view := resolveFor(p.resolver, p.ents, account); view != nil {
				for _, m := range view.Modules {
					if m == "m.admin" {
						return true
					}
				}
			}
		}
		return false
	}
}

// ───────────────────────────── 一次性签名 URL ─────────────────────────────

// hmacURLSigner 生成**不含凭据**的一次性签名 URL（docs/04 §6.4 / docs/09）。
//
// 形态：`https://backup.spark.internal/<region>/<archiveId>?exp=<unix>&sig=<hex>`
//
// ★ 为什么不用 `https://user:pass@host` 形态：那正是 docs/09 明令禁止的
//
//	「URL 携带凭据」。签名放在查询串里，且 `gate.CheckNoCredentialsInURL`
//	会在出站前复核。
//
// ★ 密钥来源：`SPARK_URL_SIGNING_KEY`。未配置时**生成进程内随机密钥**并打一行
//
//	[warn] —— 这样本地/降级模式仍可跑通链路，但重启后旧签名失效。
//	生产**必须**配置该环境变量（否则签名不可跨重启验证）。
type hmacURLSigner struct {
	key []byte
}

func newURLSigner() dr.URLSigner {
	if k := os.Getenv("SPARK_URL_SIGNING_KEY"); k != "" {
		return &hmacURLSigner{key: []byte(k)}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("[warn] 生成 URL 签名密钥失败：%v（下载签名将不可用）", err)
		return &hmacURLSigner{key: nil}
	}
	log.Printf("[warn] 未配置 SPARK_URL_SIGNING_KEY ⇒ 使用进程内随机密钥（重启后旧签名失效）")
	return &hmacURLSigner{key: buf}
}

func (s *hmacURLSigner) SignOnce(_ context.Context, a dr.Archive, account string,
	ttl time.Duration, now time.Time) (string, error) {

	if len(s.key) == 0 {
		return "", fmt.Errorf("dr: 未配置 URL 签名密钥（SPARK_URL_SIGNING_KEY）")
	}
	exp := now.Add(ttl).Unix()
	payload := string(a.Region) + "/" + a.ID + "/" + account + "/" + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))

	u := url.URL{
		Scheme: "https",
		Host:   "backup.spark.internal",
		Path:   "/" + string(a.Region) + "/" + a.ID,
		RawQuery: url.Values{
			"exp": {strconv.FormatInt(exp, 10)},
			"sig": {sig},
		}.Encode(),
	}
	return u.String(), nil
}
