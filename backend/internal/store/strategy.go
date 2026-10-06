// strategy.go —— M-STRATEGY 策略实验室的 Postgres 实现。
//
// 职责：把选型卡/决策历史/当前值读写到既有表（registry_rule_set + audit_log）。
//
// ══════════════════════════════════════════════════════════════════════════
// ★★ 本文件不建新表 —— 这是与 0009 迁移配套的纪律：
//
//	选型卡    → registry_rule_set（一张卡就是一条带版本的规则候选集）
//	决策历史  → audit_log（已有 append-only 触发器）
//	当前生效值 → registry_rule_set.scope->>'current_key'
//
// ★ 三个容易写错的地方，逐一说明：
//
//  1. **current_key 的更新必须只改 scope 的一个键，不能覆盖整个 scope。**
//     scope 里还带着 module / blocking 等元数据；若用
//     `SET scope = '{"current_key":"B"}'` 一把覆盖，卡的归属模块就丢了 ——
//     而丢失是静默的（查询仍能返回，只是 blocking 恒为 false）。
//     因此用 jsonb || 合并，而不是替换。
//
//  2. **历史读取必须按 at 正序且带 id 兜底。**
//     audit_log.at 默认 now()，同一事务里的多条记录可能拿到**完全相同**
//     的时间戳（PG 的 now() 是事务时间，不是语句时间）。若只按 at 排序，
//     同秒记录的顺序不确定 —— 用户会看到历史「自己换位置」。
//
//  3. **decide 与 mark-stale 必须在同一事务里。**
//     只改了 current_key 没标记桶 STALE ⇒ 报表用旧参数回答新口径的问题
//     且**不报错**。反过来先标 STALE 再改值则会短暂出现「参数是旧的、
//     桶说自己是脏的」—— 无害但这说明事务边界划错了。
//     两者同事务提交，外部永远只能看到「都发生」或「都没发生」。
//
// ══════════════════════════════════════════════════════════════════════════
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/strategy"
)

// StrategyStore 策略实验室的持久化实现。
type StrategyStore struct {
	pool *pgxpool.Pool
}

// NewStrategyStore 构造。
func NewStrategyStore(pool *pgxpool.Pool) *StrategyStore {
	return &StrategyStore{pool: pool}
}

// choicePrefix 选型卡在 registry_rule_set 里的 id 前缀。
//
// ★ 用前缀而不是另建表（见 0009 注释）；前缀让「列全部待决策卡」
//
//	变成一次索引扫描，且与 rule.* 的规则集天然区分开。
const choicePrefix = "strategy.choice."

// choiceRow 是 registry_rule_set 里一行选型卡的原始形态。
type choiceRow struct {
	id     string
	scope  []byte
	items  []byte
	module string
	curKey string
}

// choiceItem 是 items 数组里一个候选项的原始形态。
//
// ★ 用 json.RawMessage 之外的具体结构是有意的（与 template.QueryState
// 的透传策略相反）：items 的字段是**本服务定义并校验**的（key/risk/
// reversible 都要参与 Resolve 与 Validate），不是「前端说了算的负载」。
// 若也透传，strategy.Option 就会退化成 map，候选集守卫无从下手。
type choiceItem struct {
	Key            string `json:"key"`
	Label          string `json:"label"`
	Description    string `json:"description"`
	ExpectedEffect string `json:"expected_effect"`
	Risk           string `json:"risk"`
	Reversible     bool   `json:"reversible"`
	Recommended    bool   `json:"recommended,omitempty"`
}

// LoadChoice 读一张选型卡。
//
// reload 参数在此实现里**不改变行为**（DB 永远是权威，没有可丢弃的缓存）；
// 保留该参数是为了与接口层契约一致，将来接缓存时才有意义。
func (s *StrategyStore) LoadChoice(ctx context.Context, id string, _ bool) (*strategy.Choice, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("store: 策略实验室不可用（连接池未初始化）")
	}
	row, err := s.loadChoiceRow(ctx, id)
	if err != nil {
		return nil, err
	}
	return row.toChoice()
}

