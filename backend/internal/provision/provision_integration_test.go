package provision

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/db"
	"github.com/miccchwang/spark-cicada/backend/internal/tenant"
)

// 真库集成测试：验证开通流程的**幂等 / 可重入 / 并发安全**三条性质。
//
// ★ 为什么必须打真库：这三条性质全部依赖数据库语义
//
//	（ON CONFLICT、CREATE SCHEMA IF NOT EXISTS、咨询锁、事务）。
//	mock 会把它们全部「假成功」。
func provisionFixture(t *testing.T) (*Orchestrator, *pgxpool.Pool, string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		t.Skip("未设置 SPARK_TEST_DB_DSN，跳过真库集成测试")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("建平台池失败：%v", err)
	}
	t.Cleanup(pool.Close)

	migDir := "../../../sql/migrations"
	migs, err := db.LoadMigrations(migDir)
	if err != nil {
		t.Fatalf("加载迁移失败：%v", err)
	}

	// 用「默认 schema 迁移器」先确保平台库自身就绪（dim_tenant 存在）
	pm, err := db.NewMigrator(ctx, dsn)
	if err != nil {
		t.Fatalf("构造平台迁移器失败：%v", err)
	}
	defer pm.Close()
	if _, err := pm.Up(ctx, migs); err != nil {
		t.Fatalf("平台库迁移失败：%v", err)
	}

	return New(pool, dsn, migs), pool, dsn
}

// testTenantID 生成一个 per-test 唯一 uuid（避免测试互相污染）。
//
// ★ 用 t.Name() 的 hash 派生：同一测试每次跑得到同一 ID（可重复），
//
//	不同测试得到不同 ID（不互扰）。
func testTenantID(t *testing.T) string {
	t.Helper()
	h := uint32(2166136261)
	for _, b := range []byte(t.Name()) {
		h ^= uint32(b)
		h *= 16777619
	}
	// 拼成合法 uuid 形态（版本位设 4、variant 位设 8）
	return strings.ToLower(strings.Join([]string{
		hex8(h),
		"0000",
		"4000",
		"8000",
		hex12(h),
	}, "-"))
}

func hex8(h uint32) string  { return pad(hex(h), 8) }
func hex12(h uint32) string { return pad(hex(h), 8) + "0000" }
func pad(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s[:n]
}
func hex(h uint32) string {
	const d = "0123456789abcdef"
	if h == 0 {
		return "0"
	}
	var b []byte
	for h > 0 {
		b = append([]byte{d[h%16]}, b...)
		h /= 16
	}
	return string(b)
}

func dedicatedRecord(t *testing.T, code string) TenantRecord {
	t.Helper()
	id := testTenantID(t)
	sn, err := tenant.SchemaNameFor(id)
	if err != nil {
		t.Fatalf("派生 schema 名失败：%v", err)
	}
	return TenantRecord{
		ID: id, Code: code, Name: "Test " + code,
		Tier: tenant.TierDedicated, SchemaName: &sn,
	}
}

// cleanupTenant 清掉本测试造出的租户（记录 + schema）。
func cleanupTenant(t *testing.T, pool *pgxpool.Pool, tr TenantRecord) {
	t.Helper()
	ctx := context.Background()
	if tr.SchemaName != nil {
		_, _ = pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+*tr.SchemaName+` CASCADE`)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM dim_tenant WHERE id = $1`, tr.ID)
}

// ───────────────────────────── 幂等 ─────────────────────────────

func TestProvision_幂等重复开通无害(t *testing.T) {
	o, pool, _ := provisionFixture(t)
	ctx := context.Background()
	tr := dedicatedRecord(t, "idem")
	defer cleanupTenant(t, pool, tr)

	// 第一次
	res1, err := o.Provision(ctx, tr)
	if err != nil {
		t.Fatalf("首次开通失败：%v", err)
	}
	if !res1.Created {
		t.Error("首次开通应报告 Created=true")
	}
	if !res1.Finalised {
		t.Error("首次开通应 Finalised")
	}
	if len(res1.Applied) == 0 {
		t.Error("首次开通应应用迁移")
	}
	if res1.Schema != *tr.SchemaName {
		t.Errorf("schema 应为 %q，实际 %q", *tr.SchemaName, res1.Schema)
	}

	// 第二次（幂等）
	res2, err := o.Provision(ctx, tr)
	if err != nil {
		t.Fatalf("重复开通不应失败：%v", err)
	}
	if res2.Created {
		t.Error("重复开通应报告 Created=false")
	}
	if len(res2.Applied) != 0 {
		t.Errorf("重复开通不应再应用迁移，实际应用了 %v", res2.Applied)
	}
	if len(res2.Skipped) == 0 {
		t.Error("重复开通应跳过全部迁移")
	}
	// 状态仍须是 active（★ 不能被打回 provisioning）
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM dim_tenant WHERE id = $1`, tr.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("★ 重复开通把状态改成了 %q（应为 active）", status)
	}
}

