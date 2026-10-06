// integration_test.go —— 真库端到端测试（迁移 + 桶 NULL 语义 + D7 库层约束）。
//
// 运行方式：
//
//	SPARK_TEST_DB_DSN='postgres://user:pass@host:5432/spark_test?sslmode=disable' \
//	  go test ./internal/store/ -run Integration -v
//
// ★ 跳过 vs 失败的纪律（这是本文件最重要的设计）：
//   - 本地开发（无 docker/psql）未设 DSN ⇒ **跳过**（t.Skip），避免阻塞日常开发。
//   - CI 设 SPARK_REQUIRE_DB=1 ⇒ 未设 DSN 时**直接失败**（t.Fatal），绝不跳过。
//
// 为什么必须这样：如果 CI 也静默 skip，这 6 条真库断言就**从未被执行过**，
// 而 CI 依然全绿 —— M1 数据面（幂等迁移 / NULL 不补 0 / 审计 append-only）
// 就成了「没人验证过却显示通过」的假绿。宁可红，不可假绿。
package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/db"
)

// openTestDB 连接测试库并应用迁移。
//
// 无 DSN 时：本地跳过；CI（SPARK_REQUIRE_DB=1）失败。
func openTestDB(t *testing.T) (*db.Migrator, func()) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		if requireDB() {
			t.Fatal("★ 真库集成测试被要求必须运行（SPARK_REQUIRE_DB=1），但未设置 SPARK_TEST_DB_DSN。" +
				"闸门不允许静默跳过 —— 请起 Postgres 并注入 DSN，否则移除 SPARK_REQUIRE_DB。")
		}
		t.Skip("未配置 SPARK_TEST_DB_DSN —— 跳过真库集成测试（本地开发；CI 会设置）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 迁移目录：backend/internal/store → 仓库根/sql/migrations
	dir := filepath.Join("..", "..", "..", "sql", "migrations")
	migs, err := db.LoadMigrations(dir)
	if err != nil {
		t.Fatalf("加载迁移失败：%v", err)
	}
	m, err := db.NewMigrator(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败（%s）：%v", db.RedactDSN(dsn), err)
	}
	if _, err := m.Up(ctx, migs); err != nil {
		m.Close()
		t.Fatalf("应用迁移失败：%v", err)
	}
	return m, m.Close
}

// requireDB 报告当前是否处于「真库必须运行」模式（CI）。
func requireDB() bool {
	v := strings.TrimSpace(os.Getenv("SPARK_REQUIRE_DB"))
	return v == "1" || strings.EqualFold(v, "true")
}

// TestIntegration_SuiteIsNotSilentlySkipped 自检：防止整包被静默跳过。
//
// 若在「允许跳过」的环境下运行，本测试会明确告知「真库断言本次未执行」，
// 使 `go test ./...` 的输出不会让人误以为数据面已被验证。
func TestIntegration_SuiteIsNotSilentlySkipped(t *testing.T) {
	if strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN")) == "" && !requireDB() {
		t.Log("⚠ 本轮未执行真库集成断言（无 DSN）。数据面的真库行为**尚未被验证**。")
		return
	}
	t.Log("真库集成断言已启用")
}

