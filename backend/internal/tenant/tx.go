// Package tenant —— 租户上下文到数据库会话的施加（连接池安全的唯一方式）。
//
// ══════════════════════════════════════════════════════════════════════════
// ★★ 本文件解决的是「连接池 + 多租户」这个组合里最经典的事故：
//
//	Postgres 的 SET（会话级）会**粘在连接上**。而连接池会把同一条连接
//	复用给下一个请求。于是：
//
//	  请求 A（租户甲）: SET app.tenant_id = 甲
//	  ... 请求结束，连接归还池中（变量还留着 甲）...
//	  请求 B（租户乙）: 忘了 SET（或走了不设的代码路径）
//	                  → 但它拿到的连接上 app.tenant_id 仍然是 甲
//	                  → 乙读到甲的数据
//
//	这个 bug 的可怕之处：**它不报错**。它只在「B 恰好复用到 A 的连接」
//	时出现 —— 也就是在高并发下**偶发**。本地单线程测试永远测不出来。
//
//	唯一可靠的解法（本文件采用）：
//	  **租户状态的生命周期 = 事务的生命周期**。
//	  用 SET LOCAL（set_config(..., true)）：事务一结束自动失效，
//	  不存在「归还到池里还带着别人的租户」这个状态。
//	  并且**同一事务内**完成设置与查询 —— 两者必须同连接。
//
// ══════════════════════════════════════════════════════════════════════════
package tenant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TxRunner 在租户已施加的事务内执行 fn 的辅助签名。
type TxRunner = func(ctx context.Context, tx pgx.Tx) error

// InTenantTx 在**已施加租户约束**的事务内执行 fn。
//
// 行为（按档位）：
//   - shared    : set_config('app.tenant_id', <租户ID>, true) —— RLS 策略据此过滤。
//   - dedicated : set_config('search_path', <t_xxx>, true)    —— 物理隔离。
//
// ★ 两者都必须在**同一事务内**执行，且必须在 fn 之前：
//
//	若分成两个事务（先设置、再开事务查询），SET LOCAL 会在第一个事务
//	结束时失效 —— 查询就落回 public schema（即别人的数据），
//	而且**不报错**（public 里表是存在的）。这是最隐蔽的越权。
//
// ★ dedicated 档**同时也**设置 app.tenant_id：
//
//	虽然该档主要靠 search_path，但若某张表恰好是共享的（如平台表），
//	RLS 仍需 tenant_id 才能正确过滤。少设一次就是一次潜在泄漏。
//	代价是一次 set_config —— 可以忽略。
//
// ★ 模式选择上的纪律：本函数**不**暴露「档位」给 fn。
//
//	fn 里不该写 `if 我是独立档 { 走 A } else { 走 B }` —— 那会让两档
//	的代码路径分叉，独立档迟早漏掉后续新增的修复（见 0010 迁移的铁律）。
//	fn 只管写业务 SQL，隔离由本函数统一负责。
func InTenantTx(ctx context.Context, pool *pgxpool.Pool, tc TenantContext, fn TxRunner) error {
	if pool == nil {
		return fmt.Errorf("tenant: 连接池为空（fail-closed）")
	}
	// ★ 上下文自检：宁可在入口拒绝，也不要带着半残上下文进查询。
	//   真实风险：若 TenantID 为空，set_config 会设成空串，
	//   而 RLS 策略里的 current_setting(...)::uuid 会**报错**
	//   （看似安全），但若策略用了容错读法就会静默放行。
	//   在入口拦掉，把「不安全」变成「不可能构造」。
	if err := validateContext(tc); err != nil {
		return err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenant: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applyTenant(ctx, tx, tc); err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: 提交事务失败: %w", err)
	}
	return nil
}

// validateContext 校验租户上下文是否可用于查询。
//
// ★ 这是「不变量在入口」的实践：与其在每个查询里防御，不如在这里拒绝。
func validateContext(tc TenantContext) error {
	if !IsUUID(tc.TenantID) {
		return fmt.Errorf("tenant: 租户 ID 不是合法 uuid：%q", tc.TenantID)
	}
	if tc.RLSTenantID != tc.TenantID {
		// RLS 输入与租户身份不一致 ⇒ 拒绝。
		// 这种不一致只可能来自构造错误，而放行的后果是「按 A 过滤但以为是 B」。
		return fmt.Errorf("tenant: RLS 租户（%q）与租户身份（%q）不一致",
			tc.RLSTenantID, tc.TenantID)
	}
	if !tc.Tier.Valid() {
		return fmt.Errorf("tenant: 未知隔离档位：%q", tc.Tier)
	}
	switch tc.Tier {
	case TierDedicated:
		if tc.SchemaName == nil {
			return fmt.Errorf("tenant: 独立档缺少 schema 名（fail-closed）")
		}
		if _, err := SanitizeSchema(*tc.SchemaName); err != nil {
			return err
		}
	case TierShared:
		if tc.SchemaName != nil && *tc.SchemaName != "" {
			return fmt.Errorf("tenant: 共享档不应带 schema 名：%q", *tc.SchemaName)
		}
	}
	return nil
}

// applyTenant 把租户约束施加到事务上（SET LOCAL 语义）。
func applyTenant(ctx context.Context, tx pgx.Tx, tc TenantContext) error {
	// ① 恒设 app.tenant_id（两档都要 —— 见 InTenantTx 的说明）
	if _, err := tx.Exec(ctx, SetLocalTenantSQL, tc.RLSTenantID); err != nil {
		return fmt.Errorf("tenant: 设置 app.tenant_id 失败: %w", err)
	}
	// ② 独立档追加 search_path
	if tc.Tier == TierDedicated {
		// SanitizeSchema 已在 validateContext 里验过；此处再取一次值
		schema := ""
		if tc.SchemaName != nil {
			schema = *tc.SchemaName
		}
		// ★ 用参数化传值（set_config 第二参数是 text，可以参数化）——
		//   避免把 schema 名拼进 SQL。白名单 + 参数化，双保险。
		if _, err := tx.Exec(ctx, SearchPathSQL, schema); err != nil {
			return fmt.Errorf("tenant: 设置 search_path 失败: %w", err)
		}
	}
	return nil
}

// QueryRowInTenant 是 InTenantTx 的便捷封装：在租户事务里跑一条查询并把
// 唯一一行扫描到 dest。
//
// ★ 存在的理由：绝大多数读路径只需要「查一行」。若每个调用点都手写
//
//	Begin/apply/Query/Commit，迟早有人漏掉 apply —— 那正是本文件要消灭的
//	错误类别。提供一个**不可能漏掉 apply** 的入口，比写一百条注释有用。
func QueryRowInTenant(ctx context.Context, pool *pgxpool.Pool, tc TenantContext,
	sql string, args []any, dest ...any) error {
	return InTenantTx(ctx, pool, tc, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(dest...)
	})
}

// QueryInTenant 是 InTenantTx 的便捷封装：在租户事务里跑一条查询并收集所有行。
//
// scanFn 对每一行调用一次，负责 Scan 到调用方的切片里。
func QueryInTenant(ctx context.Context, pool *pgxpool.Pool, tc TenantContext,
	sql string, args []any, scanFn func(rows pgx.Rows) error) error {
	return InTenantTx(ctx, pool, tc, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scanFn(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// ExecInTenant 是 InTenantTx 的便捷封装：在租户事务里执行一条写语句。
func ExecInTenant(ctx context.Context, pool *pgxpool.Pool, tc TenantContext,
	sql string, args ...any) error {
	return InTenantTx(ctx, pool, tc, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}
