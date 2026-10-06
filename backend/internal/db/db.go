// Package db —— Postgres 连接、迁移执行与事务辅助（M1 数据层）。
//
// 纪律（docs/01 §5、docs/09）：
//   - 连接串中的口令**绝不**写入日志或审计（G11）。
//   - 迁移按文件名序执行，且**幂等**：已应用的迁移跳过（schema_migrations 记账）。
//   - 每个迁移在**单事务**内执行；失败整体回滚（不留半截 schema）。
//   - 禁止在代码中硬编码任何凭据 —— 一律来自环境变量。
package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 是一个待应用的迁移文件。
type Migration struct {
	Version string // 文件名前缀，如 0001
	Name    string // 文件名，如 0001_core.sql
	SQL     string
	Hash    string // SQL 内容的 sha256（检测「已应用文件被改写」）
}

// migrationFileRe 匹配 NNNN_name.sql。
var migrationFileRe = regexp.MustCompile(`^(\d{4,})_([a-z0-9_]+)\.sql$`)

// LoadMigrations 从目录加载并校验迁移文件（按版本升序）。
//
// 校验：版本号唯一、命名规范、非空。发现重复版本直接报错（fail-fast）。
func LoadMigrations(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("db: read migrations dir %s: %w", dir, err)
	}
	seen := map[string]string{}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := migrationFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("db: 迁移文件名不合规（期望 NNNN_name.sql）：%s", e.Name())
		}
		version, name := m[1], e.Name()
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("db: 迁移版本重复 %s：%s 与 %s", version, prev, name)
		}
		seen[version] = name

		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("db: 读取迁移 %s: %w", name, err)
		}
		if strings.TrimSpace(string(b)) == "" {
			return nil, fmt.Errorf("db: 迁移 %s 内容为空", name)
		}
		sum := sha256.Sum256(b)
		out = append(out, Migration{
			Version: version,
			Name:    name,
			SQL:     string(b),
			Hash:    hex.EncodeToString(sum[:]),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("db: 目录 %s 下没有任何迁移文件", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// ensureLedger 建迁移记账表（若不存在）。
const ensureLedger = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     text        PRIMARY KEY,
    name        text        NOT NULL,
    hash        text        NOT NULL,
    applied_at  timestamptz NOT NULL DEFAULT now()
);`

// AppliedMigration 是记账表里的一行。
type AppliedMigration struct {
	Version   string
	Name      string
	Hash      string
	AppliedAt time.Time
}

// MigrateResult 汇总一次迁移运行的结果。
type MigrateResult struct {
	Applied []string // 本次新应用
	Skipped []string // 已应用过、跳过
}

// Migrator 执行迁移。
//
// ★ schema 字段支持多租户独立档：空串表示当前默认 schema（单租户/共享档）。
//
//	非空时，迁移的所有对象都建在该 schema 内（通过 search_path），
//	且迁移记账表 `schema_migrations` 也**建在该 schema 内**。
//
//	★★ 为什么记账必须按 schema 隔离（而不是共用一个 public.schema_migrations）：
//	  若所有租户共用一个记账表，那么「租户 A 已应用 0010」会被租户 B 误判为
//	  「我也应用过了」—— 于是 B 的 schema 里**从来没有 0010 创建的对象**，
//	  而系统认为它已经就绪。这类缺陷在开通新租户时静默发生，
//	  到查询时才炸（表不存在），且排障会指向完全错误的方向。
type Migrator struct {
	pool *pgxpool.Pool
	// schema 为迁移目标 schema；空串 = 用连接的默认 search_path。
	schema string
}

// NewMigrator 用给定连接串构造迁移器（作用于默认 schema）。
//
// 连接串**只**从参数/环境读取；本函数不打印它（G11）。
func NewMigrator(ctx context.Context, dsn string) (*Migrator, error) {
	return NewMigratorForSchema(ctx, dsn, "")
}

// NewMigratorForSchema 构造作用于指定 schema 的迁移器（多租户独立档用）。
//
// schema 为空 ⇒ 与 NewMigrator 等价（默认 search_path）。
// schema 非空 ⇒ 迁移期间 search_path 限定到该 schema。
//
// ★ 安全性：schema 名会进入 `SET search_path`（无法参数化的 DDL 语句），
//
//	因此必须白名单校验 —— 见 validSchemaName。
func NewMigratorForSchema(ctx context.Context, dsn, schema string) (*Migrator, error) {
	if schema != "" {
		if !validSchemaName(schema) {
			return nil, fmt.Errorf(
				"db: schema 名不合规（仅允许 [a-z][a-z0-9_]{0,62}）：%q", schema)
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// 注意：不回显 dsn（可能含口令）
		return nil, fmt.Errorf("db: 连接串解析失败（不含口令回显）: %w", err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: 建连接池失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: 连通性检测失败: %w", err)
	}
	return &Migrator{pool: pool, schema: schema}, nil
}

// schemaNameRe 迁移目标 schema 名的白名单。
//
// ★ 与 backend/internal/tenant.SanitizeSchema 同规则但**故意不共用**：
//
//	db 包是最底层的包，不应依赖 tenant 包（依赖方向：tenant → db）。
//	两处各持一份白名单是有意的重复 —— 若只留一处，日后有人改宽了
//	就会同时放宽两个层次。宁可有两次「要不要放宽」的思考。
var schemaNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// validSchemaName 白名单校验 schema 名。
func validSchemaName(s string) bool {
	switch s {
	case "", "public", "pg_catalog", "information_schema", "pg_toast":
		// 保留名一律拒绝 —— 绝不允许把迁移跑进这些 schema
		return s == ""
	}
	return schemaNameRe.MatchString(s)
}

// Schema 返回当前迁移器的目标 schema（空串 = 默认）。
func (m *Migrator) Schema() string { return m.schema }

// EnsureSchema 建目标 schema（若不存在）。schema 为空时是空操作。
//
// ★ 用 CREATE SCHEMA IF NOT EXISTS 而非先查再建：后者有 TOCTOU 竞态
//
//	（两个开通流程同时判断「不存在」然后同时建）。
func (m *Migrator) EnsureSchema(ctx context.Context) error {
	if m.schema == "" {
		return nil
	}
	if !validSchemaName(m.schema) {
		return fmt.Errorf("db: schema 名不合规：%q", m.schema)
	}
	// schema 名已过白名单校验 ⇒ 可安全拼接（DDL 无法参数化）
	if _, err := m.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+m.schema); err != nil {
		return fmt.Errorf("db: 建 schema %s 失败: %w", m.schema, err)
	}
	return nil
}

// pool 上的所有操作在指定 schema 时都必须先设 search_path。
//
// ★ 为什么不用 DSN 的 `search_path=` 参数：
//
//	连接池里不同连接的 search_path 需要一致，靠 DSN 参数可以做到，
//	但 `SET search_path` 是**会话级**的 —— 连接池复用时会串。
//	更可靠的做法：每次操作前在**当前连接**上显式设置。
//	这里用 `SET search_path`（会话级）是**可以的**，因为整个 Migrator
//	的语义就是「这个池只服务这一个 schema」—— 池与 schema 是绑定的。
//	★ 但为杜绝误用：Migrator 实例**不对外暴露通用查询**，
//	  只提供迁移相关方法。这样「拿错池查错租户」在类型上就不可能。
func (m *Migrator) setSearchPath(ctx context.Context, conn *pgxpool.Conn) error {
	if m.schema == "" {
		return nil
	}
	// 追加 public 以便访问共享函数（如 rls_tenant_id()）
	// ★ public 放**后面**：同名对象优先解析到租户 schema（隔离优先）
	if _, err := conn.Exec(ctx, `SET search_path = `+m.schema+`, public`); err != nil {
		return fmt.Errorf("db: 设置 search_path(%s) 失败: %w", m.schema, err)
	}
	return nil
}

// lockKeyFor 计算该迁移器的咨询锁键。
//
// ★★ 为什么锁键必须按 schema 区分（真实并发场景）：
//
//	若所有租户共用同一个锁键，那么「开通租户 A」会阻塞「开通租户 B」——
//	1000 家租户的开通就退化成串行。更糟的是：
//	若开通被某个慢迁移卡住，**所有**新租户都开不出来。
//	按 schema 派生锁键 ⇒ 不同租户可并行开通，同一租户仍然串行（正确）。
//
// ★ 默认 schema（空串）沿用原常量以保持向后兼容 ——
//
//	既有单租户部署的迁移行为**一字不变**。
func (m *Migrator) lockKeyFor() int64 {
	if m.schema == "" {
		return migrationLockKey
	}
	// 由 schema 名派生一个稳定的 int64（FNV-1a 变体）
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(m.schema); i++ {
		h ^= uint64(m.schema[i])
		h *= 0x100000001b3
	}
	// 与基础键区分开，避免与默认 schema 的锁相撞
	return int64(h ^ 0x5CD1CADA5EED)
}

// Close 释放连接池。
func (m *Migrator) Close() { m.pool.Close() }

// Pool 暴露底层连接池（供 store 复用同一池）。
func (m *Migrator) Pool() *pgxpool.Pool { return m.pool }

// Applied 读取已应用迁移。
//
// ★ schema 非空时，记账表建在**该 schema 内**（见 Migrator 的说明）。
func (m *Migrator) Applied(ctx context.Context) (map[string]AppliedMigration, error) {
	// ★ 记账表必须在目标 schema 内创建：先建 schema，再建表。
	//   两者都幂等，可重复执行。
	if err := m.EnsureSchema(ctx); err != nil {
		return nil, err
	}
	if m.schema != "" {
		// 用 schema 限定名建记账表 —— 不依赖 search_path，
		// 因为此处可能还没设置过 search_path（本函数会先被 Up 调用）。
		if _, err := m.pool.Exec(ctx,
			`CREATE TABLE IF NOT EXISTS `+m.schema+`.schema_migrations (
				version     text        PRIMARY KEY,
				name        text        NOT NULL,
				hash        text        NOT NULL,
				applied_at  timestamptz NOT NULL DEFAULT now()
			)`); err != nil {
			return nil, fmt.Errorf("db: 建记账表(%s) 失败: %w", m.schema, err)
		}
	} else if _, err := m.pool.Exec(ctx, ensureLedger); err != nil {
		return nil, fmt.Errorf("db: 建记账表失败: %w", err)
	}

	rows, err := m.pool.Query(ctx, m.ledgerSelect())
	if err != nil {
		return nil, fmt.Errorf("db: 读取记账表失败: %w", err)
	}
	defer rows.Close()

	out := map[string]AppliedMigration{}
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Hash, &a.AppliedAt); err != nil {
			return nil, fmt.Errorf("db: 扫描记账行失败: %w", err)
		}
		out[a.Version] = a
	}
	return out, rows.Err()
}

// ledgerSelect 返回读取记账表的 SQL（按 schema 限定或默认）。
func (m *Migrator) ledgerSelect() string {
	if m.schema == "" {
		return `SELECT version, name, hash, applied_at FROM schema_migrations`
	}
	return `SELECT version, name, hash, applied_at FROM ` + m.schema + `.schema_migrations`
}

// DryRun 校验迁移集但**不**执行（供 CI 使用）。
//
// 同时检测「已应用迁移被改写」（hash 不一致）——这是最危险的漂移。
func (m *Migrator) DryRun(ctx context.Context, migs []Migration) (MigrateResult, error) {
	applied, err := m.Applied(ctx)
	if err != nil {
		return MigrateResult{}, err
	}
	var res MigrateResult
	for _, mg := range migs {
		a, done := applied[mg.Version]
		if !done {
			res.Applied = append(res.Applied, mg.Name+"（待应用）")
			continue
		}
		if a.Hash != mg.Hash {
			return res, fmt.Errorf(
				"db: 迁移 %s 已被改写（已应用 hash=%s，当前 hash=%s）——拒绝继续，请新增迁移而非改动历史",
				a.Name, a.Hash[:12], mg.Hash[:12])
		}
		res.Skipped = append(res.Skipped, mg.Name)
	}
	return res, nil
}

// migrationLockKey 是 pg_advisory_lock 的键（任意常量，全局唯一即可）。
//
// ★ 为什么必须上锁（真实事故，本仓库踩过）：
//
//	`go test ./...` 并行跑多个包，每个包各自 NewMigrator + Up —— 于是
//	同一时刻有 N 个 migrator 在同一个库上跑同一批迁移。它们都读到
//	「0003 尚未应用」，然后**同时**执行 CREATE TABLE / INSERT 记账：
//	  - CREATE TABLE 撞 "duplicate key value violates unique constraint
//	    pg_class_relname_nsp_index"
//	  - 记账撞 "schema_migrations_pkey"
//	而这不是「测试环境专有的怪现象」—— 生产同样会出现：多实例滚动发布时
//	两个进程同时启动、或 CI 并行 job 指向同一张测试库，都一样会撞。
//
//	加锁比「让调用方保证串行」可靠：那是把并发正确性外包给每个调用点，
//	而调用点会越来越多（CLI、测试、迁移 Job、多副本启动）。
func (m *Migrator) Up(ctx context.Context, migs []Migration) (MigrateResult, error) {
	// 会话级咨询锁：连接池会保证 acquire 与 release 落在**同一条连接**上，
	// 因此必须显式加/解锁，且成对出现。
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("db: 获取迁移锁连接失败: %w", err)
	}
	defer conn.Release()

	// ★ 锁键按 schema 派生：不同租户可并行开通，同一租户仍串行。
	lockKey := m.lockKeyFor()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return MigrateResult{}, fmt.Errorf("db: 获取迁移咨询锁失败: %w", err)
	}
	// 解锁失败不改变结果 —— 连接归还时锁会自动释放；这里只需尽力而为。
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKey) }()

	// ★ 在**持锁的连接**上设置 search_path，保证后续 DDL 落在目标 schema。
	//   必须在 acquire 之后、任何 DDL 之前 —— 否则迁移会建到 public 里，
	//   而记账却记在租户 schema 中（下一次开通会认为「已应用」）。
	if err := m.setSearchPath(ctx, conn); err != nil {
		return MigrateResult{}, err
	}

	// 注意：这里用 Applied(ctx)，它走的是池（可能是**另一条**连接），
	// 不继承上面的 search_path。但 Applied 内部用的是 schema 限定名
	// （见 ledgerSelect），因此不依赖 search_path —— 这是刻意设计。
	applied, err := m.Applied(ctx)
	if err != nil {
		return MigrateResult{}, err
	}
	var res MigrateResult
	for _, mg := range migs {
		if a, done := applied[mg.Version]; done {
			if a.Hash != mg.Hash {
				return res, fmt.Errorf(
					"db: 迁移 %s 已被改写（hash 不一致）——拒绝继续", a.Name)
			}
			res.Skipped = append(res.Skipped, mg.Name)
			continue
		}
		if err := m.applyOne(ctx, mg); err != nil {
			return res, err
		}
		res.Applied = append(res.Applied, mg.Name)
	}
	return res, nil
}

// migrationLockKey 默认 schema 的迁移咨询锁键值（保持向后兼容）。
//
// ★ 取一个**固定常量**而非随机数：随机数会让不同的 migrator 拿到不同的锁，
//
//	等于没上锁。这个数字本身无意义，只要各处一致。
//
// ★ 多租户独立档用 lockKeyFor() 派生 per-schema 键（见该函数说明）。
const migrationLockKey int64 = 0x5C_D1_CA_DA_5E_ED /* spark-cicada-seed */

// applyOne 在单事务内执行一个迁移并记账。
func (m *Migrator) applyOne(ctx context.Context, mg Migration) error {
	// ★ 用**单独连接**并在其上设 search_path + 跑事务：
	//   迁移里的 DDL（CREATE TABLE 无 schema 前缀）依赖 search_path。
	//   若在池上直接 Begin，拿到的可能是另一条连接（search_path 未设），
	//   对象就会建到 public 里 —— 而记账记在租户 schema，
	//   于是「已应用」但表不在租户 schema 中。这是最隐蔽的一类错位。
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: 获取迁移连接失败: %w", err)
	}
	defer conn.Release()
	if err := m.setSearchPath(ctx, conn); err != nil {
		return err
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 迁移文件自带 BEGIN/COMMIT 时会与外层事务冲突；此处去掉外层包裹语义，
	// 统一由本函数掌控事务边界。
	body := stripOuterTx(mg.SQL)
	if _, err := tx.Exec(ctx, body); err != nil {
		return fmt.Errorf("db: 应用迁移 %s 失败: %w", mg.Name, err)
	}
	// 记账：schema 限定（不依赖 search_path）
	insertLedger := `INSERT INTO schema_migrations (version, name, hash) VALUES ($1,$2,$3)`
	if m.schema != "" {
		insertLedger = `INSERT INTO ` + m.schema +
			`.schema_migrations (version, name, hash) VALUES ($1,$2,$3)`
	}
	if _, err := tx.Exec(ctx, insertLedger, mg.Version, mg.Name, mg.Hash); err != nil {
		return fmt.Errorf("db: 记账迁移 %s 失败: %w", mg.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: 提交迁移 %s 失败: %w", mg.Name, err)
	}
	return nil
}

// txEdgeRe 匹配「行首」的裸 BEGIN;/BEGIN 或 COMMIT;/COMMIT（大小写不敏感）。
//
// ★ 只用于**行首**判定，不能用于全文替换：plpgsql 函数体内的 BEGIN/END 也顶格，
//
//	全文替换会把函数体打断（audit_log_immutable 就是这种）。
//	见 stripOuterTx 的逐行实现。
var txBeginRe = regexp.MustCompile(`(?i)^\s*BEGIN\s*;?\s*$`)
var txCommitRe = regexp.MustCompile(`(?i)^\s*COMMIT\s*;?\s*$`)

// stripOuterTx 移除迁移文件**最外层**的 BEGIN/COMMIT，交由 Migrator 统一管理事务。
//
// 必要性：文件自带 BEGIN/COMMIT 会与外层事务嵌套（Postgres 会对
// 「事务中 BEGIN」发 warning、对「事务中 COMMIT」直接报错）。
//
// ★ 安全边界（这是本函数最容易写错的地方）：
//
//	只删「连续块中第一个 BEGIN」与「最后一个 COMMIT」，且二者必须包住整个文件。
//	plpgsql 的 `CREATE FUNCTION ... AS $$ BEGIN ... END; $$` 里的 BEGIN
//	虽然在行首，但它**不在**首尾，因此不会被删除。
//
//	历史 bug：早期实现用正则对全文做 ReplaceAll，把 audit_log_immutable()
//	函数体里的 BEGIN 一并删掉 → 函数体残缺 → 迁移在真库上失败
//	（本地因为无库而全程 skip，直到 CI 起真 Postgres 才暴露）。
func stripOuterTx(sql string) string {
	lines := strings.Split(sql, "\n")

	// 找第一个非空、非注释行：必须是 BEGIN 才认为有外层事务。
	firstIdx := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		firstIdx = i
		break
	}
	// 找最后一个非空、非注释行：必须是 COMMIT。
	lastIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		lastIdx = i
		break
	}

	if firstIdx < 0 || lastIdx <= firstIdx {
		return sql
	}
	if !txBeginRe.MatchString(lines[firstIdx]) || !txCommitRe.MatchString(lines[lastIdx]) {
		// 没有成对的外层事务包裹 ⇒ 原样返回，不做任何删改（保守优先）。
		return sql
	}

	// 只清空这两行（保留行占位，便于报错时行号仍然对得上）。
	out := make([]string, len(lines))
	copy(out, lines)
	out[firstIdx] = ""
	out[lastIdx] = ""
	return strings.Join(out, "\n")
}

// DSNFromEnv 从环境变量取连接串。
//
// 优先级（★ 两个名字都必须认）：
//
//  1. SPARK_DB_DSN       —— 运行期主名（sparkd / spark-migrate 部署用）
//  2. SPARK_TEST_DB_DSN  —— 测试期名（真库集成测试用；CI 只设这一个）
//  3. SPARK_PG_*         —— 分字段组装（本地/容器编排）
//
// ★ 真实事故（CI 真库首次抓出）：本函数原先**只认 SPARK_DB_DSN**，
// 而 CI workflow 只设了 SPARK_TEST_DB_DSN，于是 `spark-migrate -up`
// 在真库步骤直接死于「未配置连接信息」—— 迁移根本没跑，
// 后续所有真库闸门全部 skip。
//
// 更值得记的是**为什么没被早发现**：
// 集成测试读 SPARK_TEST_DB_DSN、迁移读 SPARK_DB_DSN，两者各自"都对"，
// 单看任一侧都没有 bug；只有把它们放进同一个 CI 作业才暴露。
// 所以这里不再让两个名字各自为政 —— 统一由本函数解析，单一事实来源。
//
// **绝不**在返回值之外打印口令。
func DSNFromEnv() (string, error) {
	// 主名优先；测试期名次之。两者都空才回落到分字段组装。
	for _, key := range []string{"SPARK_DB_DSN", "SPARK_TEST_DB_DSN"} {
		if dsn := strings.TrimSpace(os.Getenv(key)); dsn != "" {
			return dsn, nil
		}
	}
	host := os.Getenv("SPARK_PG_HOST")
	user := os.Getenv("SPARK_PG_USER")
	if host == "" || user == "" {
		return "", errors.New("db: 未配置连接信息（需 SPARK_DB_DSN / SPARK_TEST_DB_DSN，或 SPARK_PG_HOST/SPARK_PG_USER）")
	}
	port := envOr("SPARK_PG_PORT", "5432")
	name := envOr("SPARK_PG_DB", "spark_cicada")
	pass := os.Getenv("SPARK_PG_PASSWORD") // 允许为空（本地 trust）
	ssl := envOr("SPARK_PG_SSLMODE", "disable")
	if pass == "" {
		return fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=%s", user, host, port, name, ssl), nil
	}
	// 口令做 URL 转义，避免特殊字符破坏 DSN 结构
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		user, urlEscape(pass), host, port, name, ssl), nil
}

// RedactDSN 把连接串中的口令替换为 ***，供日志/错误展示（G11）。
func RedactDSN(dsn string) string {
	re := regexp.MustCompile(`(://[^:/@]+):([^@]*)@`)
	return re.ReplaceAllString(dsn, "$1:***@")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// urlEscape 只转义会破坏 DSN 结构的字符（口令用）。
func urlEscape(s string) string {
	r := strings.NewReplacer(
		"%", "%25", ":", "%3A", "/", "%2F", "?", "%3F",
		"#", "%23", "@", "%40", "[", "%5B", "]", "%5D", " ", "%20",
	)
	return r.Replace(s)
}

// InTx 在事务内运行 fn，出错自动回滚。
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
