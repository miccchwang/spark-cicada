// Package groupstore —— M-GROUP / M-REQ 的 Postgres 实现。
//
// 与 group / req 包的分工（与 collect↔collectstore 同一纪律）：
//   * group / req 包 = 纯逻辑（求值、DENY 判定、路由、状态机），可穷举单测；
//   * 本包 = IO 搬运，把纯逻辑的读写落到**既有**的 dim_/fact_ 表。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 表结构来自迁移 0001，不是本包自创（一次真实事故的结论）
//
//   初版本包用的是自创的 dim_user_group / fact_group_membership /
//   fact_group_grant / dim_account_entitlement —— 而迁移 0001 **早已**建好
//   M-GROUP / M-REQ 的表，且 0001 的设计与契约逐字对应：
//
//     dim_group               ← contracts/user-group.ts  UserGroup{grants jsonb, owners}
//     dim_group_member        ← GroupMembership{role, inheritsGrants}
//     fact_entitlement        ← contracts/entitlement.ts  Entitlement
//     fact_permission_request ← contracts/permission-request.ts
//                               PermissionRequest{approvals[], ccs[]}
//
//   两套并行结构就是「两套真相」：界面读 jsonb、后台写子表，
//   于是权限漂移，且 DENY 以谁为准无法回答。
//   ⇒ 本包统一走 0001 的表；0006 只做增量补强（约束 / 索引 / 变更流水）。
// ══════════════════════════════════════════════════════════════════════════
//
// ★ 三条纪律（都来自真实事故）：
//
//  1. **DENY 与 ALLOW 必须在同一事务/快照里被读出来。**
//     若先查允许、再查拒绝，两条查询之间若有并发写入（管理员刚加了一条禁用），
//     求值结果就是「拒绝尚未生效」的过期视图 —— 本该被否决的权限被授了出去。
//
//  2. **nil 切片入库前必须显式转成空切片。**
//     真实事故（M-COLLECT，SQLSTATE 23502）：`text[] NOT NULL DEFAULT '{}'`
//     的 DEFAULT 只在**不提供该列**时生效；显式传 NULL 会直接撞 NOT NULL，
//     而 Go 的 nil []string 经 pgx 正是被编码成 SQL NULL。
//
//  3. **inherits_grants 必须与成员行同源读出**，不做二次补查 ——
//     两次查询之间成员被设为「退出继承」，就会按旧值（继承）去授权，属于扩权。
package groupstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/group"
)

// Store 分组与授权的读写入口。
type Store struct{ pool *pgxpool.Pool }

// New 用已有连接池构造。
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// ───────────────────────────── 组授权 jsonb 的编码 ─────────────────────────────

// groupGrantsJSON 是 dim_group.grants 的落库形状。
//
// 字段与 contracts/user-group.ts 的 GroupGrants 一一对应，
// 外加 `isIT` —— 0006 的 D7 约束要用它判身份（刻意不用组名猜，见 0006 注释）。
type groupGrantsJSON struct {
	Modules               []string `json:"modules"`
	Dimensions            []dimJSON `json:"dimensions"`
	MaxLevel              string   `json:"maxLevel,omitempty"`
	CanViewBusinessValues bool     `json:"canViewBusinessValues"`
	IsIT                  bool     `json:"isIT,omitempty"`
}

type dimJSON struct {
	Dim                string   `json:"dim"`
	Values             []string `json:"values"`
	IncludeDescendants bool     `json:"includeDescendants,omitempty"`
}

// ───────────────────────────── 读：组 + 授权 ─────────────────────────────

