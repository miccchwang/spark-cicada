// Package templatestore —— M-TEMPLATE 的 Postgres 实现。
//
// 与 template 包的分工（与 group↔groupstore 同一纪律）：
//   * template 包 = 纯逻辑（校验、档位可见性、排序、默认选取），可穷举单测；
//   * 本包 = IO 搬运，把纯逻辑的读写落到 dim_view_template 等表。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本包最容易出错的地方是「把可见性判断写进 SQL」，那会制造第二份真相。
//
//   可见性判断只在 template.CanRead / CanReadTeamWithOwnerDept 里存在一份。
//   本包的做法是：
//     LoadVisibleFor  —— 只按**页面**粗筛（DB 层能做的、也是最便宜的一层），
//                        然后把候选全部捞回，交给纯函数逐条判可见。
//     OwnerDept        —— 只在 team 档「无显式分享」时才需要 owner 的部门，
//                        按需单查，不为所有模板都连表。
//
//   代价是「可能多读了不可见的行」。这是**刻意的取舍**：
//   模板总量小（每页每人几条），多读几十行的成本远低于
//   「两处可见性逻辑漂移导致越权泄露」的代价。
//
// 第二条纪律：**queryState 原样存取，绝不解释。**
//   它是前端契约定义的 JSON，后端只当不透明字节搬运（json.RawMessage）。
//   一旦后端开始解析它，就会长出第二份 QueryState 定义 —— 两套真相。
// ══════════════════════════════════════════════════════════════════════════
package templatestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/template"
	"github.com/miccchwang/spark-cicada/backend/internal/tenant"
)

// Store 模板的读写入口。
//
// ══════════════════════════════════════════════════════════════════════════
// ★★ 租户上下文（Task #57）
//
//	本 store 的所有 SQL 都作用于 `dim_view_template` / `dim_view_template_share`，
//	这两张表在 0011 迁移后都开了 RLS（策略读 `rls_tenant_id()`）。
//	但 **RLS 只在会话变量被设置时才过滤** —— 若 store 直接走连接池，
//	`app.tenant_id` 停在哨兵值，策略就过滤掉**所有**行（fail-closed 但不可用）；
//	或者更糟：若有人把哨兵默认值改成一个真租户，就会跨租户可见。
//
//	因此本 store 有两种构造方式：
//	  · New(pool)            —— **平台级/单租户**：不施加租户，RLS 未启用。
//	                            仅用于单租户部署与迁移/运维路径。
//	  · NewForTenant(pool,tc) —— **多租户**：每次操作都在 tenant.InTenantTx 内，
//	                            先 SET LOCAL app.tenant_id，再由 RLS 过滤。
//
//	★ 为什么不给每个方法都加一个 tc 参数（更"显式"的做法）：
//	  那会让 8 个方法 × 2 档 = 16 条签名，且每个调用点都要传一遍 ——
//	  漏传一处就是一次静默越权，而且编译器不会报错（因为参数可空）。
//	  把租户**绑在 store 实例上**，则「有没有租户」在构造时就确定，
//	  调用点无从漏传。这与 InTenantTx「让漏掉 apply 成为不可能」是同一思路。
//
//	★ 但绑定也带来一个新风险：**同实例跨租户复用**。
//	  故 NewForTenant 返回的实例持有固定 tc，不提供任何「改租户」的方法 ——
//	  要换租户就换实例。这样「一个实例服务两租户」在类型上不可表达。
// ══════════════════════════════════════════════════════════════════════════
type Store struct {
	pool *pgxpool.Pool
	// tc 为 nil ⇒ 平台级（不施加租户，走裸池）；非 nil ⇒ 每次操作包租户事务。
	tc *tenant.TenantContext
}

