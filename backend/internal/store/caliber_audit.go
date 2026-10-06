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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/pnl"
	"github.com/miccchwang/spark-cicada/backend/internal/tenant"
)

// CaliberAuditStore 把口径切换写入审计。
type CaliberAuditStore struct {
	pool *pgxpool.Pool
	// tc 绑定租户（Task #57）。nil ⇒ 平台级（写 tenant_id = NULL）。
	//
	// ★ 为什么用字段 + 构造期绑定（而不是给 RecordCaliberChange 加参数）：
	//   与 templatestore.Store 同一理由 —— 参数可空 ⇒ 漏传不报错 ⇒ 静默越权。
	//   绑在实例上，「有没有租户」在构造时就定了。
	//
	// ★ 为什么 nil 允许存在（不像 templatestore 那样强制二选一）：
	//   口径审计还可能由**平台级**任务触发（预计算重算、批量回填），
	//   那些没有租户上下文。此时写 NULL ⇒ 该行对所有租户不可见（RLS），
	//   这正是"平台级审计"应得的可见性。
	tc *tenant.TenantContext
}

// NewCaliberAuditStore 构造（平台级）。
func NewCaliberAuditStore(pool *pgxpool.Pool) *CaliberAuditStore {
	return &CaliberAuditStore{pool: pool}
}

// NewCaliberAuditStoreForTenant 构造**绑定租户**的口径审计。
func NewCaliberAuditStoreForTenant(pool *pgxpool.Pool, tc tenant.TenantContext) *CaliberAuditStore {
	return &CaliberAuditStore{pool: pool, tc: &tc}
}

// ForTenant 返回一个绑定到 tc 的**新**实例（不修改接收者）。
//
// ★ 返回新实例而非就地改字段：接收者可能被多个 goroutine 共享，
//   就地改字段就是数据竞争 + 串租户。这是本类型唯一的"换租户"方式。
func (s *CaliberAuditStore) ForTenant(tc tenant.TenantContext) *CaliberAuditStore {
	if s == nil {
		return nil
	}
	return &CaliberAuditStore{pool: s.pool, tc: &tc}
}

// tenantIDOrNil 返回 tenant_id 列的值（与 templatestore 同名帮手语义一致）。
func (s *CaliberAuditStore) tenantIDOrNil() any {
	if s.tc == nil {
		return nil
	}
	return s.tc.TenantID
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

	if err := s.record(c); err != nil {
		return err
	}
	return nil
}

// record 在（可能的）租户事务内写审计行。
//
// ★ 租户档下**必须**在同一事务里先 SET LOCAL app.tenant_id 再插入：
//
//	audit_log 在 0011 后开了 RLS 且策略是 WITH CHECK —— 不留租户变量直接
//	INSERT，租户档下会被策略拒绝（或落到哨兵）。平台档则走裸池。
func (s *CaliberAuditStore) record(c pnl.CaliberChange) error {
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

	const q = `INSERT INTO audit_log (actor, action, target, detail, tenant_id)
	           VALUES ($1, $2, $3, $4, $5)`
	args := []any{c.Account, pnl.AuditActionCaliberChange, c.Module, detail, s.tenantIDOrNil()}

	// ★ 用 context.Background()（沿用原实现）：口径审计是"已发生的业务事实"，
	//   若跟随请求 ctx 被取消，就会出现"用户看到口径切了、但审计没落"。
	//   审计宁可晚一点、不可丢。
	ctx := context.Background()
	if s.tc == nil {
		if _, err := s.pool.Exec(ctx, q, args...); err != nil {
			return fmt.Errorf("store: 记口径审计: %w", err)
		}
		return nil
	}
	err = tenant.InTenantTx(ctx, s.pool, *s.tc, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, q, args...)
		return e
	})
	if err != nil {
		return fmt.Errorf("store: 记口径审计: %w", err)
	}
	return nil
}