// LoadGroup 读回一个组及其全部授权 —— **单条查询**，单一快照。
func (s *Store) LoadGroup(ctx context.Context, id string) (*group.Group, error) {
	var (
		g        group.Group
		desc     *string
		grantsJS []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, description, owners, grants, created_at, updated_at
		FROM dim_group WHERE id = $1`, id).
		Scan(&g.ID, &g.Name, &desc, &g.Owners, &grantsJS, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("groupstore: 组 %s 不存在", id)
		}
		return nil, fmt.Errorf("groupstore: 读组 %s: %w", id, err)
	}
	if desc != nil {
		g.Description = *desc
	}
	if g.Owners == nil {
		g.Owners = []string{}
	}
	g.Grants = decodeGrants(grantsJS)
	return &g, nil
}

// decodeGrants 把 dim_group.grants 的 jsonb 解码成 []group.Grant。
//
// ★ 这里是「jsonb ↔ 纯逻辑结构」的唯一转换点。
//   集中在一处很重要：若散落到多个查询里，迟早出现
//   「某处解出了 DENY、某处漏了」的不一致 —— 而 DENY 漏解 = 静默扩权。
func decodeGrants(raw []byte) []group.Grant {
	out := []group.Grant{}
	if len(raw) == 0 {
		return out
	}
	var g groupGrantsJSON
	if err := json.Unmarshal(raw, &g); err != nil {
		// 解码失败返回空授权（fail-closed），不 panic。
		// 宁可「这个人暂时什么都看不到」，也不可「因为解析坏了就全放行」。
		return out
	}
	for _, m := range g.Modules {
		out = append(out, group.Grant{
			Kind: group.KindModule, Key: "module." + m,
		})
	}
	for _, d := range g.Dimensions {
		out = append(out, group.Grant{
			Kind: group.KindDimension,
			Key:  "dimension." + stripPrefix(d.Dim, "dimension."),
			Values: d.Values, IncludeDescendants: d.IncludeDescendants,
		})
	}
	if g.MaxLevel != "" {
		out = append(out, group.Grant{Kind: group.KindLevel, Key: "level." + g.MaxLevel})
	}
	return out
}

// LoadGroupsFor 读回某账号所属的全部组（含每个组的授权）与成员关系。
//
// 这是求值器的唯一入口：一次拿到 (成员关系 ∪ 组 ∪ 组授权)，
// 使 group.Evaluate 在**同一快照**内完成「并集 + DENY」，不跨查询。
func (s *Store) LoadGroupsFor(ctx context.Context, account string) ([]*group.Group, []group.Membership, error) {
	// ① 成员关系：一次取回 inherits_grants，不做二次补查（见包注释纪律 3）。
	mrows, err := s.pool.Query(ctx, `
		SELECT m.group_id, m.account, m.role, m.inherits_grants, m.joined_at,
		       g.name, COALESCE(g.description, ''), g.owners, g.grants,
		       g.created_at, g.updated_at
		FROM dim_group_member m
		JOIN dim_group g ON g.id = m.group_id
		WHERE m.account = $1
		ORDER BY m.group_id`, account)
	if err != nil {
		return nil, nil, fmt.Errorf("groupstore: 读成员关系 %s: %w", account, err)
	}
	defer mrows.Close()

	var memberships []group.Membership
	var groups []*group.Group
	for mrows.Next() {
		var (
			m        group.Membership
			name     string
			desc     string
			owners   []string
			grantsJS []byte
			cAt, uAt time.Time
		)
		if err := mrows.Scan(&m.GroupID, &m.Account, &m.Role, &m.InheritsGrants, &m.JoinedAt,
			&name, &desc, &owners, &grantsJS, &cAt, &uAt); err != nil {
			return nil, nil, fmt.Errorf("groupstore: 扫描成员关系: %w", err)
		}
		memberships = append(memberships, m)
		if owners == nil {
			owners = []string{}
		}
		groups = append(groups, &group.Group{
			ID: m.GroupID, Name: name, Description: desc,
			Owners: owners, Grants: decodeGrants(grantsJS),
			CreatedAt: cAt, UpdatedAt: uAt,
		})
	}
	if err := mrows.Err(); err != nil {
		return nil, nil, fmt.Errorf("groupstore: 遍历成员关系 %s: %w", account, err)
	}
	return groups, memberships, nil
}

// ───────────────────────────── 读：个人权威 ─────────────────────────────

// LoadPersonal 读回账号的个人勾选（fact_entitlement）。
//
// 不存在时返回**零值**而非 error：所有账号都可被视为「有个人基线」，只是为空。
// 返回 error 会迫使每个调用点写分支，且容易把「新账号」误判成「读取失败」，
// 从而 fail-open（错误分支里放行）或 fail-closed 到错误一侧。
func (s *Store) LoadPersonal(ctx context.Context, account string) (*group.Personal, error) {
	p := &group.Personal{
		Account:       account,
		BaseTemplate:  "tpl.base",
		Modules:       []string{},
		Dimensions:    map[string][]string{},
		DataUseGroups: []string{},
		Denies:        []string{},
		MaxLevel:      group.L1,
	}

	var (
		baseTemplate *string
		modulesJS    []byte
		dimsJS       []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT base_template, modules, dimensions, max_level, can_view_business_values
		FROM fact_entitlement WHERE account = $1`, account).
		Scan(&baseTemplate, &modulesJS, &dimsJS, &p.MaxLevel, &p.CanViewBusinessValues)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return p, nil // 未登记 = 空基线，不是错误
		}
		return nil, fmt.Errorf("groupstore: 读个人权威 %s: %w", account, err)
	}
	if baseTemplate != nil {
		p.BaseTemplate = *baseTemplate
	}
	if len(modulesJS) > 0 {
		// modules jsonb 形状（0001）：{"enabled":[...],"disabled":[...]}
		var m struct {
			Enabled  []string `json:"enabled"`
			Disabled []string `json:"disabled"`
		}
		if err := json.Unmarshal(modulesJS, &m); err == nil {
			p.Modules = m.Enabled
			// disabled 的模块就是个人 DENY（kind:key 形态）
			for _, d := range m.Disabled {
				p.Denies = append(p.Denies, "module:"+d)
			}
		}
	}
	if len(dimsJS) > 0 {
		// dimensions jsonb 形状（0001）：DimensionGrant[]
		var ds []dimJSON
		if err := json.Unmarshal(dimsJS, &ds); err == nil {
			for _, d := range ds {
				p.Dimensions[d.Dim] = d.Values
			}
		}
	}
	return p, nil
}

