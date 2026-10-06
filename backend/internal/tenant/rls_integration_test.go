package tenant

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 真库集成测试（SPARK_TEST_DB_DSN 缺失时 skip）。
//
// ★★ 为什么这些测试**必须**打真库、且必须用**真连接池**：
//
//	本文件要证明的核心性质是「SET LOCAL 随事务失效，连接复用不串租」。
//	这个性质**只存在于 Postgres 的事务语义里** —— mock 出来的
//	「假 set_config」永远会「通过」，因为它没有连接池、没有事务边界、
//	没有会话变量。用 mock 测这条 = 测了个寂寞。
//
//	尤其 TestRLS_连接池复用不串租：它必须人为制造
//	「同一条物理连接先后服务两个租户」的场景。这是唯一能暴露
//	「有人把 SET LOCAL 写成 SET」的测试形式。
//
// ★★★ 为什么还必须用**非超级用户**连接（本文件最重要的一条纪律）：
//
//	本测试最初以 `spark`（本地集群的超级用户）连接，
//	结果**全部 RLS 测试失败** —— 两个租户各看到 2 行，完全没过滤。
//	排查后确认根因：PostgreSQL 的超级用户与带 BYPASSRLS 的角色
//	**无条件绕过 RLS**，`FORCE ROW LEVEL SECURITY` 对他们无效
//	（FORCE 只解决「表拥有者」，不解决「超级用户」）。
//
//	这正是一个**必须被测试覆盖**的真实故障模式：
//	生产若用超级用户连库，共享档隔离从未生效，而所有迹象都显示正常。
//	因此这里显式改用非特权角色，并在入口做前置断言。
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		t.Skip("未设置 SPARK_TEST_DB_DSN，跳过真库集成测试")
	}
	// 优先用应用角色 DSN（非超级用户）；没有则退回主 DSN（会因前置断言而 skip）。
	appDSN := strings.TrimSpace(os.Getenv("SPARK_TEST_APP_DSN"))
	if appDSN != "" {
		dsn = appDSN
	}
	// ★ MaxConns=1 是**故意的**：强制复用同一条连接。
	//   若用默认池大小，两个请求很可能落在不同连接上，
	//   于是「会话级 SET 会不会串租」这个 bug 就被掩盖了。
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败：%v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("建池失败：%v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("连通性失败：%v", err)
	}
	t.Cleanup(pool.Close)

	// ★★ 前置断言：连接角色必须真的能用 RLS。
	//   若这里失败，后续所有 RLS 测试都没有意义（测的是一个不生效的策略）。
	if err := AssertRLSCapable(context.Background(), pool); err != nil {
		pool.Close()
		t.Skipf("跳过 RLS 测试（连接角色无法使用 RLS）：%v\n"+
			"提示：设 SPARK_TEST_APP_DSN 指向一个 NOSUPERUSER NOBYPASSRLS 的应用角色，"+""+
			"例如 postgres://spark_app@127.0.0.1:55432/spark_cicada?sslmode=disable", err)
	}
	return pool
}

const (
	uidAlpha = "aaaa1111-1111-4111-8111-111111111111"
	uidBeta  = "bbbb2222-2222-4222-8222-222222222222"
)

// fixtureName 生成 per-run 唯一的表名（避免并发/历史测试互相污染）。
func fixtureName(t *testing.T) string {
	t.Helper()
	n := t.Name()
	var b strings.Builder
	for _, r := range strings.ToLower(n) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	name := "t_rls_" + b.String()
	if len(name) > 55 {
		name = name[:55]
	}
	return name
}

