// Package store —— 控制面（M-ADMIN）的 Postgres 实现（admin.Store）。
//
// 纪律：
//   * 所有写操作走**参数化 SQL**；
//   * 槽/算法/桶状态变更只改注册表，**不**触碰业务事实表；
//   * region 字段保留（分地域审计/存储，G12）。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/admin"
	"github.com/miccchwang/spark-cicada/backend/internal/tenant"
)

// Admin 是 admin.Store 的 Postgres 实现。
type Admin struct {
	pool *pgxpool.Pool
	// tc 绑定租户（Task #57）。nil ⇒ 平台级审计（tenant_id = NULL）。
	//
	// ★ 平台治理操作（管槽/管账号/管算法）本质是**平台级**的，
	//   默认 nil 是正确语义：平台审计不应被任一租户看到。
	//   一旦本实例用于租户内操作，必须用 ForTenant 取绑定实例。
	tc *tenant.TenantContext
}

// NewAdmin 用已连接池构造（平台级）。
func NewAdmin(pool *pgxpool.Pool) *Admin { return &Admin{pool: pool} }

// ForTenant 返回绑定到 tc 的**新**实例（不修改接收者，避免串租户）。
func (a *Admin) ForTenant(tc tenant.TenantContext) *Admin {
	if a == nil {
		return nil
	}
	return &Admin{pool: a.pool, tc: &tc}
}

// ───────────────────────────── 槽 ─────────────────────────────

// UpsertSlot 插入或更新槽（幂等，用于声明式同步）。
func (a *Admin) UpsertSlot(ctx context.Context, s admin.Slot) error {
	_, err := a.pool.Exec(ctx, `
		INSERT INTO registry_slot
		    (id, name, source_kind, source_ref, key_strategy, coverage_gate, freshness, permission, status, notes, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT (id) DO UPDATE SET
		    name          = EXCLUDED.name,
		    source_kind   = EXCLUDED.source_kind,
		    source_ref    = EXCLUDED.source_ref,
		    key_strategy  = EXCLUDED.key_strategy,
		    coverage_gate = EXCLUDED.coverage_gate,
		    freshness     = EXCLUDED.freshness,
		    permission    = EXCLUDED.permission,
		    status        = EXCLUDED.status,
		    notes         = EXCLUDED.notes,
		    updated_at    = now()`,
		s.ID, s.Name, s.SourceKind, s.SourceRef, s.KeyStrategy,
		s.CoverageGate, s.Freshness, s.Permission, s.Status, nullable(s.Notes))
	if err != nil {
		return fmt.Errorf("store: upsert 槽 %s 失败: %w", s.ID, err)
	}
	return nil
}

// GetSlot 读取单个槽。
func (a *Admin) GetSlot(ctx context.Context, id string) (admin.Slot, bool, error) {
	var s admin.Slot
	var notes *string
	err := a.pool.QueryRow(ctx, `
		SELECT id, name, source_kind, source_ref, key_strategy, coverage_gate,
		       freshness, permission, status, notes, updated_at
		  FROM registry_slot WHERE id = $1`, id).
		Scan(&s.ID, &s.Name, &s.SourceKind, &s.SourceRef, &s.KeyStrategy,
			&s.CoverageGate, &s.Freshness, &s.Permission, &s.Status, &notes, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.Slot{}, false, nil
	}
	if err != nil {
		return admin.Slot{}, false, fmt.Errorf("store: 读槽 %s 失败: %w", id, err)
	}
	if notes != nil {
		s.Notes = *notes
	}
	return s, true, nil
}

