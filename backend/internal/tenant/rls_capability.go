package tenant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ══════════════════════════════════════════════════════════════════════════
// ★★ 本文件源自一次**真实的、差点上线的严重缺陷**，务必读懂再改。
//
// 事故经过（本地便携 Postgres 实测复现）：
//   我们建了 RLS 策略 + ENABLE + **FORCE** ROW LEVEL SECURITY，
//   插了两个租户的行，然后以应用账号查询 —— 结果**看到了全部行**。
//   策略看起来完全正确，FORCE 也加了，但 RLS 一点作用都没有。
//
// 根因：
//   本地集群的 `spark` 角色是 **SUPERUSER**（且 rolbypassrls=true）。
//   PostgreSQL 的规则是：
//
//     ★ 超级用户与带 BYPASSRLS 的角色**无条件绕过** RLS。
//       `FORCE ROW LEVEL SECURITY` **对它们无效** ——
//       FORCE 只解决「表拥有者绕过」，不解决「超级用户绕过」。
//
//   于是「隔离已启用」的所有迹象（策略存在、ENABLE、FORCE 都在）
//   全部为真，而实际过滤掉 0 行。这是最坏的一类缺陷：
//   **它不报错，且所有检查都显示正常。**
//
// 为什么必须写成代码（而不是「注意一下」）：
//   生产环境用超级用户连库太常见了（图方便、托管平台默认账号就是）。
//   一旦如此，share 档的 RLS 隔离**从未生效**，而没人会发现 ——
//   直到某天有客户在报表里看到别人的数字。
//   所以本文件提供一个**可执行的启动自检**，把那句「注意一下」
//   变成一条会失败的断言。
// ══════════════════════════════════════════════════════════════════════════

// RoleSafety 描述当前连接角色的 RLS 相关属性。
type RoleSafety struct {
	RoleName      string
	IsSuper       bool
	BypassesRLS   bool
	CanUseRLS     bool
	IsTableOwner  bool
	RowSecurityOn bool
}

// DescribeRole 读取当前连接角色的 RLS 能力。
func DescribeRole(ctx context.Context, pool *pgxpool.Pool) (RoleSafety, error) {
	var s RoleSafety
	err := pool.QueryRow(ctx, `
		SELECT current_user,
		       COALESCE(rolsuper, false),
		       COALESCE(rolbypassrls, false)
		  FROM pg_roles
		 WHERE rolname = current_user`).Scan(&s.RoleName, &s.IsSuper, &s.BypassesRLS)
	if err != nil {
		return RoleSafety{}, fmt.Errorf("tenant: 读取角色属性失败: %w", err)
	}
	// 只要不是超级用户、且没有 BYPASSRLS，RLS 就会生效
	s.CanUseRLS = !s.IsSuper && !s.BypassesRLS
	return s, nil
}

// ErrRLSBypassed 表示当前角色会绕过 RLS —— **共享档隔离将形同虚设**。
//
// ★ 这是一个**必须拒绝启动**（或必须拒绝服务共享档查询）的条件，
//
//	而不是一条警告。因为放行的后果是跨租户数据泄漏，
//	且没有任何运行时错误会提示这件事。
var ErrRLSBypassed = fmt.Errorf(
	"tenant: 当前数据库角色是超级用户或带 BYPASSRLS —— RLS 会被无条件绕过，" +
		"共享档隔离形同虚设（FORCE ROW LEVEL SECURITY 对此无效）。" +
		"请改用非超级用户、无 BYPASSRLS 的应用角色连接")

// AssertRLSCapable 自检当前连接能否真正使用 RLS。
//
// 返回 ErrRLSBypassed ⇒ 调用方**不得**以共享档提供多租户服务。
//
// ★ 用法建议：
//   - 启动时调用一次：若失败，拒绝启动（或明确降级并记高优告警）。
//   - ★ 更关键：**共享档查询入口**也应调用（或依赖启动自检的结果）。
//     启动自检只能保证「启动那一刻」是安全的 ——
//     若有人事后改组角色，启动自检不会知道。
//     因此建议把结果缓存为进程级布尔值，并在探活接口暴露。
func AssertRLSCapable(ctx context.Context, pool *pgxpool.Pool) error {
	s, err := DescribeRole(ctx, pool)
	if err != nil {
		return err
	}
	if !s.CanUseRLS {
		return fmt.Errorf("%w（当前角色：%s，superuser=%v，bypassrls=%v）",
			ErrRLSBypassed, s.RoleName, s.IsSuper, s.BypassesRLS)
	}
	return nil
}

// CanRLS 返回 (是否可用 RLS, 角色描述)。不返回错误 —— 供探活接口安全地展示。
func CanRLS(ctx context.Context, pool *pgxpool.Pool) (bool, RoleSafety) {
	s, err := DescribeRole(ctx, pool)
	if err != nil {
		// 读不到角色属性 ⇒ 视为**不可用**（fail-closed）：
		// 「不知道能不能隔离」不能当成「能」。
		return false, RoleSafety{}
	}
	return s.CanUseRLS, s
}

// RLSAuditSQL 是一段可直接执行的审计 SQL，用于运维核对
// 「所有该开 RLS 的表是否都开了，且策略是否指向统一入口」。
//
// ★ 保留为常量而不是文档里的代码块：运维脚本可以直接引用它，
//
//	避免「文档写的是 v1、脚本跑的是 v2」这类漂移。
//
// 返回列：table_name / rls_enabled / rls_forced / policy_count / uses_unified_fn
const RLSAuditSQL = `
SELECT
    c.relname                                        AS table_name,
    c.relrowsecurity                                 AS rls_enabled,
    c.relforcerowsecurity                            AS rls_forced,
    (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid) AS policy_count,
    COALESCE(
        bool_and(pg_get_expr(p.polqual, p.polrelid) LIKE '%rls_tenant_id%'),
        false
    )                                                AS uses_unified_fn
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_policy p ON p.polrelid = c.oid
WHERE n.nspname = 'public'
  AND c.relkind = 'r'
  AND c.relname IN (
      'fact_sales_daily','bucket_pnl_month','fact_entitlement',
      'fact_permission_request','dim_view_template','dim_view_template_share',
      'dim_group','dim_group_member','dim_user_group','audit_log'
  )
GROUP BY c.relname, c.relrowsecurity, c.relforcerowsecurity, c.oid
ORDER BY c.relname`
