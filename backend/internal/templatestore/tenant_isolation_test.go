// tenant_isolation_test.go —— templatestore 的**租户隔离**真库测试（Task #57）。
//
// 运行方式：
//
//	SPARK_TEST_DB_DSN='...' SPARK_REQUIRE_DB=1 go test ./internal/templatestore/ -run Tenant -v
//
// ══════════════════════════════════════════════════════════════════════════
// ★★ 本文件存在的理由：**验证 store 真的"踏上"了 RLS**。
//
//	迁移 0011 给 dim_view_template 开了 RLS 策略，但 RLS 只在
//	会话变量 app.tenant_id 被设置时才过滤。store 若走裸池（New），
//	变量停在哨兵值 ⇒ RLS 会把**所有**行过滤掉（0 行，看起来"安全"）；
//	store 若走 NewForTenant，才会在事务里施加租户。
//
//	这两种状态在**单租户**测试里完全同形（都是"查不到别人的"），
//	所以必须用**两租户**夹具把差异逼出来：
//	  · 用 NewForTenant(A) 写入的模板，NewForTenant(B) 必须看不到；
//	  · 而 **特权**通道（superuser）必须能看到全部 —— 证明数据确实在库里
//	    （否则"看不到"可能只是因为写入根本没成功）。
//
// ★ 这套"看不到 + 特权能看到"的双向断言是必须的：
//
//	只断言"B 看不到"的话，一个"写入失败"的实现也能让测试通过。
//
// ══════════════════════════════════════════════════════════════════════════
package templatestore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/template"
	"github.com/miccchwang/spark-cicada/backend/internal/tenant"
)

// tenantDSN 返回用于特权通道（superuser / BYPASSRLS）的 DSN。
//
// ★ 特权通道的用途**仅限**：从外部确认"数据确实写进了库里"（绕开 RLS 看全貌）。
//
//	隔离本身必须用**应用角色**通道验证 —— 用超级用户测 RLS 等于没测。
func tenantDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		if requireDB() {
			t.Fatal("★ 真库集成测试被要求必须运行（SPARK_REQUIRE_DB=1），但未设置 SPARK_TEST_DB_DSN。")
		}
		t.Skip("未配置 SPARK_TEST_DB_DSN —— 跳过租户隔离集成测试")
	}
	return dsn
}

// appDSN 返回**应用角色**（NOSUPERUSER NOBYPASSRLS）的 DSN。
//
// ★★★ 本文件最重要的一条纪律（真实故障模式，见 tenant/rls_integration_test.go）：
//
//	PostgreSQL 的超级用户与 BYPASSRLS 角色**无条件绕过 RLS**，
//	`FORCE ROW LEVEL SECURITY` 也拦不住（FORCE 只解决表拥有者）。
//	若用超级用户连库跑 store，共享档隔离**从未生效**，
//	而所有迹象都显示正常 —— 这是最危险的静默失效。
//
//	故隔离测试必须走应用角色；拿不到就 skip（并在 SPARK_REQUIRE_DB=1 时失败）。
func appDSN(t *testing.T) string {
	t.Helper()
	d := strings.TrimSpace(os.Getenv("SPARK_TEST_APP_DSN"))
	if d == "" {
		if requireDB() {
			t.Fatal("★ 租户隔离测试需要 SPARK_TEST_APP_DSN（非超级用户的应用角色）。\n" +
				"  用超级用户跑 RLS 测试会被无条件绕过，等于没测。")
		}
		t.Skip("未配置 SPARK_TEST_APP_DSN —— 跳过租户隔离测试（需应用角色才能验证 RLS）")
	}
	return d
}