// ListSlots 列出全部槽。
func (a *Admin) ListSlots(ctx context.Context) ([]admin.Slot, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT id, name, source_kind, source_ref, key_strategy, coverage_gate,
		       freshness, permission, status, COALESCE(notes,''), updated_at
		  FROM registry_slot ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: 列槽失败: %w", err)
	}
	defer rows.Close()
	var out []admin.Slot
	for rows.Next() {
		var s admin.Slot
		if err := rows.Scan(&s.ID, &s.Name, &s.SourceKind, &s.SourceRef, &s.KeyStrategy,
			&s.CoverageGate, &s.Freshness, &s.Permission, &s.Status, &s.Notes, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ───────────────────────────── 算法 ─────────────────────────────

// UpsertAlgorithm 插入或更新算法。
func (a *Admin) UpsertAlgorithm(ctx context.Context, al admin.Algorithm) error {
	_, err := a.pool.Exec(ctx, `
		INSERT INTO registry_algorithm
		    (id, name, version, formula, unit, permission, depends_on_slots,
		     writes_bucket, missing_policy, trace, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT (id) DO UPDATE SET
		    name             = EXCLUDED.name,
		    version          = EXCLUDED.version,
		    formula          = EXCLUDED.formula,
		    unit             = EXCLUDED.unit,
		    permission       = EXCLUDED.permission,
		    depends_on_slots = EXCLUDED.depends_on_slots,
		    writes_bucket    = EXCLUDED.writes_bucket,
		    missing_policy   = EXCLUDED.missing_policy,
		    trace            = EXCLUDED.trace,
		    updated_at       = now()`,
		al.ID, al.Name, al.Version, al.Formula, al.Unit, al.Permission,
		al.DependsOnSlots, nullable(al.WritesBucket), al.MissingPolicy, al.Trace)
	if err != nil {
		return fmt.Errorf("store: upsert 算法 %s 失败: %w", al.ID, err)
	}
	return nil
}

// GetAlgorithm 读取单个算法。
func (a *Admin) GetAlgorithm(ctx context.Context, id string) (admin.Algorithm, bool, error) {
	var al admin.Algorithm
	var writes *string
	err := a.pool.QueryRow(ctx, `
		SELECT id, name, version, formula, unit, permission, depends_on_slots,
		       writes_bucket, missing_policy, trace, updated_at
		  FROM registry_algorithm WHERE id = $1`, id).
		Scan(&al.ID, &al.Name, &al.Version, &al.Formula, &al.Unit, &al.Permission,
			&al.DependsOnSlots, &writes, &al.MissingPolicy, &al.Trace, &al.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.Algorithm{}, false, nil
	}
	if err != nil {
		return admin.Algorithm{}, false, fmt.Errorf("store: 读算法 %s 失败: %w", id, err)
	}
	if writes != nil {
		al.WritesBucket = *writes
	}
	return al, true, nil
}

// ListAlgorithms 列出全部算法。
func (a *Admin) ListAlgorithms(ctx context.Context) ([]admin.Algorithm, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT id, name, version, formula, unit, permission, depends_on_slots,
		       COALESCE(writes_bucket,''), missing_policy, trace, updated_at
		  FROM registry_algorithm ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: 列算法失败: %w", err)
	}
	defer rows.Close()
	var out []admin.Algorithm
	for rows.Next() {
		var al admin.Algorithm
		if err := rows.Scan(&al.ID, &al.Name, &al.Version, &al.Formula, &al.Unit,
			&al.Permission, &al.DependsOnSlots, &al.WritesBucket, &al.MissingPolicy,
			&al.Trace, &al.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, al)
	}
	return out, rows.Err()
}

// ───────────────────────────── 规则集 ─────────────────────────────

// UpsertRuleSet 插入或更新规则集（含版本）。
func (a *Admin) UpsertRuleSet(ctx context.Context, r admin.RuleSet) error {
	scope, err := json.Marshal(r.Scope)
	if err != nil {
		return fmt.Errorf("store: 序列化 scope 失败: %w", err)
	}
	items, err := json.Marshal(r.Items)
	if err != nil {
		return fmt.Errorf("store: 序列化 items 失败: %w", err)
	}
	_, err = a.pool.Exec(ctx, `
		INSERT INTO registry_rule_set (id, version, scope, items, updated_at)
		VALUES ($1,$2,$3,$4, now())
		ON CONFLICT (id, version) DO UPDATE SET
		    scope = EXCLUDED.scope, items = EXCLUDED.items, updated_at = now()`,
		r.ID, r.Version, scope, items)
	if err != nil {
		return fmt.Errorf("store: upsert 规则集 %s@%d 失败: %w", r.ID, r.Version, err)
	}
	return nil
}

// ───────────────────────────── 桶版本 / STALE ─────────────────────────────

// BucketAlgoVersions 读桶记录的算法版本（G6-1 漂移检测输入）。
func (a *Admin) BucketAlgoVersions(ctx context.Context, bucket string) (map[string]int, error) {
	var raw map[string]int
	err := a.pool.QueryRow(ctx,
		`SELECT algo_versions FROM registry_bucket WHERE id = $1`, bucket).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		// 未注册桶 ⇒ 视为「未知」，由调用方决定 fail-closed
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读桶 %s 算法版本失败: %w", bucket, err)
	}
	if raw == nil {
		raw = map[string]int{}
	}
	return raw, nil
}

