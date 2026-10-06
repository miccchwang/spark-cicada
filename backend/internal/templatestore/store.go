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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/template"
)

// Store 模板的读写入口。
type Store struct{ pool *pgxpool.Pool }

// New 用已有连接池构造。
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// ───────────────────────────── 读 ─────────────────────────────

const cols = `id, name, scope, owner, page, query_state, columns, layout,
              use_count, is_default, template_ver, created_at, updated_at`

// Load 读回单个模板（含分享对象）。
func (s *Store) Load(ctx context.Context, id string) (*template.Template, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+cols+` FROM dim_view_template WHERE id = $1`, id)
	t, err := scanTemplate(row)
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
func (s *Store) LoadByPage(ctx context.Context, page string, limit int) ([]*template.Template, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+cols+` FROM dim_view_template
		WHERE page = $1
		ORDER BY updated_at DESC
		LIMIT $2`, page, limit)
	if err != nil {
		return nil, fmt.Errorf("templatestore: 列模板 %s: %w", page, err)
	}
	defer rows.Close()

	out := []*template.Template{}
	ids := []string{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, fmt.Errorf("templatestore: 扫描模板: %w", err)
		}
		out = append(out, t)
		ids = append(ids, t.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("templatestore: 遍历模板: %w", err)
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
	err := s.pool.QueryRow(ctx, `SELECT primary_dept FROM dim_org WHERE account = $1`, account).Scan(&dept)
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
	rows, err := s.pool.Query(ctx, `SELECT group_id FROM dim_group_member WHERE account = $1`, account)
	if err != nil {
		return v, fmt.Errorf("templatestore: 读组: %w", err)
	}
	defer rows.Close()
	v.Groups = []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return v, fmt.Errorf("templatestore: 扫描组: %w", err)
		}
		v.Groups = append(v.Groups, g)
	}
	if err := rows.Err(); err != nil {
		return v, fmt.Errorf("templatestore: 遍历组: %w", err)
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

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("templatestore: 开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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
			 use_count, is_default, template_ver, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now(), now())
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
		t.UseCount, t.IsDefault, t.Version); err != nil {
		return fmt.Errorf("templatestore: 写模板 %s: %w", t.ID, err)
	}

	// 分享对象：先删后插（全量替换语义，与 groupstore.ReplaceGrants 一致）
	if _, err := tx.Exec(ctx, `DELETE FROM dim_view_template_share WHERE template_id = $1`, t.ID); err != nil {
		return fmt.Errorf("templatestore: 清分享: %w", err)
	}
	for _, sh := range t.Shares {
		if _, err := tx.Exec(ctx, `
			INSERT INTO dim_view_template_share (template_id, subject_kind, subject_id)
			VALUES ($1,$2,$3)
			ON CONFLICT DO NOTHING`, t.ID, sh.SubjectKind, sh.SubjectID); err != nil {
			return fmt.Errorf("templatestore: 写分享: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("templatestore: 提交: %w", err)
	}
	return nil
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
	tag, err := s.pool.Exec(ctx, sql, args...)
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
func (s *Store) BumpUse(ctx context.Context, id, actor string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("templatestore: 开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE dim_view_template SET use_count = use_count + 1, updated_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("templatestore: 累加使用次数: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"templateId": id})
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (actor, action, target, detail)
		VALUES ($1, 'view_template.apply', $2, $3)`, actor, id, detail); err != nil {
		return fmt.Errorf("templatestore: 记审计: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("templatestore: 提交: %w", err)
	}
	return nil
}

// SetDefault 把某模板设为「本页默认」。
//
// ★ 两步（清旧 + 置新）在同一事务：并发下也不可能出现两个默认
//   （部分唯一索引是最后一道保险）。
func (s *Store) SetDefault(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("templatestore: 开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var scope, owner, page string
	err = tx.QueryRow(ctx,
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
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("templatestore: 提交: %w", err)
	}
	return nil
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
	rows, err := s.pool.Query(ctx, `
		SELECT subject_kind, subject_id FROM dim_view_template_share
		WHERE template_id = $1 ORDER BY subject_kind, subject_id`, templateID)
	if err != nil {
		return nil, fmt.Errorf("templatestore: 读分享 %s: %w", templateID, err)
	}
	defer rows.Close()
	out := []template.Share{}
	for rows.Next() {
		var sh template.Share
		if err := rows.Scan(&sh.SubjectKind, &sh.SubjectID); err != nil {
			return nil, fmt.Errorf("templatestore: 扫描分享: %w", err)
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (s *Store) loadSharesBatch(ctx context.Context, ids []string) (map[string][]template.Share, error) {
	out := map[string][]template.Share{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT template_id, subject_kind, subject_id FROM dim_view_template_share
		WHERE template_id = ANY($1) ORDER BY template_id, subject_kind, subject_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("templatestore: 批量读分享: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r shareRow
		if err := rows.Scan(&r.templateID, &r.subjectKind, &r.subjectID); err != nil {
			return nil, fmt.Errorf("templatestore: 扫描分享: %w", err)
		}
		out[r.templateID] = append(out[r.templateID], template.Share{
			SubjectKind: r.subjectKind, SubjectID: r.subjectID,
		})
	}
	return out, rows.Err()
}

// deptsOf 批量取多个账号的主部门。
func (s *Store) deptsOf(ctx context.Context, accounts []string) (map[string]string, error) {
	out := map[string]string{}
	if len(accounts) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT account, primary_dept FROM dim_org WHERE account = ANY($1)`, accounts)
	if err != nil {
		return nil, fmt.Errorf("templatestore: 批量读部门: %w", err)
	}
	defer rows.Close()
	type kv struct{ a, d string }
	var kvs []kv
	for rows.Next() {
		var a string
		var d *string
		if err := rows.Scan(&a, &d); err != nil {
			return nil, fmt.Errorf("templatestore: 扫描部门: %w", err)
		}
		if d != nil {
			kvs = append(kvs, kv{a, *d})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("templatestore: 遍历部门: %w", err)
	}
	// 稳定写入（并对 key 排序，避免 map 迭代顺序影响后续判断——虽然这里不影响结果）
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].a < kvs[j].a })
	for _, p := range kvs {
		out[p.a] = p.d
	}
	return out, nil
}