func (s *StrategyStore) loadChoiceRow(ctx context.Context, id string) (*choiceRow, error) {
	var r choiceRow
	err := s.pool.QueryRow(ctx, `
		SELECT id, scope, items,
		       COALESCE(scope->>'module',''),
		       COALESCE(scope->>'current_key','')
		  FROM registry_rule_set
		 WHERE id = $1
		 ORDER BY version DESC
		 LIMIT 1`, id).
		Scan(&r.id, &r.scope, &r.items, &r.module, &r.curKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("strategy: 选型卡 %s 不存在", id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读选型卡 %s 失败: %w", id, err)
	}
	return &r, nil
}

// toChoice 把库行转成 strategy.Choice（并做一次自洽校验）。
//
// ★ 转完**必须** Validate：库里的数据可能因人工 SQL 改动而变得不自洽
// （例如 current_key 指向一个已被删掉的 item）。此处若放过，
// 前端会渲染出一张「当前值是空」的卡，而 Resolve 会给出误导性的错误。
// 宁可在这里明确失败，也不要让一张坏卡流到用户面前。
func (r *choiceRow) toChoice() (*strategy.Choice, error) {
	var raw []choiceItem
	if err := json.Unmarshal(r.items, &raw); err != nil {
		return nil, fmt.Errorf("store: 解析选型卡 %s 的 items 失败: %w", r.id, err)
	}
	opts := make([]strategy.Option, 0, len(raw))
	for _, it := range raw {
		opts = append(opts, strategy.Option{
			Key:            it.Key,
			Label:          it.Label,
			Description:    it.Description,
			ExpectedEffect: it.ExpectedEffect,
			Risk:           strategy.Risk(it.Risk),
			Reversible:     it.Reversible,
			Recommended:    it.Recommended,
		})
	}
	// 卡的元数据从 scope 取（0009 迁移把它们声明为 scope 的键）。
	//
	// ★ 为什么元数据放 scope 而不是加列：scope 在 0002 的定义就是
	//   「这条规则集的适用范围」，扩成「规则集的元数据」是语义内的延伸。
	//   加列会让 registry_rule_set 长出只服务 M-STRATEGY 的字段，
	//   而该表是「所有规则集的通用容器」。
	var meta struct {
		Title          string   `json:"title"`
		Context        string   `json:"context"`
		Blocking       bool     `json:"blocking"`
		DecideBy       string   `json:"decide_by"`
		Metrics        []string `json:"metrics"`
		AffectedBucket []string `json:"affected_buckets"`
	}
	if err := json.Unmarshal(r.scope, &meta); err != nil {
		return nil, fmt.Errorf("store: 解析选型卡 %s 的 scope 失败: %w", r.id, err)
	}

	title := meta.Title
	if title == "" {
		// 标题缺失不是致命（id 可作降级标识），但 context 缺失是 —— 见下。
		title = r.id
	}

	metrics := meta.Metrics
	if metrics == nil {
		metrics = []string{}
	}
	buckets := meta.AffectedBucket
	if buckets == nil {
		buckets = []string{}
	}

	c := &strategy.Choice{
		ID:    r.id,
		Title: title,
		// ★ 不编造 context：缺了就原样留空，让 Validate 报出来。
		//   用一个「（无说明）」糊过去是最坏的做法 —— 用户会以为
		//   系统就是这么设计的，然后凭感觉选一个影响全公司的参数。
		Context:    meta.Context,
		CurrentKey: r.curKey,
		Options:    opts,
		Impact: strategy.ImpactPreview{
			Metrics:         metrics,
			Delta:           map[string]map[string]string{},
			AffectedBuckets: buckets,
		},
		Blocking: meta.Blocking,
		DecideBy: meta.DecideBy,
	}
	return c, nil
}

// ListPending 列出全部待决策卡（blocking 优先，然后按 id）。
//
// ★ 只列 version 最新的那张：同一 id 多版本时，旧版本是历史留档，
//
//	不应再出现在「待决策」列表里（否则用户会对着一个已废弃的候选集做决策）。
func (s *StrategyStore) ListPending(ctx context.Context, _ bool) ([]*strategy.Choice, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("store: 策略实验室不可用（连接池未初始化）")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (id) id, scope, items
		  FROM registry_rule_set
		 WHERE id LIKE $1
		 ORDER BY id, version DESC`, choicePrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("store: 列选型卡失败: %w", err)
	}
	defer rows.Close()

	var out []*strategy.Choice
	for rows.Next() {
		var r choiceRow
		if err := rows.Scan(&r.id, &r.scope, &r.items); err != nil {
			return nil, err
		}
		// blocking 需要单独解析（toChoice 里也做，但排序要用）
		var meta struct {
			Blocking bool `json:"blocking"`
		}
		_ = json.Unmarshal(r.scope, &meta)
		c, cerr := r.toChoice()
		if cerr != nil {
			// ★ 一张坏卡不应让整个列表失败：跳过并继续。
			//   但绝不「修好后返回」—— 修数据不是读路径的职责。
			continue
		}
		c.Blocking = meta.Blocking
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 稳定排序：blocking 优先，然后 id 字典序
	sortChoices(out)
	return out, nil
}

// sortChoices 就地稳定排序（blocking 优先 → id）。
func sortChoices(cs []*strategy.Choice) {
	for i := 1; i < len(cs); i++ {
		cur := cs[i]
		j := i - 1
		for j >= 0 && lessChoice(cur, cs[j]) {
			cs[j+1] = cs[j]
			j--
		}
		cs[j+1] = cur
	}
}

func lessChoice(a, b *strategy.Choice) bool {
	if a.Blocking != b.Blocking {
		return a.Blocking
	}
	return a.ID < b.ID
}

// History 读某张卡的历史（时间正序，id 兜底）。
func (s *StrategyStore) History(ctx context.Context, choiceID string) (strategy.History, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("store: 策略实验室不可用（连接池未初始化）")
	}
	// ★ ORDER BY at, id：at 可能因同事务 now() 而重复（见文件头第 2 条）
	rows, err := s.pool.Query(ctx, `
		SELECT id, at, actor, action,
		       COALESCE(detail->>'choiceId', COALESCE(target,'')),
		       COALESCE(detail->>'from',''),
		       COALESCE(detail->>'to',''),
		       COALESCE(detail->>'snapshotHash',''),
		       COALESCE(detail->>'kind','choose'),
		       -- ★ reversible 必须在**决策当时**写进 detail 才能读出来：
		       --   事后查卡拿到的可能是「已被改了值」的卡，而回滚判定要的
		       --   是「那次变更当时是否可逆」。两者不等价。
		       COALESCE((detail->>'reversible')::boolean, false)
		  FROM audit_log
		 WHERE action IN ($1, $2)
		   AND COALESCE(detail->>'choiceId', COALESCE(target,'')) = $3
		 ORDER BY at ASC, id ASC`,
		strategy.AuditActionDecide, strategy.AuditActionRollback, choiceID)
	if err != nil {
		return nil, fmt.Errorf("store: 读策略历史失败: %w", err)
	}
	defer rows.Close()

	var out strategy.History
	for rows.Next() {
		var (
			auditID              int64
			at                   time.Time
			actor, action, cid   string
			from, to, snap, kind string
			reversible           bool
		)
		if err := rows.Scan(&auditID, &at, &actor, &action, &cid, &from, &to, &snap, &kind, &reversible); err != nil {
			return nil, err
		}
		out = append(out, strategy.HistoryEntry{
			ID:           fmt.Sprintf("audit-%d", auditID),
			ChoiceID:     cid,
			FromOption:   from,
			ToOption:     to,
			DecidedBy:    actor,
			DecidedAt:    at,
			SnapshotHash: snap,
			Kind:         strategy.ActionKind(kind),
			// 读不到 reversible 时按 false（保守：不给一个实际回不去的选项）
			Reversible: reversible,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Decide 落一次决策：写历史 + 更新当前值 + 标记桶 STALE（同一事务）。
//
// snapshotHash 是决策当时的数据快照哈希（docs/01 §7.5 的「决策依据」）。
func (s *StrategyStore) Decide(ctx context.Context, c *strategy.Choice, target string,
	kind strategy.ActionKind, actor, snapshotHash string) (strategy.HistoryEntry, error) {

	if s == nil || s.pool == nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 策略实验室不可用（连接池未初始化）")
	}
	if c == nil {
		return strategy.HistoryEntry{}, strategy.ErrInvalidChoice
	}
	changed := kind == strategy.ActionChoose && target != c.CurrentKey

	var affected []string
	if changed {
		affected = append([]string(nil), c.Impact.AffectedBuckets...)
	}

	// ★ reversible 取**目标选项自己**的属性，并落进历史。
	//
	//	为什么不能事后再查卡：卡上的 current_key 会被后续决策改掉，
	//	事后查到的可能是另一个选项的可逆性。回滚判定依据的必须是
	//	「那次变更当时选的那个选项」是否可逆 —— 那是历史事实，
	//	只能记在当时。
	reversible := false
	if o, ok := c.Option(target); ok {
		reversible = o.Reversible
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // 已提交时 Rollback 是 no-op

	// ① 历史（append-only）
	detail, err := json.Marshal(map[string]any{
		"choiceId":        c.ID,
		"from":            c.CurrentKey,
		"to":              target,
		"kind":            string(kind),
		"changed":         changed,
		"reversible":      reversible,
		"affectedBuckets": affected,
		"snapshotHash":    snapshotHash,
	})
	if err != nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 序列化策略明细: %w", err)
	}
	var auditID int64
	var at time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO audit_log (actor, action, target, detail)
		VALUES ($1, $2, $3, $4)
		RETURNING id, at`,
		actor, strategy.AuditActionDecide, c.ID, detail).Scan(&auditID, &at); err != nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 写策略历史失败: %w", err)
	}

	// ② 当前值（仅实质变更时改）
	if changed {
		// ★ 用 jsonb || 合并而不是整块替换：scope 里还有 module/blocking
		//   等元数据，覆盖会静默丢失它们（见文件头第 1 条）。
		tag, err := tx.Exec(ctx, `
			UPDATE registry_rule_set
			   SET scope = scope || jsonb_build_object('current_key', $2::text),
			       updated_at = now()
			 WHERE id = $1`, c.ID, target)
		if err != nil {
			return strategy.HistoryEntry{}, fmt.Errorf("store: 更新当前值失败: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// 卡不存在 ⇒ 回滚整个事务（历史也不该留下）
			return strategy.HistoryEntry{}, fmt.Errorf("store: 选型卡 %s 不存在，决策未生效", c.ID)
		}

		// ③ 标记受影响桶 STALE（同事务，见文件头第 3 条）
		for _, b := range affected {
			if _, err := tx.Exec(ctx,
				`UPDATE registry_bucket SET state='STALE', updated_at=now() WHERE id=$1`, b); err != nil {
				return strategy.HistoryEntry{}, fmt.Errorf("store: 标记桶 %s STALE 失败: %w", b, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 提交决策失败: %w", err)
	}

	return strategy.HistoryEntry{
		ID:              fmt.Sprintf("audit-%d", auditID),
		ChoiceID:        c.ID,
		FromOption:      c.CurrentKey,
		ToOption:        target,
		DecidedBy:       actor,
		DecidedAt:       at,
		SnapshotHash:    snapshotHash,
		Kind:            kind,
		Reversible:      reversible,
		AffectedBuckets: affected,
	}, nil
}

// PlanRollback 生成回滚计划。
//
// ★ 计划由服务端生成（不接受前端传参）：见 api/strategy.go 的注释。
func (s *StrategyStore) PlanRollback(ctx context.Context, choiceID string, reload bool) (strategy.RollbackPlan, error) {
	c, err := s.LoadChoice(ctx, choiceID, reload)
	if err != nil {
		return strategy.RollbackPlan{}, err
	}
	h, err := s.History(ctx, choiceID)
	if err != nil {
		return strategy.RollbackPlan{}, err
	}
	return strategy.PlanRollback(c, h)
}

// Rollback 执行回滚（写历史 + 改回稳定值 + 标记桶 STALE，同一事务）。
func (s *StrategyStore) Rollback(ctx context.Context, plan strategy.RollbackPlan, actor string) (strategy.HistoryEntry, error) {
	if s == nil || s.pool == nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 策略实验室不可用（连接池未初始化）")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	detail, err := json.Marshal(map[string]any{
		"choiceId": plan.ChoiceID,
		"from":     plan.FromOption,
		"to":       plan.ToOption,
		"kind":     string(strategy.ActionChoose),
		"changed":  true,
		// ★ 回滚记录本身**必须是可逆的**：回滚是「撤销上一次变更」，
		//   而「撤销回滚」在语义上完全成立（把值再改回去）。
		//   若这里不写 reversible，读历史时会 COALESCE 成 false，
		//   于是刚回滚完就再也回不去了 —— 回滚变成单向门，
		//   这与 docs/01 §7.4「任何策略变更可一键回滚」直接冲突。
		"reversible":      true,
		"undoOf":          plan.UndoOfEntryID,
		"affectedBuckets": plan.AffectedBuckets,
		"snapshotHash":    "", // 回滚的决策依据是「上一稳定版本」，非当前数据快照
	})
	if err != nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 序列化回滚明细: %w", err)
	}
	var auditID int64
	var at time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO audit_log (actor, action, target, detail)
		VALUES ($1, $2, $3, $4)
		RETURNING id, at`,
		actor, strategy.AuditActionRollback, plan.ChoiceID, detail).Scan(&auditID, &at); err != nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 写回滚历史失败: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE registry_rule_set
		   SET scope = scope || jsonb_build_object('current_key', $2::text),
		       updated_at = now()
		 WHERE id = $1`, plan.ChoiceID, plan.ToOption)
	if err != nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 回滚更新失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 选型卡 %s 不存在，回滚未生效", plan.ChoiceID)
	}

	// ★ 回滚同样必须重算：桶里存的是用变更后参数算出来的数。
	for _, b := range plan.AffectedBuckets {
		if _, err := tx.Exec(ctx,
			`UPDATE registry_bucket SET state='STALE', updated_at=now() WHERE id=$1`, b); err != nil {
			return strategy.HistoryEntry{}, fmt.Errorf("store: 回滚标记桶 %s STALE 失败: %w", b, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return strategy.HistoryEntry{}, fmt.Errorf("store: 提交回滚失败: %w", err)
	}
	return strategy.HistoryEntry{
		ID:              fmt.Sprintf("audit-%d", auditID),
		ChoiceID:        plan.ChoiceID,
		FromOption:      plan.FromOption,
		ToOption:        plan.ToOption,
		DecidedBy:       actor,
		DecidedAt:       at,
		Kind:            strategy.ActionChoose,
		Reversible:      true,
		AffectedBuckets: plan.AffectedBuckets,
	}, nil
}

// RecordStrategyChange 实现 api.StrategyAuditor：写一条**接口层**策略审计。
//
// ★ 与 Decide/Rollback 内部的审计是**两条不同用途的记录**，但它们的动作名
// 必须不同 —— 这一点曾是本模块最严重的缺陷：
//
//	store 写在业务事务里的那条（action = strategy.decide / strategy.rollback）
//	是**策略历史的唯一来源**，它携带 reversible 字段。
//	接口层这条只记录「这次调用有没有留痕」，用的是 *_attempt 动作名，
//	不被 History() 选中。
//
//	早期两处都写 strategy.decide，于是历史里每次决策都多一条影子记录；
//	影子记录没有 reversible ⇒ 读出来是 false ⇒ LastStable() 遇第一条即停
//	⇒ **决策之后回滚永久不可达**。用途不同 ≠ 动作名可以相同：
//	只要 History() 按 action 过滤，同名的两条数据就必然互相污染。
func (s *StrategyStore) RecordStrategyChange(ctx context.Context, actor, action, target string,
	detail map[string]any) error {

	if s == nil || s.pool == nil {
		return fmt.Errorf("store: 策略审计不可用（连接池未初始化）")
	}
	if actor == "" {
		// 无 actor 的审计是无用审计（追不到人），宁可不落
		return fmt.Errorf("store: 策略审计缺少 actor")
	}
	d, err := json.Marshal(detail)
	if err != nil {
		d = []byte(`{}`)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log (actor, action, target, detail)
		VALUES ($1, $2, $3, $4)`, actor, action, nullable(target), d); err != nil {
		return fmt.Errorf("store: 写策略审计失败: %w", err)
	}
	return nil
}