// ───────────────────────────── 可重入 ─────────────────────────────

func TestProvision_可重入_半途失败后可续跑(t *testing.T) {
	o, pool, dsn := provisionFixture(t)
	ctx := context.Background()
	tr := dedicatedRecord(t, "resume")
	defer cleanupTenant(t, pool, tr)

	// 模拟「建了 schema 但迁移只跑了一部分」：手工建 schema 并只应用前 3 个迁移
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+*tr.SchemaName); err != nil {
		t.Fatalf("建 schema 失败：%v", err)
	}
	partial, err := db.NewMigratorForSchema(ctx, dsn, *tr.SchemaName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := partial.Up(ctx, o.Migrations[:3]); err != nil {
		t.Fatalf("部分迁移失败：%v", err)
	}
	partial.Close()

	// 现在重跑完整开通：必须**续跑**剩下的迁移，而不是从头再来或失败
	res, err := o.Provision(ctx, tr)
	if err != nil {
		t.Fatalf("续跑开通失败：%v", err)
	}
	if !res.Finalised {
		t.Error("续跑后应 Finalised")
	}
	// 前 3 个应被跳过，其余应被应用
	if len(res.Skipped) != 3 {
		t.Errorf("应跳过前 3 个迁移，实际跳过 %d 个：%v", len(res.Skipped), res.Skipped)
	}
	if len(res.Applied) != len(o.Migrations)-3 {
		t.Errorf("应应用剩余 %d 个迁移，实际 %d 个",
			len(o.Migrations)-3, len(res.Applied))
	}

	// 验证租户 schema 里**真的有**迁移建出的表（而不是只记了账）
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace ns ON ns.oid = c.relnamespace
		WHERE ns.nspname = $1 AND c.relkind = 'r'`, *tr.SchemaName).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("★ 租户 schema 里没有任何表 —— 记账与实物脱节")
	}
	t.Logf("租户 schema 中共 %d 张表", n)
}

// ───────────────────────────── 并发安全 ─────────────────────────────

func TestProvision_并发开通同一租户只有一个成功建(t *testing.T) {
	o, pool, _ := provisionFixture(t)
	ctx := context.Background()
	tr := dedicatedRecord(t, "concurrent")
	defer cleanupTenant(t, pool, tr)

	const n = 6
	var wg sync.WaitGroup
	createdCount := make([]bool, n)
	errs := make([]error, n)
	// 统计「本次应用了迁移」的个数 —— 并发下应当只有一个真正建
	appliedCount := make([]int, n)

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := o.Provision(ctx, tr)
			errs[i] = err
			createdCount[i] = res.Created
			appliedCount[i] = len(res.Applied)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 个并发开通失败：%v", i, err)
		}
	}
	// ★ 恰好一个应报告 Created=true（其余走 ON CONFLICT 更新分支）
	nc := 0
	for _, c := range createdCount {
		if c {
			nc++
		}
	}
	if nc != 1 {
		t.Errorf("★ 应恰好 1 个报 Created=true，实际 %d 个", nc)
	}

	// 最终状态必须是 active，且只有一条记录
	var status string
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT status, (SELECT count(*) FROM dim_tenant WHERE code = $1)
		   FROM dim_tenant WHERE id = $2`, tr.Code, tr.ID).Scan(&status, &cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("★ 应恰有 1 条租户记录，实际 %d 条", cnt)
	}
	if status != "active" {
		t.Errorf("最终状态应为 active，实际 %q", status)
	}
}

// ───────────────────────────── 共享档 ─────────────────────────────

func TestProvision_共享档不建schema(t *testing.T) {
	o, pool, _ := provisionFixture(t)
	ctx := context.Background()

	tr := TenantRecord{
		ID: testTenantID(t), Code: "shared1", Name: "Shared One",
		Tier: tenant.TierShared, // 无 SchemaName
	}
	defer cleanupTenant(t, pool, tr)

	res, err := o.Provision(ctx, tr)
	if err != nil {
		t.Fatalf("共享档开通失败：%v", err)
	}
	if res.Schema != "" {
		t.Errorf("共享档不应有 schema，实际 %q", res.Schema)
	}
	if len(res.Applied) != 0 {
		t.Error("共享档不建 schema，故不应应用任何迁移")
	}
	if !res.Finalised {
		t.Error("共享档开通后应 Finalised")
	}
}

