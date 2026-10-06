// caliber_audit.go —— 口径切换的审计落库（M-PNL × M-AUDIT 的接缝）。
//
// 职责：把 pnl.CaliberChange 落进 0001 的 append-only audit_log。
//
// ★ 为什么口径切换值得进审计（它看起来只是个「展示选项」）：
//
//	口径决定了**同一份数据被读成「巨亏」还是「微利」**。
//	KODP 实测里，同一份数据口径 A 显示净贡献 −152,567.19、
//	口径 B 显示 +123.17 —— 符号都反了，而两者都没错。
//
//	当两个部门拿着互相矛盾的截图争吵时，唯一能厘清的就是这条记录：
//	「谁、在什么时候、用哪个模块的哪个口径、数据出是哪天」。
//	记录成本极低，省掉的扯皮极多。
//
// ★ 纪律：本文件不实现任何数值计算，也不改 audit_log 的 append-only 语义
//
//	（0001 的触发器会拦截任何 UPDATE/DELETE，这里只 INSERT）。
package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/pnl"
)

// CaliberAuditStore 把口径切换写入审计。
type CaliberAuditStore struct {
	pool *pgxpool.Pool
}

// NewCaliberAuditStore 构造。
func NewCaliberAuditStore(pool *pgxpool.Pool) *CaliberAuditStore {
	return &CaliberAuditStore{pool: pool}
}

// RecordCaliberChange 落一条口径切换审计。
//
// ★ detail 里保留**原始请求值**（Requested）与**来源**（Source）：
//
//	用户从「?caliber=ZZZ」被回退到默认口径时，只看 from/to 无法解释
//	为什么会切；带上原始值与来源，排障时一眼就能看出是脏 URL。
//
// ★ 不落任何密钥/凭据（docs/09）：本接口只写口径与模块，无敏感明文。
func (s *CaliberAuditStore) RecordCaliberChange(c pnl.CaliberChange) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("store: 口径审计不可用（连接池未初始化）")
	}
	if err := c.Validate(); err != nil {
		// 非法记录不落库：脏数据进 append-only 表就再也删不掉了。
		return err
	}

	detail, err := json.Marshal(map[string]any{
		"module":    c.Module,
		"from":      string(c.From),
		"to":        string(c.To),
		"source":    c.Source,
		"dirty":     c.Dirty,
		"requested": c.Requested,
		"summary":   c.ChangeSummary(),
	})
	if err != nil {
		return fmt.Errorf("store: 序列化口径审计明细: %w", err)
	}

	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO audit_log (actor, action, target, detail)
		VALUES ($1, $2, $3, $4)`,
		c.Account, pnl.AuditActionCaliberChange, c.Module, detail); err != nil {
		return fmt.Errorf("store: 记口径审计: %w", err)
	}
	return nil
}