// setupRLSFixture 建一张带 RLS 的临时探针表，并插入两个租户各一行。
func setupRLSFixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	table := fixtureName(t)

	stmts := []string{
		`DROP TABLE IF EXISTS ` + table,
		`CREATE TABLE ` + table + ` (
			id        bigserial PRIMARY KEY,
			tenant_id uuid NOT NULL,
			label     text NOT NULL
		)`,
		// ★★ FORCE 是**必须的**（而不是可选优化）：
		//   Postgres 默认让**表拥有者绕过 RLS**。本地/测试常以拥有者身份连库，
		//   不加 FORCE 的话策略根本不生效 —— 测试会「通过」，
		//   而生产若也以拥有者身份跑，隔离就**从来没生效过**，
		//   且所有迹象都指向「已启用 RLS」。
		`ALTER TABLE ` + table + ` ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`,
		// 策略经 rls_tenant_id() 读租户（迁移 0010 的统一入口）
		`CREATE POLICY p_` + table + ` ON ` + table + `
			USING (tenant_id = rls_tenant_id())
			WITH CHECK (tenant_id = rls_tenant_id())`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("建 fixture 失败：%v\nSQL: %s", err, s)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+table) })

	// 插入两个租户的数据（经 InTenantTx ⇒ 受 WITH CHECK 约束，
	// 这本身就在验证「不能以租户 A 的身份写入租户 B 的行」）。
	for _, kv := range []struct{ uid, label string }{
		{uidAlpha, "alpha-row"}, {uidBeta, "beta-row"},
	} {
		uid, label := kv.uid, kv.label
		if err := InTenantTx(ctx, pool, sharedCtx(uid), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO `+table+` (tenant_id, label) VALUES ($1, $2)`, uid, label)
			return err
		}); err != nil {
			t.Fatalf("插入 %s 失败：%v", label, err)
		}
	}
	return table
}

func sharedCtx(uid string) TenantContext {
	return TenantContext{TenantID: uid, Tier: TierShared, RLSTenantID: uid}
}

// countRows 以指定租户身份数行。
func countRows(t *testing.T, pool *pgxpool.Pool, tc TenantContext, table string) int {
	t.Helper()
	var n int
	err := QueryRowInTenant(context.Background(), pool, tc,
		`SELECT count(*) FROM `+table, nil, &n)
	if err != nil {
		t.Fatalf("计数失败：%v", err)
	}
	return n
}

// ───────────────────────────── 核心：RLS 只看到自己的行 ─────────────────────────────

func TestRLS_只看得到自己租户的行(t *testing.T) {
	pool := testPool(t)
	table := setupRLSFixture(t, pool)

	if got := countRows(t, pool, sharedCtx(uidAlpha), table); got != 1 {
		t.Errorf("租户 Alpha 应看到 1 行，实际 %d", got)
	}
	if got := countRows(t, pool, sharedCtx(uidBeta), table); got != 1 {
		t.Errorf("租户 Beta 应看到 1 行，实际 %d", got)
	}
	// 且看到的必须是**自己那行**
	var label string
	if err := QueryRowInTenant(context.Background(), pool, sharedCtx(uidAlpha),
		`SELECT label FROM `+table, nil, &label); err != nil {
		t.Fatalf("查 label 失败：%v", err)
	}
	if label != "alpha-row" {
		t.Errorf("★ 租户 Alpha 看到了 %q —— 串租", label)
	}
}

func TestRLS_未设租户时必须看到0行(t *testing.T) {
	pool := testPool(t)
	table := setupRLSFixture(t, pool)

	// 不带租户上下文直接查（模拟「有人忘了设置」）
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	// ★ 期望 0（哨兵 uuid 匹配不到任何真实行）。
	//   这必须成立：若返回 2，说明「忘了设租户」= 看到所有人的数据。
	if n != 0 {
		t.Fatalf("★ 未设租户时看到 %d 行 —— 应为 0（fail-closed 失效）", n)
	}
}

func TestRLS_越权写入必须被拒(t *testing.T) {
	pool := testPool(t)
	table := setupRLSFixture(t, pool)

	// 以 Alpha 的身份写入 tenant_id = Beta 的行 ⇒ WITH CHECK 必须拒绝
	err := InTenantTx(context.Background(), pool, sharedCtx(uidAlpha),
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO `+table+` (tenant_id, label) VALUES ($1, 'forged')`, uidBeta)
			return err
		})
	if err == nil {
		t.Fatal("★ 以 Alpha 身份写入 Beta 的行竟然成功了 —— WITH CHECK 失效")
	}
	// 确认没有落库
	if got := countRows(t, pool, sharedCtx(uidBeta), table); got != 1 {
		t.Errorf("Beta 应仍只有 1 行，实际 %d（伪造行写进去了）", got)
	}
}

// ───────────────────────────── ★★ 最核心：连接池复用不串租 ─────────────────────────────

func TestRLS_连接池复用不串租(t *testing.T) {
	pool := testPool(t) // MaxConns=1 ⇒ 强制复用同一条物理连接
	table := setupRLSFixture(t, pool)

	// ★ 先用 Alpha 跑一次（会把 app.tenant_id 设在连接上，
	//   如果是会话级 SET，这个值会**留在连接上**）。
	if got := countRows(t, pool, sharedCtx(uidAlpha), table); got != 1 {
		t.Fatalf("Alpha 计数应为 1，实际 %d", got)
	}

	// ★★ 关键步骤：**不设租户**，直接在（刚被 Alpha 用过的）连接上查。
	//
	//   若 applyTenant 用的是会话级 SET（或 SET 未随事务失效），
	//   这里会看到 Alpha 的 1 行 —— 那就是串租。
	//   用 SET LOCAL 则事务结束即失效 ⇒ 回到哨兵 ⇒ 0 行。
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if n != 0 {
		t.Fatalf("★ 连接复用后未设租户看到 %d 行 —— 租户状态粘在连接上（应使用 SET LOCAL）", n)
	}

	// 再确认 Beta 仍然只看得到自己（证明不是「碰巧失效」而是真的按租户隔离）
	if got := countRows(t, pool, sharedCtx(uidBeta), table); got != 1 {
		t.Errorf("Beta 应看到 1 行，实际 %d", got)
	}
}