// newAppPool 开一个**应用角色**池（受 RLS 限制），store 挂在这个池上。
func newAppPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("解析 APP DSN 失败：%v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("APP 池创建失败：%v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("APP 池 ping 失败：%v", err)
	}
	// ★ 前置断言：该角色必须真的受 RLS 约束。
	//   若这里失败，后续"隔离"断言都无意义（测的是不生效的策略）。
	if err := tenant.AssertRLSCapable(ctx, pool); err != nil {
		pool.Close()
		if requireDB() {
			t.Fatalf("★ 应用角色无法使用 RLS：%v", err)
		}
		t.Skipf("跳过租户隔离测试（连接角色不受 RLS 约束）：%v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTenantPool 开一个特权池（用于"从外部确认数据真的在库里"）。
//
// ★ 用特权通道是**刻意**的：app 角色受 RLS 限制，用它去"确认数据存在"
//
//	会得到 0 行，从而无法区分"隔离正确"与"数据没写进去"。
func newTenantPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("特权池连接失败：%v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("特权池 ping 失败：%v", err)
	}
	return pool
}

// tenantCtxFor 构造一个 shared 档租户上下文。
//
// ★ 用真实 uuid（RLS 策略要求 tenant_id 能匹配）：这里编两个固定 uuid，
//
//	测试前后清理，避免污染。
func tenantCtxFor(id string) tenant.TenantContext {
	return tenant.TenantContext{
		TenantID:    id,
		Tier:        tenant.TierShared,
		SchemaName:  nil,
		RLSTenantID: id,
	}
}

const (
	tenantA = "00000000-0000-4000-8000-00000000aaaa"
	tenantB = "00000000-0000-4000-8000-0000000000bb"
)

// cleanupTenantTemplates 删掉某租户的全部模板（特权通道）。
func cleanupTenantTemplates(ctx context.Context, pool *pgxpool.Pool, tid string) {
	_, _ = pool.Exec(ctx, `DELETE FROM dim_view_template WHERE tenant_id = $1`, tid)
	_, _ = pool.Exec(ctx, `DELETE FROM dim_view_template_share WHERE tenant_id = $1`, tid)
	_, _ = pool.Exec(ctx, `DELETE FROM audit_log WHERE action = 'view_template.apply' AND tenant_id = $1`, tid)
}

// seedOwnerOrgs 建 owner 的 dim_org 行。
//
// ★ 0012 起 dim_org 主键 = (tenant_id, account)：账号不再全局唯一，
//
//	因此**必须**显式指定每个 owner 属于哪个租户，冲突目标为 (tenant_id, account)。
//	这也正是 dim_view_template.owner 的复合外键（→ dim_org(tenant_id, account)）
//	能生效的前提 —— owner 与模板必须同租户。
func seedOwnerOrgs(ctx context.Context, pool *pgxpool.Pool, pairs ...[2]string) error {
	for _, p := range pairs {
		tid, a := p[0], p[1]
		if _, err := pool.Exec(ctx, `
			INSERT INTO dim_org (tenant_id, account, display_name, tier, primary_dept)
			VALUES ($1, $2, $2, 'T3', 'D1')
			ON CONFLICT (tenant_id, account) DO NOTHING`, tid, a); err != nil {
			return fmt.Errorf("建 dim_org %s/%s: %w", tid, a, err)
		}
	}
	return nil
}

// cleanupOwnerOrgs 按 (tenantID, account) 对清理 owner 的 dim_org 行。
// 参数为**扁平**的 tid/account 交替序列，例如 cleanupOwnerOrgs(ctx, p, tA, aA, tB, aB)。
func cleanupOwnerOrgs(ctx context.Context, pool *pgxpool.Pool, flat ...string) {
	for i := 0; i+1 < len(flat); i += 2 {
		tid, a := flat[i], flat[i+1]
		_, _ = pool.Exec(ctx, `DELETE FROM dim_view_template WHERE tenant_id=$1 AND owner=$2`, tid, a)
		_, _ = pool.Exec(ctx, `DELETE FROM dim_org WHERE tenant_id=$1 AND account=$2`, tid, a)
	}
}

// TestTenant_两租户模板互不可见 —— 本文件的核心断言。
func TestTenant_两租户模板互不可见(t *testing.T) {
	dsn := tenantDSN(t)
	app := newAppPool(t, appDSN(t))
	ctx := context.Background()
	priv := newTenantPool(t, dsn)
	defer priv.Close()

	cleanupTenantTemplates(ctx, priv, tenantA)
	cleanupTenantTemplates(ctx, priv, tenantB)
	defer cleanupTenantTemplates(ctx, priv, tenantA)
	defer cleanupTenantTemplates(ctx, priv, tenantB)

	acctA := "u.tenantA"
	acctB := "u.tenantB"
	if err := seedOwnerOrgs(ctx, priv, [2]string{tenantA, acctA}, [2]string{tenantB, acctB}); err != nil {
		t.Fatalf("%v", err)
	}
	defer cleanupOwnerOrgs(ctx, priv, tenantA, acctA, tenantB, acctB)

	sa := NewForTenant(app, tenantCtxFor(tenantA))
	sb := NewForTenant(app, tenantCtxFor(tenantB))

	ta := mkTpl("tpl.onlyA", acctA, "/report", template.ScopePersonal)
	if err := sa.Save(ctx, ta, acctA); err != nil {
		t.Fatalf("租户A 保存模板失败：%v", err)
	}
	tb := mkTpl("tpl.onlyB", acctB, "/report", template.ScopePersonal)
	if err := sb.Save(ctx, tb, acctB); err != nil {
		t.Fatalf("租户B 保存模板失败：%v", err)
	}

	// ① 各自读得到自己的
	if got, err := sa.Load(ctx, "tpl.onlyA"); err != nil || got == nil {
		t.Fatalf("租户A 应读得到自己的模板，err=%v", err)
	}
	if got, err := sb.Load(ctx, "tpl.onlyB"); err != nil || got == nil {
		t.Fatalf("租户B 应读得到自己的模板，err=%v", err)
	}

	// ② 各自读不到对方的（这就是隔离）
	if _, err := sa.Load(ctx, "tpl.onlyB"); err == nil {
		t.Fatalf("★ 租户A 读到了租户B 的模板 —— RLS 未生效（store 没施加租户？）")
	}
	if _, err := sb.Load(ctx, "tpl.onlyA"); err == nil {
		t.Fatalf("★ 租户B 读到了租户A 的模板 —— RLS 未生效")
	}

	// ③ 特权通道必须能看到两行 —— 证明数据真的写进去了（否则②的"看不到"是假象）
	var n int
	if err := priv.QueryRow(ctx,
		`SELECT count(*) FROM dim_view_template WHERE id IN ('tpl.onlyA','tpl.onlyB')`).Scan(&n); err != nil {
		t.Fatalf("特权查询失败：%v", err)
	}
	if n != 2 {
		t.Fatalf("特权通道应看到 2 行（数据确实在库里），实际 %d —— 若为 0 说明写入路径根本没落库", n)
	}

	// ④ tenant_id 必须被正确写入（不是 NULL）
	var tidA string
	if err := priv.QueryRow(ctx,
		`SELECT tenant_id::text FROM dim_view_template WHERE id = 'tpl.onlyA'`).Scan(&tidA); err != nil {
		t.Fatalf("读 tenant_id 失败：%v", err)
	}
	if tidA != tenantA {
		t.Fatalf("Save 写入的 tenant_id 应为 %s，实际 %q", tenantA, tidA)
	}
}

// TestTenant_列表也只含本租户 —— 列表接口是最容易漏租户的路径。
func TestTenant_列表也只含本租户(t *testing.T) {
	dsn := tenantDSN(t)
	app := newAppPool(t, appDSN(t))
	ctx := context.Background()
	priv := newTenantPool(t, dsn)
	defer priv.Close()

	cleanupTenantTemplates(ctx, priv, tenantA)
	cleanupTenantTemplates(ctx, priv, tenantB)
	defer cleanupTenantTemplates(ctx, priv, tenantA)
	defer cleanupTenantTemplates(ctx, priv, tenantB)

	acctA, acctB := "u.listA", "u.listB"
	if err := seedOwnerOrgs(ctx, priv, [2]string{tenantA, acctA}, [2]string{tenantB, acctB}); err != nil {
		t.Fatalf("%v", err)
	}
	defer cleanupOwnerOrgs(ctx, priv, tenantA, acctA, tenantB, acctB)

	sa := NewForTenant(app, tenantCtxFor(tenantA))
	sb := NewForTenant(app, tenantCtxFor(tenantB))

	if err := sa.Save(ctx, mkTpl("tpl.listA", acctA, "/pnl", template.ScopePersonal), acctA); err != nil {
		t.Fatalf("A 保存失败：%v", err)
	}
	if err := sb.Save(ctx, mkTpl("tpl.listB", acctB, "/pnl", template.ScopePersonal), acctB); err != nil {
		t.Fatalf("B 保存失败：%v", err)
	}

	la, err := sa.LoadByPage(ctx, "/pnl", 0)
	if err != nil {
		t.Fatalf("A 列表失败：%v", err)
	}
	lb, err := sb.LoadByPage(ctx, "/pnl", 0)
	if err != nil {
		t.Fatalf("B 列表失败：%v", err)
	}
	for _, x := range la {
		if x.ID == "tpl.listB" {
			t.Fatalf("★ 租户A 的列表里出现了租户B 的模板 —— LoadByPage 漏了租户")
		}
	}
	for _, x := range lb {
		if x.ID == "tpl.listA" {
			t.Fatalf("★ 租户B 的列表里出现了租户A 的模板 —— LoadByPage 漏了租户")
		}
	}
}

// TestTenant_跨租户写入必须被RLS拒绝 —— 伪造 tenant_id 应失败。
func TestTenant_跨租户写入必须被RLS拒绝(t *testing.T) {
	dsn := tenantDSN(t)
	app := newAppPool(t, appDSN(t))
	ctx := context.Background()
	priv := newTenantPool(t, dsn)
	defer priv.Close()

	cleanupTenantTemplates(ctx, priv, tenantA)
	cleanupTenantTemplates(ctx, priv, tenantB)
	defer cleanupTenantTemplates(ctx, priv, tenantA)
	defer cleanupTenantTemplates(ctx, priv, tenantB)

	acctA := "u.forgeA"
	if err := seedOwnerOrgs(ctx, priv, [2]string{tenantA, acctA}); err != nil {
		t.Fatalf("%v", err)
	}
	defer cleanupOwnerOrgs(ctx, priv, tenantA, acctA)

	sa := NewForTenant(app, tenantCtxFor(tenantA))
	if err := sa.Save(ctx, mkTpl("tpl.forge", acctA, "/report", template.ScopePersonal), acctA); err != nil {
		t.Fatalf("A 保存失败：%v", err)
	}

	// 用 B 的会话把 A 的行的 tenant_id 改成 B —— RLS 的 USING 会先把该行
	// 过滤掉（B 看不见它），故 UPDATE 影响 0 行；若策略是 WITH CHECK-only
	// 或未开 RLS，则可能改成功。两种情况都必须"改不到"。
	sb := NewForTenant(app, tenantCtxFor(tenantB))
	var affected int64
	err := sb.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, e := tx.Exec(ctx,
			`UPDATE dim_view_template SET tenant_id = $2 WHERE id = $1`, "tpl.forge", tenantB)
		if e != nil {
			return e
		}
		affected = tag.RowsAffected()
		return nil
	})
	// 要么直接报错（策略拒绝），要么影响 0 行（USING 过滤）；二者都算隔离成功。
	if err == nil && affected != 0 {
		t.Fatalf("★ 租户B 改动了租户A 的行（affected=%d）—— RLS 未生效", affected)
	}

	// 特权确认该行仍属于 A
	var owner string
	if err := priv.QueryRow(ctx,
		`SELECT tenant_id::text FROM dim_view_template WHERE id = 'tpl.forge'`).Scan(&owner); err != nil {
		t.Fatalf("特权查询失败：%v", err)
	}
	if owner != tenantA {
		t.Fatalf("该行应仍属于租户A，实际 %q", owner)
	}
}