// New 用已有连接池构造**平台级** store（不施加租户）。
//
// ★ 仅用于单租户部署与运维路径。多租户读写下必须用 NewForTenant，
//   否则 RLS 不会过滤（表已开 RLS ⇒ 裸池下会看到 0 行或取决于默认值）。
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// NewForTenant 构造**绑定租户**的 store：所有操作都在该租户的事务内执行。
//
// ★ tc 的 Tier 决定施加方式（shared ⇒ app.tenant_id；dedicated ⇒ search_path），
//   但**本 store 不感知档位差异** —— 差异全部由 tenant.InTenantTx 处理。
//   这正是「档位是存储细节、不是业务分支」这条铁律的落地。
//
// ★ 入参校验交给 tenant.InTenantTx（每方法入口都会校验一次）。
//   这里不重复校验：重复会让「校验规则」出现两份，迟早漂移。
func NewForTenant(pool *pgxpool.Pool, tc tenant.TenantContext) *Store {
	return &Store{pool: pool, tc: &tc}
}

// TenantBound 报告本实例是否绑定了租户（供 api 层做装配期自检）。
//
// ★ 用途：装配期断言「多租户配置下拿到的是绑定实例」。
//	这类断言应当在**启动时**失败，而不是等第一次查询返回空结果才发现。
func (s *Store) TenantBound() bool { return s != nil && s.tc != nil }

// ───────────────────────────── 租户感知的池访问 ─────────────────────────────
//
// ★ 下面三个 helper 是本 store 唯一的池访问入口。
//   所有方法都必须经它们 —— 直接写 s.pool 就等于绕过租户施加。
//   （可用 grep 校验：本文件除这三个 helper 外不应出现 `s.pool.`）

// qRows 执行一条「取多行」的查询（租户感知），把**未推进的**游标交给 scan。
//
// ══════════════════════════════════════════════════════════════════════════
// ★★★ 游标契约（务必读懂再改 —— 这里出过真实的静默丢行缺陷）：
//
//	scan 收到的 rows **停在第一行之前**，必须**自己**写推进循环：
//
//	    for rows.Next() { rows.Scan(...) }
//
//	本方法的职责只是「在租户事务内取到游标 + 保证 rows 被关闭 + 返回 rows.Err()」。
//	**绝不**在调用 scan 之前替它推进游标 —— 否则 scan 里再推一轮就变成
//	「每外层一次跳一行」，**恰好一行时读到 0 行且不报错**。
//
//	★ 反面案例（本次缺陷）：tenant.QueryInTenant 曾先 `for rows.Next()`
//	  推进一轮再调回调，而本包的 Load/LoadByPage/loadShares 回调又推进一轮，
//	  于是「有 WHERE 的单行读」全部返回 0 行。症状与 RLS 失效、
//	  计划缓存污染完全同形（都表现为空结果集），极难定位。
//	  ⇒ **游标只能被推进一次，且必须由回调推进。**
// ══════════════════════════════════════════════════════════════════════════
//
// ★ 这是本 store 唯一的**读**入口。所有单行读也走它（配 qOne）：
//
//	为什么不另外提供 qRow：单行读若返回 pgx.Row，事务必须在**扫描之后**
//	才能提交 —— 而调用方拿到 Row 时 InTenantTx 已经返回并提交了事务，
//	SET LOCAL 随之失效。这个生命周期错位极其隐蔽（查询可能仍返回正确结果，
//	因为数据已经读进 pgx 缓冲），但一旦查询被延迟执行就会漏掉租户约束。
//	统一走「物化到回调里」消除了这个时序问题。
func (s *Store) qRows(ctx context.Context, sql string,
	args []any, scan func(pgx.Rows) error) error {
	if s.tc == nil {
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		if err := scan(rows); err != nil {
			return err
		}
		return rows.Err()
	}
	return tenant.QueryInTenant(ctx, s.pool, *s.tc, sql, args, scan)
}