// ───────────────────────────── 入参校验（fail-fast） ─────────────────────────────

func TestProvision_非法入参必须被拒(t *testing.T) {
	o, _, _ := provisionFixture(t)
	ctx := context.Background()
	validID := testTenantID(t)
	sn, _ := tenant.SchemaNameFor(validID)

	cases := []struct {
		name string
		in   TenantRecord
	}{
		{"ID 非 uuid", TenantRecord{ID: "acme", Code: "x", Tier: tenant.TierShared}},
		{"档位未知", TenantRecord{ID: validID, Code: "x", Tier: "weird"}},
		{"短码为空", TenantRecord{ID: validID, Code: "", Tier: tenant.TierShared}},
		{"短码含大写", TenantRecord{ID: validID, Code: "Acme", Tier: tenant.TierShared}},
		{"短码含下划线", TenantRecord{ID: validID, Code: "a_c", Tier: tenant.TierShared}},
		{"短码以数字开头", TenantRecord{ID: validID, Code: "1acme", Tier: tenant.TierShared}},
		{"独立档缺 schema", TenantRecord{ID: validID, Code: "acme", Tier: tenant.TierDedicated}},
		{"独立档 schema 非法", TenantRecord{
			ID: validID, Code: "acme", Tier: tenant.TierDedicated,
			SchemaName: strp("public")}},
		{"共享档带 schema", TenantRecord{
			ID: validID, Code: "acme", Tier: tenant.TierShared, SchemaName: &sn}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := o.Provision(ctx, c.in); err == nil {
				t.Fatal("★ 非法入参竟然通过 —— 必须 fail-fast")
			}
			// ★ 且不得留下任何记录（不做一半）。
			//   注意：ID 非法（如 "acme"）时无法用 uuid 比较 ——
			//   改用 code 查（若真写进去了，code 是唯一的，查得到）。
			var n int
			if tenant.IsUUID(c.in.ID) {
				if err := o.Platform.QueryRow(ctx,
					`SELECT count(*) FROM dim_tenant WHERE id = $1`, c.in.ID).Scan(&n); err != nil {
					t.Fatal(err)
				}
			} else if c.in.Code != "" {
				if err := o.Platform.QueryRow(ctx,
					`SELECT count(*) FROM dim_tenant WHERE code = $1`, c.in.Code).Scan(&n); err != nil {
					t.Fatal(err)
				}
			}
			if n != 0 {
				t.Errorf("★ 非法入参留下了 %d 条记录（应 fail-fast 不做一半）", n)
			}
		})
	}
}

// ───────────────────────────── 防「重试复活已停用租户」 ─────────────────────────────

