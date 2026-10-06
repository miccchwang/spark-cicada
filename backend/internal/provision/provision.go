// Package provision —— 租户开通与迁移编排（多租户独立档）。
//
// ══════════════════════════════════════════════════════════════════════════
// ★★ 本包要解决的核心问题是**幂等 + 可重入 + 并发安全**，三者缺一不可：
//
//	幂等：开通流程会因网络抖动/进程重启被重跑。第二次跑必须无害。
//	可重入：失败发生在「建了 schema 但迁移跑了一半」时，
//	       重试必须**从断点继续**，而不是从头再来（或直接失败）。
//	并发：两个开通请求同时到（用户连点两次、或运营批量导入），
//	     必须只有一个真正建、另一个安全等待 —— 且**都不能**建出半个租户。
//
//	这三条各自都不难，难在同时成立。本包的做法：
//	  · 状态机推进（provisioning → active），不靠「有没有建过」猜测
//	  · 迁移记账按 schema 隔离（所以重试能精确续跑）
//	  · 咨询锁按 schema 派生（所以同租户串行、异租户并行）
//	  · ★ 先登记再建（而非先建再登记）—— 见下
//
// ══════════════════════════════════════════════════════════════════════════
package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/db"
	"github.com/miccchwang/spark-cicada/backend/internal/tenant"
)

// ErrTenantExists 表示该租户已开通（幂等：调用方可视为成功）。
var ErrTenantExists = errors.New("provision: 租户已存在")

// TenantRecord 是 dim_tenant 的一行（开通流程的输入/输出）。
type TenantRecord struct {
	ID         string
	Code       string
	Name       string
	Tier       tenant.IsolationTier
	Status     tenant.TenantStatus
	SchemaName *string
	Region     string
}

// Orchestrator 编排「建 schema → 跑迁移 → 注册租户」。
type Orchestrator struct {
	// Platform 平台库连接（dim_tenant 所在）。含 public.search_path。
	Platform *pgxpool.Pool
	// DSN 用于为租户建独立 Migrator（同一库、不同 schema）。
	//
	// ★ 为什么不复用 Platform 池：Migrator 的池与 schema 绑定，
	//   复用会让「作用域」变得隐式（同一个池时而跑租户 A、时而 B）。
	//   显式给 DSN 换一个池，作用域在类型上就清楚了。
	DSN string
	// Migrations 已加载的迁移集（由调用方 LoadMigrations 一次，复用）。
	Migrations []db.Migration
}

// New 构造编排器。
func New(platform *pgxpool.Pool, dsn string, migs []db.Migration) *Orchestrator {
	return &Orchestrator{Platform: platform, DSN: dsn, Migrations: migs}
}

// ProvisionResult 汇总一次开通的结果。
type ProvisionResult struct {
	TenantID  string
	Schema    string
	Created   bool // 本次是否**新建**了租户记录（false = 已存在，幂等返回）
	Applied   []string
	Skipped   []string
	Finalised bool // 是否已置为 active
}

// Provision 开通一个租户（幂等、可重入、并发安全）。
//
// 步骤：
//  1. 校验入参（档位 / 短码 / schema 名），任何不合规立即拒绝（不做一半）。
//  2. 在**平台库**登记 dim_tenant（status=provisioning）。
//     ★ 先登记再建：若先建 schema 后登记，中间失败会留下**孤儿 schema**
//     —— 无人知道它属于谁、也没人清理。先登记则孤儿变成「半开通记录」，
//     可被巡检发现并重试（重试正是本函数支持的）。
//  3. 独立档：建 schema（IF NOT EXISTS）→ 跑全部迁移（幂等续跑）。
//  4. 置 status=active（只有此时才允许服务）。
//
// ★ 失败时**不**回滚 dim_tenant 记录：留下 provisioning 状态是**有意的**
//
//	—— 它让失败可见且可重试。若删掉记录，失败就变成「什么都没发生」，
//	而 schema 可能已经建了一半。
func (o *Orchestrator) Provision(ctx context.Context, in TenantRecord) (ProvisionResult, error) {
	if err := validateInput(in); err != nil {
		return ProvisionResult{}, err
	}

	var res ProvisionResult
	res.TenantID = in.ID

	// ── ① 登记（幂等：ON CONFLICT 只更新元信息，**不**把 active 打回 provisioning）
	created, err := o.upsertTenant(ctx, in)
	if err != nil {
		return res, err
	}
	res.Created = created

	// ── ② 独立档：建 schema + 跑迁移
	schema := ""
	if in.Tier == tenant.TierDedicated {
		if in.SchemaName == nil || *in.SchemaName == "" {
			return res, fmt.Errorf("provision: 独立档必须提供 schema 名")
		}
		schema = *in.SchemaName
		res.Schema = schema

		if o.DSN == "" {
			return res, fmt.Errorf("provision: 未提供 DSN，无法为租户建 schema")
		}
		m, err := db.NewMigratorForSchema(ctx, o.DSN, schema)
		if err != nil {
			return res, fmt.Errorf("provision: 构造租户迁移器失败: %w", err)
		}
		defer m.Close()

		// ★ 迁移在**租户 schema** 内执行：Up 内部按 schema 派生锁键，
		//   因此不同租户可并行开通、同一租户自动串行。
		mres, err := m.Up(ctx, o.Migrations)
		if err != nil {
			// 保留 provisioning 状态，返回错误供调用方重试
			return res, fmt.Errorf("provision: 租户 %s 迁移失败（可重试）: %w", in.Code, err)
		}
		res.Applied = mres.Applied
		res.Skipped = mres.Skipped
	}

	// ── ③ 置 active
	if err := o.activate(ctx, in.ID); err != nil {
		return res, err
	}
	res.Finalised = true
	return res, nil
}

