// Package store —— M-QUERY 的 Postgres 实现（query.Store）。
//
// 纪律（docs/01 §5、docs/03、闸门 G3/G5/G6）：
//   * 桶只读：**绝不**在查询路径写任何派生值（预计算层负责写）。
//   * NULL 语义：桶里 NULL = 依赖缺失被跳过 ⇒ 返回 nil（渲染「待接入」）。
//     **严禁**把 NULL 转成 0 —— 这是本项目第一红线（G3）。
//   * 桶状态非 FRESH ⇒ 由调用方 fail-closed 拒绝（本层如实上报状态）。
//   * 所有 SQL 使用**参数占位**，字段名走白名单（防注入）。
package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/query"
)

// Postgres 是 query.Store 的生产实现。
type Postgres struct {
	pool *pgxpool.Pool
}

// New 用已有连接池构造。
func New(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// bucketSpec 描述一个桶的物理形态（grain 列 + 度量列）。
//
// 纪律：**度量列清单在此显式声明**，不反射读表结构 —— 避免「新加列悄悄泄漏」。
type bucketSpec struct {
	table   string
	grain   []string // 维度键列
	metrics []string // 派生度量列（可为 NULL）
	covCols []string // 覆盖率列
	// grainExpr 覆盖个别 grain 列的**选择表达式**（如 date 列需 ::text）。
	// 未列出的列按原列名选择。
	//
	// 存在的理由：grain 一律按 *string 扫描（对外契约是 map[string]string），
	// 但 DDL 里的维度列可能是 date/timestamptz/numeric 等非文本类型。
	// pgx 的强类型扫描会拒绝从 date 扫进 *string。
	grainExpr   map[string]string
	skippedCol  string // 跳过字段数组列
	hasAlgoVers bool
	hasRuleVers bool
}

// specs 桶注册（新增桶必须在此登记，否则查询直接失败 —— fail-closed）。
var specs = map[string]bucketSpec{
	"pnl_month": {
		table:   "bucket_pnl_month",
		grain:   []string{"month", "channel_code", "shop_id", "brand"},
		metrics: []string{"rev", "cogs", "gp", "gmp", "net_contrib"},
		covCols: []string{"cov_cogs", "cov_affiliate"},
		// ★ month 在 DDL 里是 date，而 grain 一律按 *string 扫描
		//   （对外的 Key 契约是 map[string]string，前端直接展示）。
		//   必须显式 ::text 转换，否则 pgx 报：
		//     cannot scan date (OID 1082) in binary format into *string
		//   真实事故：这条断言在本机一直 skip（无库），CI 起了真 Postgres
		//   才第一次跑到 —— 说明「跳过」会长期掩盖 SQL 与 Go 的类型不匹配。
		grainExpr: map[string]string{"month": "month::text"},
		// skipped_fields / algo_versions / rule_versions 显式读取
		skippedCol:  "skipped_fields",
		hasAlgoVers: true,
		hasRuleVers: true,
	},
}

// SelectBucket 读取桶行（**只读**）。
//
// 关键实现点：
//   - 度量列用 *float64 接收：pgx 把 SQL NULL 扫成 nil ⇒ 我们原样返回 nil。
//     （若改用 float64 会把 NULL 变成 0，直接违反 G3。）
//   - 过滤条件由 query.BuildWhere 生成，参数化绑定。
func (p *Postgres) SelectBucket(ctx context.Context, bucket string, q contracts.QueryState) ([]query.BucketRow, error) {
	spec, ok := specs[bucket]
	if !ok {
		return nil, fmt.Errorf("store: 未注册的桶 %q（拒绝隐式查询）", bucket)
	}

	cols := make([]string, 0, len(spec.grain)+len(spec.metrics)+len(spec.covCols)+4)
	// grain 列：按 grainExpr 覆盖（date → ::text 等），否则用原列名。
	// ★ 选择表达式必须与下方「扫描目标按 spec.grain 顺序建 *string」严格对齐 ——
	//   顺序错位会把 date 扫进错误的列，且**不会报错**（都是文本）⇒ 静默错值。
	for _, g := range spec.grain {
		if expr, ok := spec.grainExpr[g]; ok {
			cols = append(cols, expr+" AS "+g)
			continue
		}
		cols = append(cols, g)
	}
	cols = append(cols, spec.metrics...)
	cols = append(cols, spec.covCols...)
	if spec.skippedCol != "" {
		cols = append(cols, spec.skippedCol)
	}
	verCols := []string{}
	if spec.hasAlgoVers {
		verCols = append(verCols, "algo_versions")
	}
	if spec.hasRuleVers {
		verCols = append(verCols, "rule_versions")
	}
	cols = append(cols, verCols...)

	where, args := query.BuildWhere(q)
	sql := fmt.Sprintf("SELECT %s FROM %s WHERE %s",
		strings.Join(cols, ", "), spec.table, where)

	// 排序下推（白名单列）
	if q.Order != nil {
		if dir, ok := orderCol(q.Order.Field, spec); ok {
			s := "ASC"
			if strings.EqualFold(q.Order.Dir, "desc") {
				s = "DESC"
			}
			sql += fmt.Sprintf(" ORDER BY %s %s NULLS LAST", dir, s)
		}
	}
	// 分页下推
	if q.Page != nil {
		if q.Page.Limit > 0 {
			sql += fmt.Sprintf(" LIMIT %d", q.Page.Limit)
		}
		if q.Page.Offset > 0 {
			sql += fmt.Sprintf(" OFFSET %d", q.Page.Offset)
		}
	}

	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询桶 %s 失败: %w", bucket, err)
	}
	defer rows.Close()

	// 扫描目标：grain → **string（可空维度），metrics/cov → **float64，versions → map
	//
	// ★ 必须用「指针的指针」接收，不能复用 *float64 再手工置 nil。
	//
	//   真实事故（CI 真库首次抓出的第 5 个 bug）：
	//   原实现对每个可空列分配 new(float64)，把该 *float64 放进 dest，
	//   然后在每行开始前把 metricPtrs[i] = nil 「重置」——
	//   但 dest 里存的仍是**同一个 *float64 值**（拷贝），
	//   于是 pgx 拿到的是有效指针、扫到 NULL 时却报
	//       cannot scan NULL into *float64
	//   （而当次重置若生效，dest 里就是 nil，pgx 更会直接拒绝）。
	//
	//   正确做法：dest 里放 `**float64`。pgx 扫到 NULL 时把内层指针设为 nil，
	//   扫到值时为内层分配 —— 这样「NULL ⇒ nil」是驱动层原生语义，
	//   **不依赖我们手工重置**，也就不可能因为忘记重置而把上一行的值留下来。
	//
	//   G3（NULL 绝不当 0）在这里是**由类型系统保证**的：
	//   *float64 能表达 nil，float64 不能；换成 float64 会静默把 NULL 变 0。
	grainPtrs := make([]*string, len(spec.grain))
	grainDest := make([]any, len(spec.grain))
	for i := range grainPtrs {
		grainDest[i] = &grainPtrs[i]
	}
	metricPtrs := make([]*float64, len(spec.metrics))
	metricDest := make([]any, len(spec.metrics))
	for i := range metricPtrs {
		metricDest[i] = &metricPtrs[i]
	}
	covPtrs := make([]*float64, len(spec.covCols))
	covDest := make([]any, len(spec.covCols))
	for i := range covPtrs {
		covDest[i] = &covPtrs[i]
	}

	var skipped []string
	algoVers := map[string]int{}
	ruleVers := map[string]int{}

	dest := make([]any, 0, len(cols))
	dest = append(dest, grainDest...)
	dest = append(dest, metricDest...)
	dest = append(dest, covDest...)
	if spec.skippedCol != "" {
		dest = append(dest, &skipped)
	}
	if spec.hasAlgoVers {
		dest = append(dest, &algoVers)
	}
	if spec.hasRuleVers {
		dest = append(dest, &ruleVers)
	}

	var out []query.BucketRow
	for rows.Next() {
		// ★ 每个可空列由 pgx 经「指针的指针」逐行写入：
		//   SQL NULL ⇒ 内层指针被置为 nil；有值 ⇒ 内层指针被分配。
		//   因此**不需要**（也**不应该**）手工重置 —— 手工重置曾是第 5 个
		//   真库 bug 的来源（dest 里放的是指针拷贝，重置无效）。
		//   这里保留 skipped/versions 的重置，因为它们用值类型接收。
		skipped = nil
		algoVers = map[string]int{}
		ruleVers = map[string]int{}

		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: 扫描桶行失败: %w", err)
		}

		br := query.BucketRow{
			Keys:          map[string]string{},
			Metrics:       map[string]*float64{},
			AlgoVersions:  algoVers,
			RuleVersions:  ruleVers,
			SkippedFields: skipped,
		}
		for i, col := range spec.grain {
			// grain 列在 DDL 中 NOT NULL；此处 nil 视为空串（不会发生）
			if grainPtrs[i] != nil {
				br.Keys[col] = *grainPtrs[i]
			} else {
				br.Keys[col] = ""
			}
		}
		// ★ 关键：*float64 == nil ⇒ 保持 nil（NULL 不补 0）
		for i, col := range spec.metrics {
			br.Metrics[col] = metricPtrs[i]
		}
		for i, col := range spec.covCols {
			switch col {
			case "cov_cogs":
				br.CovCogs = covPtrs[i]
			case "cov_affiliate":
				br.CovAffiliate = covPtrs[i]
			}
		}
		out = append(out, br)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历桶行失败: %w", err)
	}
	return out, nil
}