func TestProvision_重试不得复活已停用租户(t *testing.T) {
	o, pool, _ := provisionFixture(t)
	ctx := context.Background()
	tr := dedicatedRecord(t, "suspend")
	defer cleanupTenant(t, pool, tr)

	// 先正常开通
	if _, err := o.Provision(ctx, tr); err != nil {
		t.Fatalf("开通失败：%v", err)
	}
	// 运营把它停用（欠费/违规）
	if _, err := pool.Exec(ctx,
		`UPDATE dim_tenant SET status = 'suspended' WHERE id = $1`, tr.ID); err != nil {
		t.Fatal(err)
	}

	// ★ 现在重跑开通流程 —— 必须**拒绝**，不得把 suspended 改成 active。
	//   理由：运维重试开通是很常见的动作，若它能复活停用租户，
	//   那「停用」这个动作就形同虚设。
	if _, err := o.Provision(ctx, tr); err == nil {
		t.Fatal("★ 重试开通复活了已停用租户 —— 停用动作形同虚设")
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM dim_tenant WHERE id = $1`, tr.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "suspended" {
		t.Fatalf("状态应保持 suspended，实际 %q", status)
	}
}

// ───────────────────────────── 卡住租户巡检 ─────────────────────────────

func TestProvision_卡住租户可被巡检发现(t *testing.T) {
	o, pool, _ := provisionFixture(t)
	ctx := context.Background()

	// 造一个「卡在 provisioning 很久」的记录
	id := testTenantID(t)
	sn, _ := tenant.SchemaNameFor(id)
	tr := TenantRecord{ID: id, Code: "stuck1", Name: "Stuck",
		Tier: tenant.TierDedicated, SchemaName: &sn}
	defer cleanupTenant(t, pool, tr)

	if _, err := pool.Exec(ctx, `
		INSERT INTO dim_tenant (id, code, name, tier, status, schema_name, created_at)
		VALUES ($1,$2,$3,'dedicated','provisioning',$4, now() - interval '3 hours')
		ON CONFLICT (id) DO UPDATE SET status='provisioning',
		                               created_at = now() - interval '3 hours'`,
		tr.ID, tr.Code, tr.Name, *tr.SchemaName); err != nil {
		t.Fatalf("造卡住记录失败：%v", err)
	}

	stuck, err := o.FindStuck(ctx, 0)
	if err != nil {
		t.Fatalf("巡检失败：%v", err)
	}
	found := false
	for _, s := range stuck {
		if s.ID == tr.ID {
			found = true
			if s.AgeString == "" {
				t.Error("应带出卡住时长")
			}
		}
	}
	if !found {
		t.Fatal("★ 卡在 provisioning 3 小时的租户未被巡检发现")
	}

	// 阈值过滤：1 天以上则这个（3 小时）不该被列出
	stuck2, err := o.FindStuck(ctx, 24*60*60*1e9) // 24h
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stuck2 {
		if s.ID == tr.ID {
			t.Error("阈值 24h 时不应列出仅卡住 3h 的租户")
		}
	}
}

// ───────────────────────────── 迁移记账按 schema 隔离 ─────────────────────────────

func TestProvision_记账按schema隔离(t *testing.T) {
	o, pool, dsn := provisionFixture(t)
	ctx := context.Background()

	trA := dedicatedRecord(t, "isoa")
	defer cleanupTenant(t, pool, trA)

	// ★ 关键：隔离性测试需要**两个不同的 uuid 派生出两个不同 schema**，
	//   但 testTenantID 依赖 t.Name() 故两者相同。这里手工造第二个。
	trB := trA
	idB := flipUUID(trA.ID)
	snB, err := tenant.SchemaNameFor(idB)
	if err != nil {
		t.Fatal(err)
	}
	trB.ID = idB
	trB.Code = "isob"
	trB.SchemaName = &snB
	defer cleanupTenant(t, pool, trB)

	if trA.SchemaName == trB.SchemaName {
		t.Fatal("前置条件失败：两个租户派生出同一 schema")
	}

	// 只开通 A
	if _, err := o.Provision(ctx, trA); err != nil {
		t.Fatalf("开通 A 失败：%v", err)
	}

	// B 的 schema 不存在，故其记账表也不存在 —— 证明记账**不是**共用的
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_namespace WHERE nspname = $1`, *trB.SchemaName).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("B 的 schema 不应存在（只开通了 A）")
	}

	// 再开通 B：它必须**从头**跑全部迁移（而不是因为 A 跑过就跳过）
	resB, err := o.Provision(ctx, trB)
	if err != nil {
		t.Fatalf("开通 B 失败：%v", err)
	}
	if len(resB.Applied) != len(o.Migrations) {
		t.Errorf("★ B 应从头应用全部 %d 个迁移（记账按 schema 隔离），实际应用 %d 个",
			len(o.Migrations), len(resB.Applied))
	}

	// 且 A 的记账表里仍是「全部已应用」（未被 B 影响）
	mA, err := db.NewMigratorForSchema(ctx, dsn, *trA.SchemaName)
	if err != nil {
		t.Fatal(err)
	}
	defer mA.Close()
	appliedA, err := mA.Applied(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(appliedA) != len(o.Migrations) {
		t.Errorf("A 的记账应仍有 %d 条，实际 %d 条", len(o.Migrations), len(appliedA))
	}
}

// flipUUID 把 uuid 的首字符改一下，得到另一个合法 uuid。
func flipUUID(u string) string {
	b := []byte(u)
	if b[0] == 'f' {
		b[0] = 'e'
	} else {
		b[0] = 'f'
	}
	return string(b)
}

func strp(s string) *string { return &s }

// 断言 jsonb 配额可被读出（防止 quota 序列化出错）
func TestProvision_配额写入可读(t *testing.T) {
	o, pool, _ := provisionFixture(t)
	ctx := context.Background()
	tr := dedicatedRecord(t, "quota1")
	defer cleanupTenant(t, pool, tr)

	if _, err := o.Provision(ctx, tr); err != nil {
		t.Fatalf("开通失败：%v", err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT quota FROM dim_tenant WHERE id = $1`, tr.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var q map[string]any
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatalf("配额不是合法 JSON：%v", err)
	}
	for _, k := range []string{"maxAccounts", "maxRows", "maxQueriesPerMin", "allowExport"} {
		if _, ok := q[k]; !ok {
			t.Errorf("配额缺少字段 %q", k)
		}
	}
}