// TestTenant_平台档写入被RLS拒绝 —— NULL 归属的写入契约。
//
// ══════════════════════════════════════════════════════════════════════════
// ★★ 这条钉死两个**相互独立**的契约，缺一不可：
//
//	① **写侧**：应用角色**不能**写入 tenant_id = NULL 的行。
//	   RLS 策略的 WITH CHECK(tenant_id = rls_tenant_id()) 会以
//	   SQLSTATE 42501 拒绝。这不是缺陷，是**正确的 fail-closed**：
//	   「无归属的行」不该由租户会话凭空造出来 —— 否则任何租户都能
//	   种一行全世界可见/可探测的「孤儿数据」。
//
//	② **读侧**：即便库里**已经**有 tenant_id = NULL 的行（由特权/运维通道写入），
//	   任何租户会话都**读不到**它。策略的条件 `tenant_id = rls_tenant_id()`
//	   对 NULL 求值为 NULL ⇒ 行被过滤。
//
//	★ 为什么必须写这条测试：
//	   平台级模板（对所有租户共用的默认视图）是一个**很自然的需求**，
//	   后来者极可能想「用 NULL 表示所有人可见」—— 那会**彻底破坏**隔离
//	   （NULL 行对所有租户都可见 ⇒ 谁都能塞一行给全平台看）。
//	   本测试把「NULL ≠ 公共」这条语义钉在代码里，让这个反模式**改不动**。
//	   若将来真需要平台共享模板，正确做法是**新增一条 OR tenant_id IS NULL
//	   的独立策略**（显式、可审计），而不是把写入放宽到允许 NULL。
//
// ══════════════════════════════════════════════════════════════════════════
func TestTenant_平台档写入被RLS拒绝(t *testing.T) {
	dsn := tenantDSN(t)
	app := newAppPool(t, appDSN(t))
	ctx := context.Background()
	priv := newTenantPool(t, dsn)
	defer priv.Close()

	cleanupTenantTemplates(ctx, priv, tenantA)
	defer cleanupTenantTemplates(ctx, priv, tenantA)
	// 平台行（tenant_id IS NULL）只能由特权通道清理
	defer func() {
		_, _ = priv.Exec(ctx, `DELETE FROM dim_view_template WHERE id = 'tpl.platform'`)
	}()
	cleanupTenantTemplates(ctx, priv, tenantB)

	acctP := "u.platform"
	if err := seedOwnerOrgs(ctx, priv, [2]string{tenantA, acctP}); err != nil {
		t.Fatalf("%v", err)
	}
	defer cleanupOwnerOrgs(ctx, priv, tenantA, acctP)

	// ── ① 写侧：应用角色的平台档 store（New ⇒ 不施加租户）写入必须被拒 ──
	//
	// ★ 用 New（平台档）而不是 NewForTenant：New 走裸池，不设 app.tenant_id，
	//   于是 rls_tenant_id() 返回哨兵 ⇒ WITH CHECK 拒绝 NULL 行。
	//   这恰好是我们要的：**平台档在应用角色下根本写不进去**。
	sp := New(app)
	err := sp.Save(ctx, mkTpl("tpl.platform", acctP, "/report", template.ScopePersonal), acctP)
	if err == nil {
		t.Fatalf("★ 应用角色的平台档写入**应当被 RLS 拒绝**（WITH CHECK），却成功了 —— " +
			"若这么容易就能写入 tenant_id=NULL 的行，任何人都能种一行全局可见的孤儿数据")
	}

	// 特权确认确实没写进去
	var n int
	if err := priv.QueryRow(ctx,
		`SELECT count(*) FROM dim_view_template WHERE id='tpl.platform'`).Scan(&n); err != nil {
		t.Fatalf("特权查询失败：%v", err)
	}
	if n != 0 {
		t.Fatalf("平台档写入被拒后库里不应有该行，实际 %d 行", n)
	}

	// ── ② 读侧：用特权通道**故意**种一行 tenant_id = NULL ──
	//
	// ★ 用特权通道种是为了能构造出「库里存在 NULL 行」这个读侧前提 ——
	//   应用角色种不出来（① 已证），但运维/历史数据可能留下 NULL 行。
	if _, err := priv.Exec(ctx, `
		INSERT INTO dim_view_template
			(id, name, scope, owner, page, query_state, columns, layout,
			 use_count, is_default, template_ver, created_at, updated_at, tenant_id)
		VALUES ('tpl.platform','平台模板','personal',$1,'/report','{}','[]','{}',
		        0,false,'1.0',now(),now(),NULL)`, acctP); err != nil {
		t.Fatalf("特权通道写入 NULL 行失败：%v", err)
	}

	// 特权能看见（确认写入成功）
	if err := priv.QueryRow(ctx,
		`SELECT count(*) FROM dim_view_template WHERE id='tpl.platform' AND tenant_id IS NULL`).Scan(&n); err != nil {
		t.Fatalf("特权查询失败：%v", err)
	}
	if n != 1 {
		t.Fatalf("特权通道应看到 1 行 tenant_id IS NULL，实际 %d", n)
	}

	// ★ 关键断言：两个租户都**看不到**这行 NULL 归属的行
	for name, tc := range map[string]string{"A": tenantA, "B": tenantB} {
		s := NewForTenant(app, tenantCtxFor(tc))
		if _, err := s.Load(ctx, "tpl.platform"); err == nil {
			t.Fatalf("★ 租户%s 看到了平台级（tenant_id=NULL）的行 —— 归属契约被破坏："+
				"NULL 绝不等于「公共」，否则任何租户都能凭一行 NULL 数据面向全平台", name)
		}
	}
}

