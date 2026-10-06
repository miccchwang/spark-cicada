// Package db —— Postgres 连接、迁移执行与事务辅助（M1 数据层）。
//
// 纪律（docs/01 §5、docs/09）：
//   * 连接串中的口令**绝不**写入日志或审计（G11）。
//   * 迁移按文件名序执行，且**幂等**：已应用的迁移跳过（schema_migrations 记账）。
//   * 每个迁移在**单事务**内执行；失败整体回滚（不留半截 schema）。
//   * 禁止在代码中硬编码任何凭据 —— 一律来自环境变量。
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
type Migrator struct {
	pool *pgxpool.Pool
}

// NewMigrator 用给定连接串构造迁移器。
//
// 连接串**只**从参数/环境读取；本函数不打印它（G11）。
func NewMigrator(ctx context.Context, dsn string) (*Migrator, error) {
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
	return &Migrator{pool: pool}, nil
}

// Close 释放连接池。
func (m *Migrator) Close() { m.pool.Close() }

// Pool 暴露底层连接池（供 store 复用同一池）。
func (m *Migrator) Pool() *pgxpool.Pool { return m.pool }

// Applied 读取已应用迁移。
func (m *Migrator) Applied(ctx context.Context) (map[string]AppliedMigration, error) {
	if _, err := m.pool.Exec(ctx, ensureLedger); err != nil {
		return nil, fmt.Errorf("db: 建记账表失败: %w", err)
	}
	rows, err := m.pool.Query(ctx, `SELECT version, name, hash, applied_at FROM schema_migrations`)
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
//   `go test ./...` 并行跑多个包，每个包各自 NewMigrator + Up —— 于是
//   同一时刻有 N 个 migrator 在同一个库上跑同一批迁移。它们都读到
//   「0003 尚未应用」，然后**同时**执行 CREATE TABLE / INSERT 记账：
//     - CREATE TABLE 撞 "duplicate key value violates unique constraint
//       pg_class_relname_nsp_index"
//     - 记账撞 "schema_migrations_pkey"
//   而这不是「测试环境专有的怪现象」—— 生产同样会出现：多实例滚动发布时
//   两个进程同时启动、或 CI 并行 job 指向同一张测试库，都一样会撞。
//
//   加锁比「让调用方保证串行」可靠：那是把并发正确性外包给每个调用点，
//   而调用点会越来越多（CLI、测试、迁移 Job、多副本启动）。
func (m *Migrator) Up(ctx context.Context, migs []Migration) (MigrateResult, error) {
	// 会话级咨询锁：连接池会保证 acquire 与 release 落在**同一条连接**上，
	// 因此必须显式加/解锁，且成对出现。
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("db: 获取迁移锁连接失败: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return MigrateResult{}, fmt.Errorf("db: 获取迁移咨询锁失败: %w", err)
	}
	// 解锁失败不改变结果 —— 连接归还时锁会自动释放；这里只需尽力而为。
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationLockKey) }()

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

// migrationLockKey 迁移咨询锁的键值（任意常量，全仓库一致即可）。
//
// ★ 取一个**固定常量**而非随机数：随机数会让不同的 migrator 拿到不同的锁，
//   等于没上锁。这个数字本身无意义，只要各处一致。
const migrationLockKey int64 = 0x5C_D1_CA_DA_5E_ED /* spark-cicada-seed */

// applyOne 在单事务内执行一个迁移并记账。
func (m *Migrator) applyOne(ctx context.Context, mg Migration) error {
	tx, err := m.pool.Begin(ctx)
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
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, hash) VALUES ($1,$2,$3)`,
		mg.Version, mg.Name, mg.Hash); err != nil {
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
//   全文替换会把函数体打断（audit_log_immutable 就是这种）。
//   见 stripOuterTx 的逐行实现。
var txBeginRe = regexp.MustCompile(`(?i)^\s*BEGIN\s*;?\s*$`)
var txCommitRe = regexp.MustCompile(`(?i)^\s*COMMIT\s*;?\s*$`)

// stripOuterTx 移除迁移文件**最外层**的 BEGIN/COMMIT，交由 Migrator 统一管理事务。
//
// 必要性：文件自带 BEGIN/COMMIT 会与外层事务嵌套（Postgres 会对
// 「事务中 BEGIN」发 warning、对「事务中 COMMIT」直接报错）。
//
// ★ 安全边界（这是本函数最容易写错的地方）：
//   只删「连续块中第一个 BEGIN」与「最后一个 COMMIT」，且二者必须包住整个文件。
//   plpgsql 的 `CREATE FUNCTION ... AS $$ BEGIN ... END; $$` 里的 BEGIN
//   虽然在行首，但它**不在**首尾，因此不会被删除。
//
//   历史 bug：早期实现用正则对全文做 ReplaceAll，把 audit_log_immutable()
//   函数体里的 BEGIN 一并删掉 → 函数体残缺 → 迁移在真库上失败
//   （本地因为无库而全程 skip，直到 CI 起真 Postgres 才暴露）。
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