// upsertTenant 登记或更新租户记录。返回 created=true 表示本次新建。
func (o *Orchestrator) upsertTenant(ctx context.Context, in TenantRecord) (bool, error) {
	if o.Platform == nil {
		return false, fmt.Errorf("provision: 平台库连接为空")
	}
	quota := tenant.DefaultTenantQuota
	// ★ 状态刻意用 provisioning 而非直接 active：
	//   「记录已存在」不等于「可以服务」。只有迁移跑完才 active。
	// ★ (xmax = 0) 是 Postgres 里「本行是本次 INSERT 新插入的」惯用判据
	//   （xmax=0 表示没有删除/更新事务标记）。
	var inserted bool
	err := o.Platform.QueryRow(ctx, `
		INSERT INTO dim_tenant (id, code, name, tier, status, schema_name, region, quota)
		VALUES ($1, $2, $3, $4, 'provisioning', $5, $6,
		        jsonb_build_object(
		            'maxAccounts', $7::int,
		            'maxRows', $8::bigint,
		            'maxQueriesPerMin', $9::int,
		            'allowExport', $10::boolean))
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			code = EXCLUDED.code,
			updated_at = now()
		RETURNING (xmax = 0) AS inserted`,
		in.ID, in.Code, in.Name, string(in.Tier), in.SchemaName, regionOr(in.Region),
		quota.MaxAccounts, quota.MaxRows, quota.MaxQueriesPerMin, quota.AllowExport).
		Scan(&inserted)
	if err != nil {
		return false, fmt.Errorf("provision: 登记租户失败: %w", err)
	}
	// ★ ON CONFLICT 分支**故意不更新 tier / status / schema_name**：
	//
	//   更新 status 会把一个已 active 的租户打回 provisioning
	//   （重跑开通流程就把线上租户弄下线了）。
	//   更新 tier 会让「共享档改独立档」看起来只要重跑开通即可 ——
	//   而迁移档位需要数据搬迁，绝不是一个 upsert 能完成的事。
	//   更新 schema_name 会让 DB 的 CHECK 约束与租户身份脱节。
	//
	//   换言之：upsert 只更新**展示性元信息**（name/code）。
	//   档位与状态的变更走各自专门的流程（那是两个不同的运维动作）。
	return inserted, nil
}

// activate 把租户置为 active。
//
// ★ 幂等：已经是 active 时重复执行无害。
// ★ 但**不**允许把 suspended / closed 改成 active：
//
//	那意味着「重跑一次开通流程」就能让一个被停用（欠费/违规）的租户复活。
//	这是一个真实的风险 —— 运维重试开通是很常见的动作。
func (o *Orchestrator) activate(ctx context.Context, id string) error {
	tag, err := o.Platform.Exec(ctx, `
		UPDATE dim_tenant
		   SET status = 'active', updated_at = now()
		 WHERE id = $1
		   AND status IN ('provisioning','active')`, id)
	if err != nil {
		return fmt.Errorf("provision: 置为 active 失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 没改到 ⇒ 要么不存在，要么是 suspended/closed
		var status string
		err := o.Platform.QueryRow(ctx,
			`SELECT status FROM dim_tenant WHERE id = $1`, id).Scan(&status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("provision: 租户 %s 不存在", id)
			}
			return fmt.Errorf("provision: 查询租户状态失败: %w", err)
		}
		if status == "suspended" || status == "closed" {
			return fmt.Errorf(
				"provision: 拒绝把 %s 状态的租户置为 active（防止重试流程复活已停用租户）",
				status)
		}
		return fmt.Errorf("provision: 租户 %s 状态为 %s，无法置为 active", id, status)
	}
	return nil
}

