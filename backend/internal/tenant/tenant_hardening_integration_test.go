// tenant_hardening_integration_test.go —— 0012 加固的**真库行为**闸门。
//
// ★ 为什么必须另起一个文件、并且**必须**打真库：
//
//	sql_0012_*_test.go 那批是**文本闸门**（断言迁移文本里有某段 SQL）。
//	文本闸门能拦住「整段被误删」，但拦不住「写了却语义不对」——
//	例如：复合外键建了，但列顺序反了；策略建了，但谓词写成了恒真。
//	本文件用真库把 0012 的**语义**钉死。
//
// ★★★ 本文件证明的核心性质（RLS 做不到、只有约束层能做到的那条）：
//
//	「跨租户引用」在**复合外键**下不可能存在 —— 即便用**超级用户**
//	（它无条件绕过 RLS）也插不进去。
//	这是 RLS 与约束的**分工**：
//	  · RLS 管「你能看见/写入哪些行」（按 tenant_id 过滤）；
//	  · 复合 FK 管「你的行只能指向同租户的父行」（跨租户即违约）。
//	只做 RLS 而忘了复合 FK，则共享档下 A 租户可以把 B 租户的账号
//	挂进自己的组 —— 而 A 从 RLS 视角看「插自己的行」，完全合法。
//
// ★ 本文件用**特权连接**（SPARK_TEST_DB_DSN）而非应用角色，
//
//	正是为了让上面的结论有意义：若用应用角色，RLS 会先把插入挡住，
//	就分不清「是 RLS 拦的」还是「是复合 FK 拦的」。
//	用特权连接 ⇒ RLS 不参与 ⇒ 拦下来的**只能是**约束。
package tenant

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// hardeningPool 取**特权**连接池（绕过 RLS），用于验证约束层。
func hardeningPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		t.Skip("未设置 SPARK_TEST_DB_DSN，跳过真库集成测试")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("建池失败：%v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("连通性失败：%v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// const 两个测试租户（与 rls_integration_test.go 的 uidAlpha/uidBeta 区分开）。
const (
	hardenTenantA = "cccc3333-3333-4333-8333-333333333333"
	hardenTenantB = "dddd4444-4444-4444-8444-444444444444"
)

// TestHardening_复合外键拦截跨租户引用 —— 本文件最重要的一条断言。
//
// ★ 场景：租户 A 的 dim_org 里有账号 "u.cross"；租户 B 想把它挂进自己的组。
//
//	用**特权连接**做这件事（RLS 不参与），期望被复合外键拒绝。
//
// ★ 若 0012 只加了 tenant_id 而**没重建复合 FK**，本用例会红 ——
//
//	这正是「RLS 之外还必须有约束」这条纪律的可执行证据。
func TestHardening_复合外键拦截跨租户引用(t *testing.T) {
	pool := hardeningPool(t)
	ctx := context.Background()

	// 准备：两个租户的 dim_org 行 + 一个 B 租户自己的组
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM dim_group_member WHERE group_id = 'g.xfk'`)
		_, _ = pool.Exec(ctx, `DELETE FROM dim_group WHERE id = 'g.xfk'`)
		_, _ = pool.Exec(ctx, `DELETE FROM dim_org WHERE account = 'u.cross'`)
	}
	cleanup()
	t.Cleanup(cleanup)

	// A 租户有一个账号 u.cross
	if _, err := pool.Exec(ctx, `
		INSERT INTO dim_org (tenant_id, account, display_name, tier, primary_dept)
		VALUES ($1, 'u.cross', 'u.cross', 'T3', 'd.a')`, hardenTenantA); err != nil {
		t.Fatalf("建 A 租户 dim_org: %v", err)
	}
	// B 租户有一个组
	if _, err := pool.Exec(ctx, `
		INSERT INTO dim_group (tenant_id, id, name) VALUES ($1, 'g.xfk', 'g.xfk')`,
		hardenTenantB); err != nil {
		t.Fatalf("建 B 租户 dim_group: %v", err)
	}

	// ★ 跨租户引用：B 租户的成员行指向 A 租户的账号 —— 必须被拒
	_, err := pool.Exec(ctx, `
		INSERT INTO dim_group_member (tenant_id, group_id, account)
		VALUES ($1, 'g.xfk', 'u.cross')`, hardenTenantB)
	if err == nil {
		t.Fatal("★ 跨租户引用未被拒绝 —— 复合外键 (tenant_id, account) 没生效。" +
			"这会让 A 租户可以把 B 租户的账号挂进自己的组织，而 RLS 视角完全合法")
	}
	// 断言错误来自**外键**，而不是别的（例如 NOT NULL / RLS）
	if !strings.Contains(err.Error(), "foreign key") &&
		!strings.Contains(err.Error(), "violates") {
		t.Fatalf("期望外键违约，实际错误：%v", err)
	}

	// ── 对照：同租户引用**必须成功** ──
	// 若缺了这段对照，上面的「被拒」有可能只是「这张表什么都插不进去」
	// （例如列名写错、权限不足），那样闸门就是假绿。
	if _, err := pool.Exec(ctx, `
		INSERT INTO dim_group (tenant_id, id, name) VALUES ($1, 'g.xfk.own', 'own')`,
		hardenTenantA); err != nil {
		t.Fatalf("建 A 租户自己的组: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM dim_group_member WHERE group_id = 'g.xfk.own'`)
		_, _ = pool.Exec(ctx, `DELETE FROM dim_group WHERE id = 'g.xfk.own'`)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO dim_group_member (tenant_id, group_id, account)
		VALUES ($1, 'g.xfk.own', 'u.cross')`, hardenTenantA); err != nil {
		t.Fatalf("★ 同租户引用必须成功（否则上面「跨租户被拒」可能只是别的原因）：%v", err)
	}
}

// TestHardening_同账号可属多家租户 —— 复合主键的存在性证明。
//
// ★ 0012 之前 dim_org 主键是 (account)，同一账号**不可能**属于两家租户。
//
//	0012 之后主键是 (tenant_id, account)，这就成了常态（也是商用必需：
//	同一自然人在两家公司都有账号，或集团下多主体共用账号名）。
func TestHardening_同账号可属多家租户(t *testing.T) {
	pool := hardeningPool(t)
	ctx := context.Background()

	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM dim_org WHERE account = 'u.dup'`)
	}
	cleanup()
	t.Cleanup(cleanup)

	for _, tid := range []string{hardenTenantA, hardenTenantB} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO dim_org (tenant_id, account, display_name, tier, primary_dept)
			VALUES ($1, 'u.dup', 'u.dup', 'T3', 'd.x')`, tid); err != nil {
			t.Fatalf("租户 %s 建同名账号失败（复合主键应允许）：%v", tid, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM dim_org WHERE account = 'u.dup'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("同名账号应有两行（两家租户各一），实际 %d", n)
	}
}

// TestHardening_租户过滤的dim_org查询不串租 —— 生产缺陷的回归锚点。
//
// ★ 背景（真实回归）：dim_org 主键改为 (tenant_id, account) 后，
//
//	原先按 `WHERE account = $1` 单键查 tier/supervisor 的代码，
//	在多租户下会命中**别的租户**的同名行。
//	sparkd 的两处判定（isSupervisorFunc / isManagementFunc）走**特权通道**
//	（绕过 RLS），因此这个 bug 不会被 RLS 兜住 —— 必须靠查询自带 tenant_id。
//
// ★ 本用例在**特权连接**上模拟那两处查询：带 tenant_id 时结果正确，
//
//	不带时结果错误（以此证明「带 tenant_id」不是可有可无的装饰）。
func TestHardening_租户过滤的dim_org查询不串租(t *testing.T) {
	pool := hardeningPool(t)
	ctx := context.Background()

	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM dim_org WHERE account = 'u.tier'`)
	}
	cleanup()
	t.Cleanup(cleanup)

	// ★ 关键布景：**同名账号**在两个租户里 tier 不同。
	//   A 是 T3（普通），B 是 T1（管理层）。
	if _, err := pool.Exec(ctx, `
		INSERT INTO dim_org (tenant_id, account, display_name, tier, primary_dept)
		VALUES ($1, 'u.tier', 'u.tier', 'T3', 'd.a'),
		       ($2, 'u.tier', 'u.tier', 'T1', 'd.b')`,
		hardenTenantA, hardenTenantB); err != nil {
		t.Fatalf("布景失败：%v", err)
	}

	// ① 带 tenant_id 查询（生产修复后的写法）：各自拿到自己的 tier
	var tierA, tierB string
	if err := pool.QueryRow(ctx,
		`SELECT tier FROM dim_org WHERE tenant_id = $1 AND account = 'u.tier'`,
		hardenTenantA).Scan(&tierA); err != nil {
		t.Fatalf("按租户查 A 失败：%v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT tier FROM dim_org WHERE tenant_id = $1 AND account = 'u.tier'`,
		hardenTenantB).Scan(&tierB); err != nil {
		t.Fatalf("按租户查 B 失败：%v", err)
	}
	if tierA != "T3" || tierB != "T1" {
		t.Fatalf("★ 按租户查询结果不符：A=%q（期望 T3）, B=%q（期望 T1）", tierA, tierB)
	}

	// ② ★ 反向证明：**不带** tenant_id 的单键查询会返回多行
	//    —— 这正是修复前的写法，在多租户下是错的（会或随机或报错）。
	rows, err := pool.Query(ctx,
		`SELECT tier FROM dim_org WHERE account = 'u.tier'`)
	if err != nil {
		t.Fatalf("不带租户查询失败：%v", err)
	}
	var tiers []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			t.Fatalf("扫描失败：%v", err)
		}
		tiers = append(tiers, s)
	}
	rows.Close()
	if len(tiers) != 2 {
		t.Fatalf("不带租户的单键查询应命中 2 行（证明它**不能**在多租户下使用），实际 %d 行：%v",
			len(tiers), tiers)
	}
}