// MarkBucketStale 把桶标记为 STALE（待重算）。
func (a *Admin) MarkBucketStale(ctx context.Context, bucket, reason string) error {
	tag, err := a.pool.Exec(ctx, `
		UPDATE registry_bucket
		   SET state = 'STALE',
		       updated_at = now()
		 WHERE id = $1`, bucket)
	if err != nil {
		return fmt.Errorf("store: 标记桶 %s STALE 失败: %w", bucket, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: 桶 %s 未注册，无法标记 STALE", bucket)
	}
	return nil
}

// ───────────────────────────── 迁移辅助 ─────────────────────────────

// SeedBucket 注册一个桶（供迁移/种子使用）。
func (a *Admin) SeedBucket(ctx context.Context, id string, grain, producedBy []string,
	refresh string, algoVers, ruleVers map[string]int, state string) error {

	g, _ := json.Marshal(grain)
	p, _ := json.Marshal(producedBy)
	av, _ := json.Marshal(algoVers)
	rv, _ := json.Marshal(ruleVers)
	_, err := a.pool.Exec(ctx, `
		INSERT INTO registry_bucket (id, grain, produced_by, refresh, algo_versions, rule_versions, state, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now())
		ON CONFLICT (id) DO UPDATE SET
		    grain = EXCLUDED.grain, produced_by = EXCLUDED.produced_by,
		    refresh = EXCLUDED.refresh, algo_versions = EXCLUDED.algo_versions,
		    rule_versions = EXCLUDED.rule_versions, state = EXCLUDED.state,
		    updated_at = now()`,
		id, g, p, refresh, av, rv, state)
	if err != nil {
		return fmt.Errorf("store: 注册桶 %s 失败: %w", id, err)
	}
	return nil
}

// InsertAudit 写审计（append-only；触发器保证不可改）。
func (a *Admin) InsertAudit(ctx context.Context, actor, action, target string,
	detail map[string]any, requestID, region string) error {

	d, err := json.Marshal(detail)
	if err != nil {
		d = []byte(`{}`)
	}
	// ★ 带 tenant_id（Task #57）：audit_log 在 0011 后开了 RLS，
	//   不带租户的行在租户档下会被 WITH CHECK 策略拒绝。
	var tid any
	if a.tc != nil {
		tid = a.tc.TenantID
	}
	const q = `INSERT INTO audit_log (actor, action, target, detail, request_id, region, tenant_id)
	           VALUES ($1,$2,$3,$4,$5,$6,$7)`
	args := []any{actor, action, nullable(target), d, nullable(requestID), nullable(region), tid}
	if a.tc == nil {
		if _, err := a.pool.Exec(ctx, q, args...); err != nil {
			return fmt.Errorf("store: 写审计失败: %w", err)
		}
		return nil
	}
	// 租户档：先 SET LOCAL app.tenant_id 再插入，否则策略拒绝。
	if err := tenant.InTenantTx(ctx, a.pool, *a.tc, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, q, args...)
		return e
	}); err != nil {
		return fmt.Errorf("store: 写审计失败: %w", err)
	}
	return nil
}

// AuditSink 返回一个 admin.AuditFunc，绑定到本 Postgres 实例。
func (a *Admin) AuditSink() admin.AuditFunc {
	return func(ctx context.Context, actor, action, target string, detail map[string]any) error {
		return a.InsertAudit(ctx, actor, action, target, detail, "", "")
	}
}

// nullable 把空串转 NULL（避免审计里出现无意义的空字符串）。
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Ping 连通性探针（启动自检用）。
func (a *Admin) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return a.pool.Ping(ctx)
}