// EntitlementsFunc 构造 req.Service 所需的授权加载器。
//
// ★ 实现要点：把「个人权威」与「组权威」都映射成 authz.Entitlement，
//   让 req 包继续复用既有的 authz.Resolver 求值路径 —— 而不是在 req 里
//   再写一套合并逻辑（两套合并逻辑必然漂移）。
func (s *Store) EntitlementsFunc() func(account string) (*authz.Entitlement, []*authz.Entitlement) {
	return func(account string) (*authz.Entitlement, []*authz.Entitlement) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		p, err := s.LoadPersonal(ctx, account)
		if err != nil {
			// ★ fail-closed：读不到就返回**空授权**而非 nil。
			//   返回 nil 会让 Resolver 收到 nil（既有约定是「不应收到 nil」）。
			return &authz.Entitlement{Account: account, MaxLevel: authz.L1}, nil
		}
		base := &authz.Entitlement{
			Account:               p.Account,
			BaseTemplate:          p.BaseTemplate,
			MaxLevel:              toAuthzLevel(p.MaxLevel),
			CanViewBusinessValues: p.CanViewBusinessValues,
		}
		for _, m := range p.Modules {
			base.Modules.Enabled = append(base.Modules.Enabled, m)
		}
		for _, d := range sortedDimKeys(p.Dimensions) {
			base.Dimensions = append(base.Dimensions, authz.DimensionGrant{
				Dim: d, Values: p.Dimensions[d],
			})
		}

		groups, memberships, err := s.LoadGroupsFor(ctx, account)
		if err != nil {
			return base, nil
		}
		// ★ Evaluate 要 []*Membership：显式取址，避免把切片元素
		//   直接当指针用（&memberships[i] 才稳定，&m 是循环变量的地址）。
		mptrs := make([]*group.Membership, len(memberships))
		for i := range memberships {
			mptrs[i] = &memberships[i]
		}
		ev := group.Evaluate(p, groups, mptrs)

		// 把求值结果里的 DENY 落到 ModuleGrant.Disabled，
		// 让 Resolver 的既有 DENY 通道生效 —— 保持「单一求值真相」。
		extra := &authz.Entitlement{Account: account, MaxLevel: toAuthzLevel(ev.MaxLevel)}
		for _, dd := range ev.Denies {
			if m, ok := splitDenyModule(dd); ok {
				extra.Modules.Disabled = append(extra.Modules.Disabled, m)
			}
		}
		return base, []*authz.Entitlement{extra}
	}
}

// ───────────────────────────── 写：组定义 ─────────────────────────────