// TestTenant_BumpUse审计必须带租户 —— 审计留痕必须查得到。
func TestTenant_BumpUse审计必须带租户(t *testing.T) {
	dsn := tenantDSN(t)
	app := newAppPool(t, appDSN(t))
	ctx := context.Background()
	priv := newTenantPool(t, dsn)
	defer priv.Close()

	suffix := time.Now().UnixNano() % 1_000_000_000
	tplID := fmt.Sprintf("tpl.audit.%d", suffix)
	acct := fmt.Sprintf("u.audit.%d", suffix)

	cleanupTenantTemplates(ctx, priv, tenantA)
	defer cleanupTenantTemplates(ctx, priv, tenantA)
	defer cleanupOwnerOrgs(ctx, priv, tenantA, acct)

	if err := seedOwnerOrgs(ctx, priv, [2]string{tenantA, acct}); err != nil {
		t.Fatalf("%v", err)
	}

	sa := NewForTenant(app, tenantCtxFor(tenantA))
	if err := sa.Save(ctx, mkTpl(tplID, acct, "/report", template.ScopePersonal), acct); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if err := sa.BumpUse(ctx, tplID, acct); err != nil {
		t.Fatalf("BumpUse 失败：%v", err)
	}

	// 特权通道确认审计行的 tenant_id 正确
	var tid *string
	if err := priv.QueryRow(ctx, `
		SELECT tenant_id::text FROM audit_log
		 WHERE action = 'view_template.apply' AND target = $1
		 ORDER BY at DESC, id DESC LIMIT 1`, tplID).Scan(&tid); err != nil {
		t.Fatalf("读审计失败：%v", err)
	}
	if tid == nil {
		t.Fatalf("★ BumpUse 写的审计没有 tenant_id（NULL）—— 租户档下该行对谁都不可见，等于审计失效")
	}
	if *tid != tenantA {
		t.Fatalf("审计 tenant_id 应为 %s，实际 %q", tenantA, *tid)
	}
}