// qOne 执行一条**期望 0 或 1 行**的查询（租户感知），把唯一一行交给 scanRow。
//
// ★ scanRow 的契约与 qRows 的 scan **不同**（这是刻意的分层）：
//
//	qRows.scan —— 游标停在第一行之前，**回调自己**写 for rows.Next()。
//	qOne.scanRow —— 游标**已经停在那一行上**（由本方法推进），
//	                故 scanRow 里直接 rows.Scan(...) 即可，**不要**再 Next()。
//
//	★ 分层的意义：让「期望 0/1 行」的语义在本方法里**只有一份**实现
//	  （多行时报错、0 行时返回 ErrNoRows），调用方不必各自重复这段判断。
//
// 返回 pgx.ErrNoRows（若结果为空）—— 与 pgx.QueryRow 的行为一致，
// 便于上层沿用 errors.Is(err, pgx.ErrNoRows) 的判断。
func (s *Store) qOne(ctx context.Context, sql string, args []any,
	scanRow func(pgx.Rows) error) error {
	n := 0
	err := s.qRows(ctx, sql, args, func(rows pgx.Rows) error {
		for rows.Next() {
			n++
			if n > 1 {
				return fmt.Errorf("templatestore: 期望至多 1 行，实际更多")
			}
			if err := scanRow(rows); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// exec 执行一条写语句（租户感知）。
func (s *Store) exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if s.tc == nil {
		return s.pool.Exec(ctx, sql, args...)
	}
	var tag pgconn.CommandTag
	err := tenant.InTenantTx(ctx, s.pool, *s.tc,
		func(ctx context.Context, tx pgx.Tx) error {
			t, err := tx.Exec(ctx, sql, args...)
			tag = t
			return err
		})
	return tag, err
}

// inTx 在**租户事务**内执行 fn；平台档下则开一个普通事务。
//
// ★ 这是 Save / BumpUse / SetDefault 这类「多条语句必须同事务」的入口。
//	租户档下不能「先开普通事务、再设租户变量」—— 见 tx.go 的说明：
//	设置与使用必须在同一事务，否则 SET LOCAL 已随前一个事务失效。
func (s *Store) inTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if s.tc == nil {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	return tenant.InTenantTx(ctx, s.pool, *s.tc, fn)
}

// ───────────────────────────── 读 ─────────────────────────────

const cols = `id, name, scope, owner, page, query_state, columns, layout,
              use_count, is_default, template_ver, created_at, updated_at`

// Load 读回单个模板（含分享对象）。
func (s *Store) Load(ctx context.Context, id string) (*template.Template, error) {
	var t *template.Template
	err := s.qOne(ctx, `SELECT `+cols+` FROM dim_view_template WHERE id = $1`, []any{id},
		func(rows pgx.Rows) error {
			tt, err := scanTemplate(rows)
			if err != nil {
				return err
			}
			t = tt
			return nil
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("templatestore: %w: %s", template.ErrNotFound, id)
		}
		return nil, fmt.Errorf("templatestore: 读模板 %s: %w", id, err)
	}
	shares, err := s.loadShares(ctx, id)
	if err != nil {
		return nil, err
	}
	t.Shares = shares
	return t, nil
}

// LoadByPage 按页面粗筛候选（**不做可见性过滤** —— 那个由纯函数负责）。
//
// ★ 只取 personal（本人） + team + system 三档的全部行，
//   把过滤权交给 template.CanRead。见包注释的取舍说明。
//
// ★ 本方法的「多读」是**租户内**的多读：RLS 已把范围限在自己租户，
//   纯函数再从中筛可见性。两层各管一件事，不重叠。
func (s *Store) LoadByPage(ctx context.Context, page string, limit int) ([]*template.Template, error) {
	if limit <= 0 {
		limit = 500
	}
	out := []*template.Template{}
	ids := []string{}
	err := s.qRows(ctx, `
		SELECT `+cols+` FROM dim_view_template
		WHERE page = $1
		ORDER BY updated_at DESC
		LIMIT $2`, []any{page, limit}, func(rows pgx.Rows) error {
		for rows.Next() {
			t, err := scanTemplate(rows)
			if err != nil {
				return fmt.Errorf("templatestore: 扫描模板: %w", err)
			}
			out = append(out, t)
			ids = append(ids, t.ID)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("templatestore: 列模板 %s: %w", page, err)
	}
	// 批量补 shares（一次查全部，避免 N+1）
	shareMap, err := s.loadSharesBatch(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, t := range out {
		if sh, ok := shareMap[t.ID]; ok {
			t.Shares = sh
		}
	}
	return out, nil
}

// OwnerDept 取某账号的主部门 code（team 档「无显式分享」时的可见性依据）。
//
// 查不到时返回空串（调用方据此保守拒绝，而非报错 ——
// 「查不到部门」不应当让整个列表接口挂掉）。
func (s *Store) OwnerDept(ctx context.Context, account string) (string, error) {
	var dept *string
	err := s.qOne(ctx, `SELECT primary_dept FROM dim_org WHERE account = $1`, []any{account},
		func(rows pgx.Rows) error { return rows.Scan(&dept) })
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("templatestore: 读部门 %s: %w", account, err)
	}
	if dept == nil {
		return "", nil
	}
	return *dept, nil
}

// ViewerContext 一次性取齐可见性判断所需的上下文（组 + 部门 + 是否管理员）。
type ViewerContext struct {
	Groups        []string
	Dept          string
	IsSystemAdmin bool
}

// ResolveViewer 组装 viewer 上下文。
//
// isAdmin 由调用方注入判断（与 api.GroupHandlers.IsAdmin 同一纪律：
// 「何为管理员」会变，store 不该硬编码）。
func (s *Store) ResolveViewer(ctx context.Context, account string, isAdmin func(string) bool) (template.Viewer, error) {
	v := template.Viewer{Account: account}
	if isAdmin != nil {
		v.IsSystemAdmin = isAdmin(account)
	}
	// 组：一次查全部（含 owner 身份，owner 也是组内角色）
	v.Groups = []string{}
	err := s.qRows(ctx, `SELECT group_id FROM dim_group_member WHERE account = $1`,
		[]any{account}, func(rows pgx.Rows) error {
			for rows.Next() {
				var g string
				if err := rows.Scan(&g); err != nil {
					return fmt.Errorf("templatestore: 扫描组: %w", err)
				}
				v.Groups = append(v.Groups, g)
			}
			return nil
		})
	if err != nil {
		return v, fmt.Errorf("templatestore: 读组: %w", err)
	}
	// 部门
	dept, err := s.OwnerDept(ctx, account)
	if err != nil {
		return v, err
	}
	v.Dept = dept
	return v, nil
}

// VisibleFor 是接口层的主入口：取某页全部**可读**模板，按常用度排序。
//
// 步骤：粗筛（按页）→ 纯函数过滤可见 → 补 owner 部门（仅 team 无分享档）→ 排序。
func (s *Store) VisibleFor(ctx context.Context, viewer template.Viewer, page string) ([]*template.Template, error) {
	cand, err := s.LoadByPage(ctx, page, 0)
	if err != nil {
		return nil, err
	}
	// 需要 owner 部门的 team 模板：先收集 owner，批量查一次
	needDept := map[string]bool{}
	for _, t := range cand {
		if t.Scope == template.ScopeTeam && len(t.Shares) == 0 && t.Owner != viewer.Account {
			needDept[t.Owner] = true
		}
	}
	depts := map[string]string{}
	if len(needDept) > 0 {
		owners := make([]string, 0, len(needDept))
		for o := range needDept {
			owners = append(owners, o)
		}
		m, err := s.deptsOf(ctx, owners)
		if err != nil {
			return nil, err
		}
		depts = m
	}

	out := []*template.Template{}
	for _, t := range cand {
		ok := template.CanRead(t, viewer)
		if !ok && t.Scope == template.ScopeTeam && len(t.Shares) == 0 {
			ok = template.CanReadTeamWithOwnerDept(t, viewer, depts[t.Owner])
		}
		if ok {
			out = append(out, t)
		}
	}
	return template.SortByUseCountDesc(out), nil
}

// ───────────────────────────── 写 ─────────────────────────────

// Save 新建或更新模板（按 id upsert），并同步分享对象。
//
// ★ 用单事务包住「模板 + 分享」两处写入：
//   若只写了模板却漏写分享，team 档会退化成「同部门可见」，
//   等于**静默扩大了可见范围** —— 比写入失败更危险（失败至少看得见）。
func (s *Store) Save(ctx context.Context, t *template.Template, actor string) error {
	if problems := template.Validate(t); len(problems) > 0 {
		return fmt.Errorf("%w: %v", template.ErrInvalid, problems)
	}
	if t.Version == "" {
		t.Version = template.Version
	}
	// ★ 事务入口统一走 s.inTx：租户档下它会先 SET LOCAL app.tenant_id，
	//   再在**同一事务**里跑 saveInTx 的所有语句。
	//   若这里直接 s.pool.Begin 再往下跑，RLS 会话变量根本没设 ——
	//   等于在写入路径上绕过租户（表已开 RLS，写入要么被拒要么落到哨兵）。
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.saveInTx(ctx, tx, t)
	})
}

// saveInTx 是 Save 的事务体（拆出来以便租户档复用同一段 SQL）。
func (s *Store) saveInTx(ctx context.Context, tx pgx.Tx, t *template.Template) error {
	queryState := t.QueryState
	if len(queryState) == 0 {
		queryState = []byte(`{}`)
	}
	colsJSON, err := json.Marshal(nonNilCols(t.Columns))
	if err != nil {
		return fmt.Errorf("templatestore: 编码列偏好: %w", err)
	}
	layoutJSON, err := json.Marshal(layoutOrEmpty(t.Layout))
	if err != nil {
		return fmt.Errorf("templatestore: 编码布局: %w", err)
	}

	// ★ 「设默认」的唯一性由部分唯一索引保证；upsert 时若把 is_default 置 true，
	//   必须先在**同一事务**里清掉同 (scope,owner,page) 下的旧默认，
	//   否则会撞 uq_view_template_default_per_owner_page。
	if t.IsDefault {
		if _, err := tx.Exec(ctx, `
			UPDATE dim_view_template SET is_default = false, updated_at = now()
			WHERE scope = $1 AND owner = $2 AND page = $3 AND is_default AND id <> $4`,
			string(t.Scope), t.Owner, t.Page, t.ID); err != nil {
			return fmt.Errorf("templatestore: 清除旧默认: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO dim_view_template
			(id, name, scope, owner, page, query_state, columns, layout,
			 use_count, is_default, template_ver, created_at, updated_at, tenant_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now(), now(), $12)
		ON CONFLICT (id) DO UPDATE SET
			name         = EXCLUDED.name,
			scope        = EXCLUDED.scope,
			page         = EXCLUDED.page,
			query_state  = EXCLUDED.query_state,
			columns      = EXCLUDED.columns,
			layout       = EXCLUDED.layout,
			is_default   = EXCLUDED.is_default,
			template_ver = EXCLUDED.template_ver,
			updated_at   = now()`,
		t.ID, t.Name, string(t.Scope), t.Owner, t.Page,
		queryState, colsJSON, layoutJSON,
		t.UseCount, t.IsDefault, t.Version, tenantIDOrNil(s.tc)); err != nil {
		return fmt.Errorf("templatestore: 写模板 %s: %w", t.ID, err)
	}

	// 分享对象：先删后插（全量替换语义，与 groupstore.ReplaceGrants 一致）
	if _, err := tx.Exec(ctx, `DELETE FROM dim_view_template_share WHERE template_id = $1`, t.ID); err != nil {
		return fmt.Errorf("templatestore: 清分享: %w", err)
	}
	for _, sh := range t.Shares {
		if _, err := tx.Exec(ctx, `
			INSERT INTO dim_view_template_share (template_id, subject_kind, subject_id, tenant_id)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT DO NOTHING`, t.ID, sh.SubjectKind, sh.SubjectID, tenantIDOrNil(s.tc)); err != nil {
			return fmt.Errorf("templatestore: 写分享: %w", err)
		}
	}
	return nil
}

// tenantIDOrNil 返回 tenant_id 列的写入值。
//
// ★ 平台档（未绑租户）返回 nil ⇒ 列写 NULL。
//
//	NULL 行在 RLS 下对**所有租户不可见**（`NULL = uuid` 求值为 NULL）。
//	这是刻意的：平台级模板不应被任意租户看到。
//	若将来需要「平台模板对所有租户可见」，那是**另一套策略**
//	（例如再加一条 `OR tenant_id IS NULL` 的 policy），而不是在写入端
//	编一个租户 uuid —— 那会把平台数据伪装成某个租户的私有数据。
func tenantIDOrNil(tc *tenant.TenantContext) any {
	if tc == nil {
		return nil
	}
	return tc.TenantID
}

// Delete 删除模板（分享由 ON DELETE CASCADE 带走）。
//
// ★ 必须在**同一语句**里带上归属条件，不能「先查再删」——
//   否则两次查询之间归属可能被改，出现 TOCTOU。
//   返回是否真的删掉了一行（用于把「不存在」与「无权」分开报）。
func (s *Store) Delete(ctx context.Context, id, owner string, isAdmin bool) (bool, error) {
	// ★ 管理员与实际归属走不同 WHERE：非管理员带上 owner 条件，
	//   这样「删他人的模板」在 SQL 层就被挡住，不依赖调用方先查再判。
	//   注意只执行其中一条 —— 若先按 owner 删再按 id 删，管理员会多跑一条
	//   误删语句（且 tag 被覆盖后也可能误报）。
	sql := `DELETE FROM dim_view_template WHERE id = $1 AND owner = $2`
	args := []any{id, owner}
	if isAdmin {
		sql = `DELETE FROM dim_view_template WHERE id = $1`
		args = []any{id}
	}
	tag, err := s.exec(ctx, sql, args...)
	if err != nil {
		return false, fmt.Errorf("templatestore: 删模板 %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		// 无法区分「不存在」与「非本人」——都按找不到处理，避免探测他人模板是否存在
		return false, nil
	}
	return true, nil
}

// BumpUse 记录一次套用：use_count + 1（供常用排序与推荐）。
//
// ★ 同时写 audit_log（沿用 0001 的 append-only 表）：
//   模板是「批量改口径」的载体，套用行为必须留痕，事后能查「谁在何时套过」。
//
// ★ 审计行必须带 tenant_id（Task #57）：audit_log 在 0011 后开了 RLS，
//   不带租户的审计行在 RLS 下对所有租户不可见 —— 等于**审计留痕但查不到**，
//   比不记更糟（会让人误以为没有这条记录）。
//   注意：加租户**不是**为了把它藏起来，而是让它归属于产生它的租户。
func (s *Store) BumpUse(ctx context.Context, id, actor string) error {
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE dim_view_template SET use_count = use_count + 1, updated_at = now() WHERE id = $1`, id); err != nil {
			return fmt.Errorf("templatestore: 累加使用次数: %w", err)
		}
		detail, _ := json.Marshal(map[string]string{"templateId": id})
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_log (actor, action, target, detail, tenant_id)
			VALUES ($1, 'view_template.apply', $2, $3, $4)`,
			actor, id, detail, tenantIDOrNil(s.tc)); err != nil {
			return fmt.Errorf("templatestore: 记审计: %w", err)
		}
		return nil
	})
}

// SetDefault 把某模板设为「本页默认」。
//
// ★ 两步（清旧 + 置新）在同一事务：并发下也不可能出现两个默认
//   （部分唯一索引是最后一道保险）。
func (s *Store) SetDefault(ctx context.Context, id string) error {
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var scope, owner, page string
		err := tx.QueryRow(ctx,
			`SELECT scope, owner, page FROM dim_view_template WHERE id = $1 FOR UPDATE`, id).
			Scan(&scope, &owner, &page)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("templatestore: %w: %s", template.ErrNotFound, id)
			}
			return fmt.Errorf("templatestore: 读模板 %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE dim_view_template SET is_default = false, updated_at = now()
			WHERE scope = $1 AND owner = $2 AND page = $3 AND is_default`, scope, owner, page); err != nil {
			return fmt.Errorf("templatestore: 清除旧默认: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE dim_view_template SET is_default = true, updated_at = now() WHERE id = $1`, id); err != nil {
			return fmt.Errorf("templatestore: 置默认: %w", err)
		}
		return nil
	})
}

// ───────────────────────────── 扫描 / 帮手 ─────────────────────────────

// rowScanner 兼容 pgx.Row 与 pgx.Rows。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTemplate(r rowScanner) (*template.Template, error) {
	var (
		t          template.Template
		scope      string
		queryState []byte
		colsJS     []byte
		layoutJS   []byte
	)
	if err := r.Scan(
		&t.ID, &t.Name, &scope, &t.Owner, &t.Page,
		&queryState, &colsJS, &layoutJS,
		&t.UseCount, &t.IsDefault, &t.Version, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, err
	}
	t.Scope = template.Scope(scope)
	// queryState 原样透传（不解释）
	t.QueryState = json.RawMessage(queryState)
	t.Columns = decodeColumns(colsJS)
	t.Layout = decodeLayout(layoutJS)
	return &t, nil
}

func decodeColumns(raw []byte) []template.ColumnPref {
	out := []template.ColumnPref{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// 非法 JSON 不当作致命错误：返回空列偏好（前端会用默认列），
		// 但绝不 panic —— 一条坏模板不该打挂整个列表接口。
		return []template.ColumnPref{}
	}
	return out
}

func decodeLayout(raw []byte) template.LayoutPref {
	out := template.LayoutPref{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return template.LayoutPref{}
	}
	return out
}

func nonNilCols(in []template.ColumnPref) []template.ColumnPref {
	if in == nil {
		return []template.ColumnPref{}
	}
	return in
}

func layoutOrEmpty(in template.LayoutPref) template.LayoutPref {
	if in.Expanded == nil {
		in.Expanded = map[string]bool{}
	}
	return in
}

type shareRow struct {
	templateID  string
	subjectKind string
	subjectID   string
}

func (s *Store) loadShares(ctx context.Context, templateID string) ([]template.Share, error) {
	out := []template.Share{}
	err := s.qRows(ctx, `
		SELECT subject_kind, subject_id FROM dim_view_template_share
		WHERE template_id = $1 ORDER BY subject_kind, subject_id`, []any{templateID},
		func(rows pgx.Rows) error {
			for rows.Next() {
				var sh template.Share
				if err := rows.Scan(&sh.SubjectKind, &sh.SubjectID); err != nil {
					return err
				}
				out = append(out, sh)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("templatestore: 读分享 %s: %w", templateID, err)
	}
	return out, nil
}

func (s *Store) loadSharesBatch(ctx context.Context, ids []string) (map[string][]template.Share, error) {
	out := map[string][]template.Share{}
	if len(ids) == 0 {
		return out, nil
	}
	err := s.qRows(ctx, `
		SELECT template_id, subject_kind, subject_id FROM dim_view_template_share
		WHERE template_id = ANY($1) ORDER BY template_id, subject_kind, subject_id`, []any{ids},
		func(rows pgx.Rows) error {
			for rows.Next() {
				var r shareRow
				if err := rows.Scan(&r.templateID, &r.subjectKind, &r.subjectID); err != nil {
					return err
				}
				out[r.templateID] = append(out[r.templateID], template.Share{
					SubjectKind: r.subjectKind, SubjectID: r.subjectID,
				})
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("templatestore: 批量读分享: %w", err)
	}
	return out, nil
}

// deptsOf 批量取多个账号的主部门。
func (s *Store) deptsOf(ctx context.Context, accounts []string) (map[string]string, error) {
	out := map[string]string{}
	if len(accounts) == 0 {
		return out, nil
	}
	type kv struct{ a, d string }
	var kvs []kv
	err := s.qRows(ctx,
		`SELECT account, primary_dept FROM dim_org WHERE account = ANY($1)`, []any{accounts},
		func(rows pgx.Rows) error {
			for rows.Next() {
				var a string
				var d *string
				if err := rows.Scan(&a, &d); err != nil {
					return err
				}
				if d != nil {
					kvs = append(kvs, kv{a, *d})
				}
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("templatestore: 批量读部门: %w", err)
	}
	// 稳定写入（并对 key 排序，避免 map 迭代顺序影响后续判断——虽然这里不影响结果）
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].a < kvs[j].a })
	for _, p := range kvs {
		out[p.a] = p.d
	}
	return out, nil
}
