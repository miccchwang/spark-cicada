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
	table       string
	grain       []string // 维度键列
	metrics     []string // 派生度量列（可为 NULL）
	covCols     []string // 覆盖率列
	skippedCol  string   // 跳过字段数组列
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
	cols = append(cols, spec.grain...)
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

	// 扫描目标：grain → *string，metrics/cov → *float64，versions → map
	grainPtrs := make([]*string, len(spec.grain))
	for i := range grainPtrs {
		grainPtrs[i] = new(string)
	}
	metricPtrs := make([]*float64, len(spec.metrics))
	for i := range metricPtrs {
		metricPtrs[i] = new(float64)
	}
	covPtrs := make([]*float64, len(spec.covCols))
	for i := range covPtrs {
		covPtrs[i] = new(float64)
	}
	var skipped []string
	algoVers := map[string]int{}
	ruleVers := map[string]int{}

	dest := make([]any, 0, len(cols))
	for i := range grainPtrs {
		dest = append(dest, grainPtrs[i])
	}
	for i := range metricPtrs {
		dest = append(dest, metricPtrs[i])
	}
	for i := range covPtrs {
		dest = append(dest, covPtrs[i])
	}
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
		// pgx 会为被扫描的指针分配；复用前先清 nil 标记
		for i := range grainPtrs {
			grainPtrs[i] = nil
		}
		for i := range metricPtrs {
			metricPtrs[i] = nil
		}
		for i := range covPtrs {
			covPtrs[i] = nil
		}
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