// UpsertGroup 插入或更新组定义（幂等，用于声明式同步）。
func (s *Store) UpsertGroup(ctx context.Context, g *group.Group) error {
	if g == nil {
		return errors.New("groupstore: 组为空")
	}
	grantsJS, err := encodeGrants(g.Grants, g.Restricted)
	if err != nil {
		return fmt.Errorf("groupstore: 编码组授权 %s: %w", g.ID, err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO dim_group (id, name, description, grants, owners, updated_at)
		VALUES ($1,$2,$3,$4::jsonb,$5, now())
		ON CONFLICT (id) DO UPDATE SET
			name        = EXCLUDED.name,
			description = EXCLUDED.description,
			grants      = EXCLUDED.grants,
			owners      = EXCLUDED.owners,
			updated_at  = now()`,
		g.ID, g.Name, nullable(g.Description), grantsJS, nonNil(g.Owners))
	if err != nil {
		return fmt.Errorf("groupstore: upsert 组 %s: %w", g.ID, err)
	}
	return nil
}

// encodeGrants 把 []group.Grant 编码成 dim_group.grants 的 jsonb。
func encodeGrants(grants []group.Grant, restricted bool) ([]byte, error) {
	g := groupGrantsJSON{
		Modules:    []string{},
		Dimensions: []dimJSON{},
		IsIT:       restricted,
	}
	for _, gr := range grants {
		switch gr.Kind {
		case group.KindModule:
			g.Modules = append(g.Modules, stripPrefix(gr.Key, "module."))
		case group.KindDimension:
			g.Dimensions = append(g.Dimensions, dimJSON{
				Dim:                stripPrefix(gr.Key, "dimension."),
				Values:             nonNil(gr.Values),
				IncludeDescendants: gr.IncludeDescendants,
			})
		case group.KindLevel:
			g.MaxLevel = stripPrefix(gr.Key, "level.")
		case group.KindDataUseGroup:
			// 勾选组（数据用途组）在 0001 里由 fact_entitlement.data_use_groups 承担，
			// 不属于 dim_group.grants 的字段。此处刻意忽略而非塞进 jsonb ——
			// 塞进去会造出契约里没有的字段，是「悄悄扩契约」。
			continue
		}
	}
	return json.Marshal(g)
}

// ReplaceGrants 用给定集合**整体替换**某组的授权。
//
// ★ 为什么是「整体替换」而非「逐条 upsert」：
//   逐条 upsert 无法表达「撤销」——被删掉的那条会永久留在 grants 里，
//   于是界面上取消勾选后权限**依然生效**。
//
// ★ 顺带写 fact_group_grant_change 变更流水（0006 新增表）：
//   审计要回答「谁在什么时候给哪个组加了什么」，只有 jsonb 快照是答不了的
//   （读的人要自己做 diff，而 diff 在并发修改下不可靠）。
//   流水与主表**同事务**提交，避免「改了权限但没留痕」。
func (s *Store) ReplaceGrants(ctx context.Context, groupID string, grants []group.Grant, actor, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("groupstore: 开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 读旧值（同事务内 ⇒ 快照一致）
	var oldJS []byte
	if err := tx.QueryRow(ctx,
		`SELECT grants FROM dim_group WHERE id = $1 FOR UPDATE`, groupID).Scan(&oldJS); err != nil {
		return fmt.Errorf("groupstore: 锁读组 %s: %w", groupID, err)
	}
	old := decodeGrants(oldJS)
	newAll := filterNonDeny(grants)
	// ★ 必须走 encodeGrants 编码成 dim_group.grants 的形状（GroupGrants），
	//   不能直接 marshal []group.Grant —— 后者带的是 Go 字段名（Kind/Key/Values），
	//   与库里读回时的解码形状不一致，会造成「写进去读不出来」（静默丢授权）。
	newEnc, err := encodeGrants(newAll, groupIsIT(oldJS))
	if err != nil {
		return fmt.Errorf("groupstore: 编码组授权 %s: %w", groupID, err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE dim_group SET grants = $2::jsonb, updated_at = now() WHERE id = $1`,
		groupID, newEnc); err != nil {
		return fmt.Errorf("groupstore: 写组授权 %s: %w", groupID, err)
	}

	// 落变更流水：差集 → GRANT / REVOKE 各一条
	for _, gr := range diffGrants(old, newAll) {
		action := "GRANT"
		if gr.Revoked {
			action = "REVOKE"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO fact_group_grant_change
				(group_id, action, kind, key, values, actor, reason)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			groupID, action, gr.Grant.Kind, gr.Grant.Key,
			nonNil(gr.Grant.Values), actor, nullable(reason)); err != nil {
			return fmt.Errorf("groupstore: 落变更流水（%s:%s）: %w", gr.Grant.Kind, gr.Grant.Key, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("groupstore: 提交授权替换 %s: %w", groupID, err)
	}
	return nil
}

// SetMembership 插入或更新成员关系（含继承开关）。
func (s *Store) SetMembership(ctx context.Context, m group.Membership) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO dim_group_member (group_id, account, role, inherits_grants)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (group_id, account) DO UPDATE SET
			role            = EXCLUDED.role,
			inherits_grants = EXCLUDED.inherits_grants`,
		m.GroupID, m.Account, roleOrDefault(m.Role), m.InheritsGrants)
	if err != nil {
		return fmt.Errorf("groupstore: 写成员关系（%s/%s）: %w", m.GroupID, m.Account, err)
	}
	return nil
}

// RemoveMembership 移除成员关系。
//
// ★ 与 inherits_grants=false 的区别：此处移除后「他在组内」这一事实消失，
//   若非预期的借调/离岗场景，应优先用 SetMembership(inherits=false) 保留事实。
func (s *Store) RemoveMembership(ctx context.Context, groupID, account string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM dim_group_member WHERE group_id = $1 AND account = $2`, groupID, account)
	if err != nil {
		return fmt.Errorf("groupstore: 移除成员关系（%s/%s）: %w", groupID, account, err)
	}
	return nil
}

// ───────────────────────────── 小工具 ─────────────────────────────

// toAuthzLevel 把 group.Level 转成 authz.Level。
//
// 两个包各自定义了 Level 类型（有意为之：group 包不依赖 authz，保持纯逻辑可单测），
// 因此转换必须显式 —— 编译器会强制每个跨层点写明意图，
// 避免「同名类型悄悄互通」而掩盖口径分歧。
//
// ★ 非法值一律降为 L1（最严），而不是原样透传或 panic：
//   未知密级若被当作 L4 透传，就是一次静默扩权。
func toAuthzLevel(l group.Level) authz.Level {
	switch l {
	case group.L1:
		return authz.L1
	case group.L2:
		return authz.L2
	case group.L3:
		return authz.L3
	case group.L4:
		return authz.L4
	}
	return authz.L1
}

// nonNil 把 nil 切片转成空切片。
//
// ★ 这不是「防御性编程」，是修一个真实缺陷（SQLSTATE 23502）：
//   `text[] NOT NULL DEFAULT '{}'` 的 DEFAULT 只在**不提供该列**时生效；
//   显式传 NULL 会直接撞 NOT NULL。Go 的 nil []string 经 pgx 编码为 SQL NULL。
func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func roleOrDefault(r string) string {
	if r == "" {
		return "member"
	}
	return r
}

func stripPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

// groupIsIT 从 grants 的原始 jsonb 里读出 isIT 标记。
//
// ★ 为什么必须从库里读、而不是从参数传：
//   ReplaceGrants 只收「授权列表」，而 isIT 是组的**身份属性**（0006 的 D7 约束要用）。
//   若替换授权时把它丢掉，一个 IT 组会在某次改授权后变回普通组 ——
//   于是它突然可以持有业务数值，属于静默扩权。这里保留原值。
func groupIsIT(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var g groupGrantsJSON
	if err := json.Unmarshal(raw, &g); err != nil {
		return false
	}
	return g.IsIT
}

func sortedDimKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 插入排序：维度数量极少（个位数），避免为此引入 sort 依赖
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// splitDenyModule 从 "module:module.pnl" 形式解析出 module.pnl。
func splitDenyModule(deny string) (string, bool) {
	const p = "module:"
	if len(deny) > len(p) && deny[:len(p)] == p {
		return deny[len(p):], true
	}
	return "", false
}

// filterNonDeny 只保留允许项（DENY 由 0006 的约束与 Evaluate 的 deny 通道承担，
// 不写进 grants 的允许集合 —— 否则会污染「这个组能看什么」的语义）。
func filterNonDeny(gs []group.Grant) []group.Grant {
	out := make([]group.Grant, 0, len(gs))
	for _, g := range gs {
		if !g.Deny {
			out = append(out, g)
		}
	}
	return out
}

// grantDiff 一条授权差异。
type grantDiff struct {
	Grant   group.Grant
	Revoked bool
}

// diffGrants 计算 旧→新 的差异（新增 + 移除）。
//
// ★ 判等用 kind+key 的归一形式，与 group.denyKey 同一口径 ——
//   否则「key 写法不同但语义相同」的条目会被误判成「移除又新增」，
//   在流水里留下两条互相抵消的噪音记录。
func diffGrants(old, new []group.Grant) []grantDiff {
	key := func(g group.Grant) string { return g.Kind + ":" + g.Key }
	oldSet := map[string]group.Grant{}
	for _, g := range old {
		oldSet[key(g)] = g
	}
	newSet := map[string]group.Grant{}
	for _, g := range new {
		newSet[key(g)] = g
	}

	var out []grantDiff
	for k, g := range newSet {
		if _, ok := oldSet[k]; !ok {
			out = append(out, grantDiff{Grant: g})
		}
	}
	for k, g := range oldSet {
		if _, ok := newSet[k]; !ok {
			out = append(out, grantDiff{Grant: g, Revoked: true})
		}
	}
	return out
}