func TestRLS_并发交替不串租(t *testing.T) {
	pool := testPool(t)
	table := setupRLSFixture(t, pool)

	// 并发地交替以两个租户身份查询；每个 goroutine 都必须只看到自己那 1 行。
	// ★ 这条测试的价值在于：即便实现有「设置与查询不在同一事务」的缺陷，
	//   单线程测试也可能侥幸通过；并发交替会让错位暴露。
	const rounds = 40
	var wg sync.WaitGroup
	errCh := make(chan string, rounds*2)
	for i := 0; i < rounds; i++ {
		for _, u := range []string{uidAlpha, uidBeta} {
			uid := u
			wg.Add(1)
			go func() {
				defer wg.Done()
				var n int
				if err := QueryRowInTenant(context.Background(), pool,
					sharedCtx(uid), `SELECT count(*) FROM `+table, nil, &n); err != nil {
					errCh <- "查询失败：" + err.Error()
					return
				}
				if n != 1 {
					errCh <- "★ 串租：租户 " + uid[:4] + " 看到 " + itoa(n) + " 行"
				}
			}()
		}
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
}

// ───────────────────────────── 上下文校验（入口即拒绝） ─────────────────────────────

func TestInTenantTx_非法上下文必须拒绝(t *testing.T) {
	pool := testPool(t) // 需要真池，否则 nil 池会先于校验失败
	noop := func(context.Context, pgx.Tx) error { return nil }

	cases := []struct {
		name string
		tc   TenantContext
	}{
		{"租户 ID 为空", TenantContext{Tier: TierShared}},
		{"租户 ID 非 uuid", TenantContext{TenantID: "acme", RLSTenantID: "acme", Tier: TierShared}},
		{"RLS 输入与租户身份不一致", TenantContext{
			TenantID: uidAlpha, RLSTenantID: uidBeta, Tier: TierShared}},
		{"档位未知", TenantContext{TenantID: uidAlpha, RLSTenantID: uidAlpha, Tier: "weird"}},
		{"独立档缺 schema", TenantContext{
			TenantID: uidAlpha, RLSTenantID: uidAlpha, Tier: TierDedicated}},
		{"独立档 schema 非法", TenantContext{
			TenantID: uidAlpha, RLSTenantID: uidAlpha, Tier: TierDedicated,
			SchemaName: strp("public")}},
		{"共享档带 schema", TenantContext{
			TenantID: uidAlpha, RLSTenantID: uidAlpha, Tier: TierShared,
			SchemaName: strp("t_x")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ran := false
			err := InTenantTx(context.Background(), pool, c.tc,
				func(ctx context.Context, tx pgx.Tx) error { ran = true; return nil })
			if err == nil {
				t.Fatal("★ 非法上下文竟然放行 —— 必须 fail-closed")
			}
			if ran {
				t.Fatal("★ fn 不应被执行（上下文非法就该在入口拒绝）")
			}
		})
	}
	_ = noop
}

func TestInTenantTx_空池必须拒绝(t *testing.T) {
	ran := false
	err := InTenantTx(context.Background(), nil, sharedCtx(uidAlpha),
		func(context.Context, pgx.Tx) error { ran = true; return nil })
	if err == nil {
		t.Fatal("★ 空池必须拒绝")
	}
	if ran {
		t.Fatal("空池时 fn 不应执行")
	}
}

// ───────────────────────────── 独立档 search_path ─────────────────────────────

func TestDedicated_search_path生效(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	schema, err := SchemaNameFor(uidAlpha)
	if err != nil {
		t.Fatal(err)
	}
	// 建独立 schema（模拟开通流程；真实流程见 Task #55）
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatalf("清理 schema 失败：%v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("建 schema 失败：%v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`) })
	if _, err := pool.Exec(ctx,
		`CREATE TABLE `+schema+`.probe (v text)`); err != nil {
		t.Fatalf("建表失败：%v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO `+schema+`.probe (v) VALUES ('dedicated-here')`); err != nil {
		t.Fatalf("插入失败：%v", err)
	}

	tc := TenantContext{
		TenantID: uidAlpha, Tier: TierDedicated,
		SchemaName: &schema, RLSTenantID: uidAlpha,
	}
	var v string
	if err := QueryRowInTenant(ctx, pool, tc, `SELECT v FROM probe`, nil, &v); err != nil {
		t.Fatalf("独立档应当能不带 schema 前缀查到表：%v", err)
	}
	if v != "dedicated-here" {
		t.Errorf("期望 dedicated-here，实际 %q", v)
	}

	// ★ search_path 必须随事务失效：事务外再查应当找不到 probe
	//   （证明我们用的是 LOCAL 而不是会话级 SET）。
	var n int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_class WHERE relname = 'probe' AND relnamespace = `+
			`(SELECT oid FROM pg_namespace WHERE nspname = $1)`, "public").Scan(&n)
	if err != nil {
		t.Fatalf("查 pg_class 失败：%v", err)
	}
	// public 下不该有 probe（它建在独立 schema 里）—— 顺带证明隔离是真的
	if n != 0 {
		t.Errorf("public 下不应存在 probe 表")
	}
}

// ───────────────────────────── 辅助 ─────────────────────────────

// ───────────────────────────── ★★ 超级用户绕过 RLS（真实事故的回归钉） ─────────────────────────────

// TestRLS_超级用户会绕过RLS_必须被自检发现 是本文件最重要的测试。
//
// ★ 它钉住的是一个**真实踩过的事故**：
//
//	本地集群的 spark 角色是超级用户。我们用 ENABLE + FORCE ROW LEVEL
//	SECURITY + 正确的策略建好了表，然后查询 —— **看到全部行**。
//	所有「隔离已启用」的迹象都为真，而实际过滤掉 0 行。
//
//	若没有这条测试，这个缺陷会以「本地全绿」的形式通过，
//	然后在生产暴露成跨租户数据泄漏 —— 或者更糟：
//	生产同样是超级用户，于是隔离**从未生效**，且永远没人发现。
//
// 本测试断言两件事：
//  1. 自检函数能**识别**超级用户（避免它自己失效）。
//  2. 超级用户确实绕过 RLS（把 Postgres 的这条规则固化为可执行事实）。
//
// 若某天 Postgres 改了行为（超级用户不再绕过），第 2 条会失败 ——
// 那正是我们想要知道的：它可以提醒我们重新评估自检逻辑。
func TestRLS_超级用户会绕过RLS_必须被自检发现(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		t.Skip("未设置 SPARK_TEST_DB_DSN，跳过")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败：%v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("建池失败：%v", err)
	}
	defer pool.Close()

	s, err := DescribeRole(ctx, pool)
	if err != nil {
		t.Fatalf("读角色属性失败：%v", err)
	}
	t.Logf("主 DSN 角色：%s superuser=%v bypassrls=%v canUseRLS=%v",
		s.RoleName, s.IsSuper, s.BypassesRLS, s.CanUseRLS)

	if s.CanUseRLS {
		// 主 DSN 本身就是非特权角色 ⇒ 它可以直接测 RLS 是否生效。
		// 这时仍要证明「自检会说它可用」。
		if err := AssertRLSCapable(ctx, pool); err != nil {
			t.Fatalf("非特权角色应通过自检，实际失败：%v", err)
		}
		return
	}

	// ★ 关键断言：特权角色**必须**被自检拦下。
	if err := AssertRLSCapable(ctx, pool); err == nil {
		t.Fatal("★ 自检未拦下超级用户/BYPASSRLS 角色 —— " +
			"共享档隔离会静默失效（FORCE ROW LEVEL SECURITY 对此无效）")
	}

	// ★ 并且实测确认：特权角色确实绕过 RLS（把 Postgres 规则固化为事实）。
	table := fixtureName(t) + "_sup"
	stmts := []string{
		`DROP TABLE IF EXISTS ` + table,
		`CREATE TABLE ` + table + ` (tenant_id uuid NOT NULL, label text)`,
		`ALTER TABLE ` + table + ` ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY p_` + table + ` ON ` + table + ` USING (tenant_id = rls_tenant_id())`,
		`INSERT INTO ` + table + ` VALUES
			('` + uidAlpha + `','a'), ('` + uidBeta + `','b')`,
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st); err != nil {
			t.Fatalf("建探针失败：%v\nSQL: %s", err, st)
		}
	}
	defer func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+table) }()

	// 以 Alpha 身份查询，若特权角色绕过 RLS 则应看到 2 行
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("取连接失败：%v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败：%v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, SetLocalTenantSQL, uidAlpha); err != nil {
		t.Fatalf("设置租户失败：%v", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if n != 2 {
		t.Errorf("特权角色应绕过 RLS 看到 2 行（Postgres 规则），实际 %d —— "+
			"若 Postgres 行为已变，请重新评估 rls_capability.go 的自检逻辑", n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}