// TestIntegration_MigrationsIdempotent 迁移可重复执行（第二次全部跳过）。
func TestIntegration_MigrationsIdempotent(t *testing.T) {
	m, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	dir := filepath.Join("..", "..", "..", "sql", "migrations")
	migs, err := db.LoadMigrations(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Up(ctx, migs)
	if err != nil {
		t.Fatalf("二次迁移失败：%v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("迁移不幂等：第二次仍应用了 %v", res.Applied)
	}
	if len(res.Skipped) != len(migs) {
		t.Fatalf("期望全部跳过 %d 个，实际跳过 %d 个", len(migs), len(res.Skipped))
	}
}

// TestIntegration_AuditAppendOnly DB 层触发器必须真的阻止 UPDATE/DELETE（G10）。
func TestIntegration_AuditAppendOnly(t *testing.T) {
	m, done := openTestDB(t)
	defer done()
	ctx := context.Background()
	pool := m.Pool()

	// 写一条
	if _, err := pool.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target) VALUES ('t','integration.test','x')`); err != nil {
		t.Fatalf("写审计失败：%v", err)
	}
	// UPDATE 必须被触发器拒绝
	if _, err := pool.Exec(ctx,
		`UPDATE audit_log SET action='tampered' WHERE action='integration.test'`); err == nil {
		t.Fatal("G10 失败：审计表 UPDATE 竟然成功 —— append-only 触发器失效")
	}
	// DELETE 必须被拒绝
	if _, err := pool.Exec(ctx,
		`DELETE FROM audit_log WHERE action='integration.test'`); err == nil {
		t.Fatal("G10 失败：审计表 DELETE 竟然成功")
	}
}

// TestIntegration_ITCannotViewBusinessValues DB 层 CHECK 必须真的拦住（D7）。
func TestIntegration_ITCannotViewBusinessValues(t *testing.T) {
	m, done := openTestDB(t)
	defer done()
	ctx := context.Background()
	pool := m.Pool()

	// 准备两个组织节点（外键要求）
	//
	// ★ 0011 之后 fact_entitlement 的主键 (account) 已被替换为
	//   **部分唯一索引** uq_fact_entitlement_tenant_account (tenant_id, account)
	//   WHERE tenant_id IS NOT NULL —— 因为 account 必须改为「租户内唯一」。
	//
	// ★★ 后果（必须理解，否则会写出查不到冲突目标的 upsert）：
	//   PostgreSQL 的 `ON CONFLICT (cols)` **无法推断部分唯一索引**，
	//   除非把该索引的 WHERE 谓词**原样写进 ON CONFLICT**：
	//       ON CONFLICT (tenant_id, account) WHERE tenant_id IS NOT NULL
	//   漏掉 WHERE ⇒ SQLSTATE 42P10「no unique or exclusion constraint
	//   matching the ON CONFLICT specification」。
	//   这正是本用例迁移后第一次运行时踩到的报错。
	_, _ = pool.Exec(ctx, `INSERT INTO dim_org (account, display_name, tier, primary_dept)
		VALUES ('it.t','IT','T4','SEA') ON CONFLICT DO NOTHING`)
	_, _ = pool.Exec(ctx, `INSERT INTO dim_org (account, display_name, tier, primary_dept)
		VALUES ('biz.t','Biz','T3','SEA') ON CONFLICT DO NOTHING`)

	// ① 业务账号：允许可见
	_, err := pool.Exec(ctx, `INSERT INTO fact_entitlement (account, base_template, can_view_business_values)
		VALUES ('biz.t','tpl.lead', true)
		ON CONFLICT (tenant_id, account) WHERE tenant_id IS NOT NULL
		DO UPDATE SET can_view_business_values = true`)
	if err != nil {
		t.Fatalf("业务账号授权不应失败：%v", err)
	}

	// ② IT 账号可见业务数值：DB 必须拒绝
	_, err = pool.Exec(ctx, `INSERT INTO fact_entitlement (account, base_template, can_view_business_values)
		VALUES ('it.t','tpl.it', true)
		ON CONFLICT (tenant_id, account) WHERE tenant_id IS NOT NULL
		DO UPDATE SET base_template='tpl.it', can_view_business_values=true`)
	if err == nil {
		t.Fatal("D7 失败：DB 层竟然允许 IT 账号可见业务数值 —— CHECK 约束失效")
	}
}

// TestIntegration_NullNotZero 桶里 NULL 必须原样读成 nil（绝不补 0）—— G3 第一红线。
func TestIntegration_NullNotZero(t *testing.T) {
	m, done := openTestDB(t)
	defer done()
	ctx := context.Background()
	pool := m.Pool()

	// 注册桶为 FRESH，否则查询侧会 fail-closed 拒绝
	if _, err := pool.Exec(ctx, `
		INSERT INTO registry_bucket (id, grain, produced_by, refresh, algo_versions, rule_versions, state)
		VALUES ('pnl_month', ARRAY['month','channel_code','shop_id','brand'],
		        ARRAY['algo.gp','algo.cogs'], 'monthly_incremental',
		        '{"algo.gp":3,"algo.cogs":2}'::jsonb, '{}'::jsonb, 'FRESH')
		ON CONFLICT (id) DO UPDATE SET state='FRESH'`); err != nil {
		t.Fatalf("注册桶失败：%v", err)
	}

	// 清掉测试月的数据，避免与历史数据混淆
	_, _ = pool.Exec(ctx, `DELETE FROM bucket_pnl_month WHERE month = DATE '2099-01-01'`)

	// 插一行：cogs 有值，net_contrib 为 NULL（依赖缺失被跳过）
	if _, err := pool.Exec(ctx, `
		INSERT INTO bucket_pnl_month
		    (month, channel_code, shop_id, brand, rev, cogs, gp, gmp, net_contrib, cov_cogs, skipped_fields)
		VALUES (DATE '2099-01-01','TK-TH','S-IT-1','KONVY',
		        1000, 400, 600, 0.6, NULL, 0.95, ARRAY['net_contrib'])`); err != nil {
		t.Fatalf("插入桶行失败：%v", err)
	}

	st := New(pool)
	// 桶状态必须 FRESH
	meta, err := st.BucketState(ctx, "pnl_month")
	if err != nil {
		t.Fatal(err)
	}
	if meta.State != "FRESH" {
		t.Fatalf("桶状态期望 FRESH，得 %s", meta.State)
	}
	if meta.AlgoVersions["algo.gp"] != 3 {
		t.Fatalf("桶算法版本读取错误：%+v", meta.AlgoVersions)
	}

	// 查询该行
	q := contracts.QueryState{}
	q.Time.From = "2099-01-01"
	q.Time.To = "2099-01-01"
	q.Dims.StoreKey = []string{"S-IT-1"}
	rows, err := st.SelectBucket(ctx, "pnl_month", q)
	if err != nil {
		t.Fatalf("查询桶失败：%v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("期望 1 行，得 %d 行", len(rows))
	}
	r := rows[0]

	// ★ 核心断言：NULL 读成 nil，**不是** 0
	if r.Metrics["net_contrib"] != nil {
		t.Fatalf("G3 严重失败：NULL 被读成 %v（应为 nil）—— 缺失被补成了 0！", *r.Metrics["net_contrib"])
	}
	if r.Metrics["cogs"] == nil {
		t.Fatal("有值的 cogs 读成了 nil")
	}
	if *r.Metrics["cogs"] != 400 {
		t.Fatalf("cogs 值错误：期望 400，得 %v", *r.Metrics["cogs"])
	}
	// 覆盖率列
	if r.CovCogs == nil || *r.CovCogs != 0.95 {
		t.Fatalf("cov_cogs 读取错误：%v", r.CovCogs)
	}
	// 跳过字段
	found := false
	for _, f := range r.SkippedFields {
		if f == "net_contrib" {
			found = true
		}
	}
	if !found {
		t.Fatalf("skipped_fields 未包含 net_contrib：%v", r.SkippedFields)
	}

	// 清理
	_, _ = pool.Exec(ctx, `DELETE FROM bucket_pnl_month WHERE month = DATE '2099-01-01'`)
}

// TestIntegration_SlotHealth 覆盖率无实测记录时视为 0（未知 ⇒ 不达标，fail-closed）。
func TestIntegration_SlotHealth(t *testing.T) {
	m, done := openTestDB(t)
	defer done()
	ctx := context.Background()
	pool := m.Pool()

	// slot.affiliate 在 0002 里可能不存在；显式登记一个已知 MISSING 槽
	if _, err := pool.Exec(ctx, `
		INSERT INTO registry_slot (id, name, source_kind, source_ref, key_strategy, coverage_gate, freshness, permission, status)
		VALUES ('slot.integ_test','集成测试槽','db','x','k',0.80,'1d','L2','MISSING')
		ON CONFLICT (id) DO UPDATE SET status='MISSING'`); err != nil {
		t.Fatalf("登记测试槽失败：%v", err)
	}
	st := New(pool)
	hs, err := st.SlotHealth(ctx, []string{"slot.integ_test"})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := hs["slot.integ_test"]
	if !ok {
		t.Fatal("未返回槽健康度")
	}
	if s.Status != "MISSING" {
		t.Fatalf("槽状态期望 MISSING，得 %s", s.Status)
	}
	if s.Coverage != 0 {
		t.Fatalf("无实测覆盖率应视为 0（fail-closed），得 %v", s.Coverage)
	}

	_, _ = pool.Exec(ctx, `DELETE FROM registry_slot WHERE id='slot.integ_test'`)
}

// TestIntegration_UnregisteredBucketFailsClosed 未注册桶 ⇒ BucketState 返回 UNREGISTERED。
func TestIntegration_UnregisteredBucketFailsClosed(t *testing.T) {
	m, done := openTestDB(t)
	defer done()
	st := New(m.Pool())
	meta, err := st.BucketState(context.Background(), "bucket_does_not_exist")
	if err != nil {
		t.Fatal(err)
	}
	if meta.State != "UNREGISTERED" {
		t.Fatalf("未注册桶状态期望 UNREGISTERED，得 %q（查询侧将 fail-closed 拒绝）", meta.State)
	}
}