// BucketState 读桶状态与版本（供 G6 漂移检测与 fail-closed 判定）。
func (p *Postgres) BucketState(ctx context.Context, bucket string) (query.BucketMeta, error) {
	var (
		id        string
		state     string
		algoVers  map[string]int
		ruleVers  map[string]int
	)
	err := p.pool.QueryRow(ctx,
		`SELECT id, state, algo_versions, rule_versions FROM registry_bucket WHERE id = $1`,
		bucket).Scan(&id, &state, &algoVers, &ruleVers)
	if err != nil {
		if err == pgx.ErrNoRows {
			// 未注册的桶 = 不可用（fail-closed），不是「空桶」
			return query.BucketMeta{ID: bucket, State: "UNREGISTERED"}, nil
		}
		return query.BucketMeta{}, fmt.Errorf("store: 读桶状态 %s 失败: %w", bucket, err)
	}
	if algoVers == nil {
		algoVers = map[string]int{}
	}
	if ruleVers == nil {
		ruleVers = map[string]int{}
	}
	return query.BucketMeta{ID: id, State: state, AlgoVersions: algoVers, RuleVersions: ruleVers}, nil
}

// SlotHealth 读依赖槽的运行态（状态 + 覆盖率 + 门限）。
//
// 覆盖率取**最新一次实测**（registry_slot_coverage）；无实测记录 ⇒ coverage=0
// （即「未知 ⇒ 视为不达标」，fail-closed，绝不当成 1.0）。
func (p *Postgres) SlotHealth(ctx context.Context, slotIDs []string) (map[string]query.SlotStatus, error) {
	out := map[string]query.SlotStatus{}
	if len(slotIDs) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT s.id, s.status, s.coverage_gate,
		       COALESCE(c.coverage, 0) AS coverage
		  FROM registry_slot s
		  LEFT JOIN LATERAL (
		       SELECT coverage FROM registry_slot_coverage
		        WHERE slot_id = s.id ORDER BY measured_at DESC LIMIT 1
		  ) c ON true
		 WHERE s.id = ANY($1)`, slotIDs)
	if err != nil {
		return nil, fmt.Errorf("store: 读槽健康度失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, status string
			gate, cov  float64
		)
		if err := rows.Scan(&id, &status, &gate, &cov); err != nil {
			return nil, fmt.Errorf("store: 扫描槽行失败: %w", err)
		}
		out[id] = query.SlotStatus{Status: status, Coverage: cov, Gate: gate}
	}
	return out, rows.Err()
}

// orderCol 把 QueryState.Order.Field 映射到物理列（白名单）。
func orderCol(field string, spec bucketSpec) (string, bool) {
	if field == "" {
		return "", false
	}
	for _, c := range spec.grain {
		if c == field {
			return c, true
		}
	}
	for _, c := range spec.metrics {
		if c == field {
			return c, true
		}
	}
	// 驼峰别名映射（前端 key → 列）
	switch field {
	case "channelCode":
		return "channel_code", true
	case "storeKey":
		return "shop_id", true
	case "month":
		return "month", true
	}
	return "", false
}