// TestTenant_TenantBound自检 —— 构造方式必须能被观测到。
func TestTenant_TenantBound自检(t *testing.T) {
	app := newAppPool(t, appDSN(t))
	if New(app).TenantBound() {
		t.Fatalf("New 构造的实例不应报告 TenantBound")
	}
	if !NewForTenant(app, tenantCtxFor(tenantA)).TenantBound() {
		t.Fatalf("NewForTenant 构造的实例应报告 TenantBound")
	}
}

// TestTenant_分享表也受隔离 —— dim_view_template_share 必须一并隔离。
func TestTenant_分享表也受隔离(t *testing.T) {
	dsn := tenantDSN(t)
	app := newAppPool(t, appDSN(t))
	ctx := context.Background()
	priv := newTenantPool(t, dsn)
	defer priv.Close()

	cleanupTenantTemplates(ctx, priv, tenantA)
	cleanupTenantTemplates(ctx, priv, tenantB)
	defer cleanupTenantTemplates(ctx, priv, tenantA)
	defer cleanupTenantTemplates(ctx, priv, tenantB)

	acctA, acctB := "u.shareA", "u.shareB"
	if err := seedOwnerOrgs(ctx, priv, [2]string{tenantA, acctA}, [2]string{tenantB, acctB}); err != nil {
		t.Fatalf("%v", err)
	}
	defer cleanupOwnerOrgs(ctx, priv, acctA, acctB)

	sa := NewForTenant(app, tenantCtxFor(tenantA))
	sb := NewForTenant(app, tenantCtxFor(tenantB))

	// A 建一个带显式分享的模板（分享给某组）
	ta := mkTpl("tpl.shareA", acctA, "/report", template.ScopeTeam)
	ta.Shares = []template.Share{{SubjectKind: "group", SubjectID: "g.finance"}}
	if err := sa.Save(ctx, ta, acctA); err != nil {
		t.Fatalf("A 保存带分享模板失败：%v", err)
	}

	// B 的会话读不到 A 的分享行
	// （通过 Load 读不到模板即证明；这里再单独查分享表确认 RLS 覆盖）
	//
	// ★ 用 qOne 而不是 qRows：count(*) 恒返回**恰好一行**，且 qOne 的
	//   scanRow 契约是「游标已停在该行上」（直接 Scan，不要 Next），
	//   与 qRows 的「回调自己写 Next 循环」不同。见 store.go 两个方法的契约说明。
	var n int
	if err := sb.qOne(ctx,
		`SELECT count(*) FROM dim_view_template_share WHERE template_id = $1`,
		[]any{"tpl.shareA"},
		func(rows pgx.Rows) error { return rows.Scan(&n) }); err != nil {
		t.Fatalf("B 查分享表失败：%v", err)
	}
	if n != 0 {
		t.Fatalf("★ 租户B 看到了租户A 的分享行 %d 条 —— dim_view_template_share 未隔离", n)
	}

	// 特权确认分享行确实存在
	var total int
	if err := priv.QueryRow(ctx,
		`SELECT count(*) FROM dim_view_template_share WHERE template_id = 'tpl.shareA'`).Scan(&total); err != nil {
		t.Fatalf("特权查询失败：%v", err)
	}
	if total == 0 {
		t.Fatalf("特权通道看不到分享行 —— Save 的分享写入失败")
	}
}