// Fail 把一个开通失败的租户标记为 provisioning（保持不可服务）。
//
// ★ 提供这个显式方法而不是让调用方写 SQL：
//
//	「失败时该置成什么状态」应当只有一个答案。
//	直觉上会想「删掉记录」，但那会丢失「这里有过一次失败尝试」的信息。
func (o *Orchestrator) Fail(ctx context.Context, id string) error {
	_, err := o.Platform.Exec(ctx, `
		UPDATE dim_tenant SET status = 'provisioning', updated_at = now()
		 WHERE id = $1 AND status = 'provisioning'`, id)
	return err
}

// validateInput 校验入参（fail-fast，不做一半）。
func validateInput(in TenantRecord) error {
	if !tenant.IsUUID(in.ID) {
		return fmt.Errorf("provision: 租户 ID 不是合法 uuid：%q", in.ID)
	}
	if !tenant.IsolationTier(string(in.Tier)).Valid() {
		return fmt.Errorf("provision: 未知档位：%q", in.Tier)
	}
	if strings.TrimSpace(in.Code) == "" {
		return fmt.Errorf("provision: 租户短码不能为空")
	}
	// 短码必须 DNS 安全（用于子域名）
	for i, r := range in.Code {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return fmt.Errorf("provision: 短码含非法字符 %q（应仅 [a-z0-9-]）：%q", r, in.Code)
		}
		if i == 0 && (r == '-' || (r >= '0' && r <= '9')) {
			return fmt.Errorf("provision: 短码必须以小写字母开头：%q", in.Code)
		}
	}
	switch in.Tier {
	case tenant.TierDedicated:
		if in.SchemaName == nil || *in.SchemaName == "" {
			return fmt.Errorf("provision: 独立档必须提供 schema 名")
		}
		if _, err := tenant.SanitizeSchema(*in.SchemaName); err != nil {
			return err
		}
	case tenant.TierShared:
		// ★ 共享档必须**不带** schema（与 DB CHECK 约束一致）。
		if in.SchemaName != nil && *in.SchemaName != "" {
			return fmt.Errorf("provision: 共享档不应带 schema 名：%q", *in.SchemaName)
		}
	}
	return nil
}

// regionOr 默认地域。
func regionOr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "ap-southeast-1"
	}
	return s
}

// ───────────────────────────── 开通状态查询（供巡检） ─────────────────────────────

// StuckTenant 表示一个卡在 provisioning 超时的租户。
type StuckTenant struct {
	ID        string
	Code      string
	Tier      tenant.IsolationTier
	CreatedAt time.Time
	AgeString string
}

// FindStuck 找出卡在 provisioning 超过 threshold 的租户。
//
// ★ 用途：开通失败的记录会被有意保留为 provisioning（便于重试），
//
//	但「有意保留」如果没有巡检，就退化成「永久垃圾记录」。
//	这里提供查询，让「半开通租户」可被看见、被重试或清理。
func (o *Orchestrator) FindStuck(ctx context.Context, threshold time.Duration) ([]StuckTenant, error) {
	rows, err := o.Platform.Query(ctx, `
		SELECT id::text, code, tier, created_at, now() - created_at
		  FROM dim_tenant
		 WHERE status = 'provisioning'
		   AND created_at < now() - $1::interval
		 ORDER BY created_at`, threshold.String())
	if err != nil {
		return nil, fmt.Errorf("provision: 查卡住租户失败: %w", err)
	}
	defer rows.Close()
	var out []StuckTenant
	for rows.Next() {
		var s StuckTenant
		var age time.Duration
		if err := rows.Scan(&s.ID, &s.Code, &s.Tier, &s.CreatedAt, &age); err != nil {
			return nil, err
		}
		s.AgeString = age.Truncate(time.Second).String()
		out = append(out, s)
	}
	return out, rows.Err()
}
