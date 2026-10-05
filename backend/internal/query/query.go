// Package query —— M-QUERY 查询编排模块。
//
// 职责（docs/02 M-QUERY）：
//   QueryState → 查询计划 → DB → ResultSet
//
// **边界纪律**：
//   * 本包**不含任何业务公式**（GP/GMP/净贡献等一律由 compute 内核产出）。
//   * 本包只做：规范化、缓存、择桶、生成 SQL、分页/排序下推。
//   * 所有派生列的值，通过 compute 内核（Rust）计算后填入。
package query

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/compute"
	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
)

// ErrNoBucket 表示未找到可用预计算桶（须回退到实时计算或拒绝）。
var ErrNoBucket = errors.New("no usable precompute bucket")

// ResultSet 是 M-QUERY 的产出（比 DataContract 更靠近数据源，
// 由 api 层组装为 DataContract 交给 M-RENDER）。
type ResultSet struct {
	QueryHash   string
	Rows        []contracts.Row
	Columns     []contracts.ColumnDef
	Aggregates  map[string]*float64
	AlgoTrace   []contracts.AlgoTrace
	Gaps        []contracts.DataGap
	Precomputed bool
	GeneratedAt time.Time
}

// Store 是 DB 访问的最小抽象（便于测试注入）。
type Store interface {
	// SelectBucket 读取预计算桶行。
	SelectBucket(ctx context.Context, bucket string, q contracts.QueryState) ([]BucketRow, error)
	// BucketState 查询桶状态（FRESH/STALE/...）与版本。
	BucketState(ctx context.Context, bucket string) (BucketMeta, error)
	// SlotHealth 查询依赖槽的健康度（状态 + 覆盖率 + 门限）。
	SlotHealth(ctx context.Context, slotIDs []string) (map[string]SlotStatus, error)
}

// BucketRow 是桶里一行（含派生列与覆盖率元数据）。
type BucketRow struct {
	Keys          map[string]string
	Metrics       map[string]*float64
	SkippedFields []string
	CovCogs       *float64
	CovAffiliate  *float64
	AlgoVersions  map[string]int
	RuleVersions  map[string]int
}

type BucketMeta struct {
	ID           string
	State        string // FRESH | STALE | BUILDING | FAILED
	AlgoVersions map[string]int
	RuleVersions map[string]int
}

// SlotStatus 是槽运行态（与 Rust gate::SlotHealth 对齐）。
type SlotStatus struct {
	Status   string // ACTIVE | MISSING | DEGRADED | DISABLED
	Coverage float64
	Gate     float64
}

// Cache 是结果缓存抽象（Redis 实现）。
type Cache interface {
	Get(ctx context.Context, key string) (*ResultSet, bool)
	Set(ctx context.Context, key string, rs *ResultSet, ttl time.Duration) error
}

// Orchestrator 编排 QueryState → ResultSet。
type Orchestrator struct {
	Store Store
	Cache Cache
	// Kernel 是计算内核客户端（Rust）。Go 侧**只调用**，不实现公式。
	Kernel compute.Kernel
	// Policy 择桶与降级策略。
	Policy Policy
}

// Policy 控制择桶与降级行为。
type Policy struct {
	// 允许在桶 STALE 时使用（默认 false → fail-closed 拒绝）
	AllowStaleBucket bool
	// 缓存 TTL
	CacheTTL time.Duration
	// 桶字段 → 算法 ID 的映射（用于 AlgoTrace）
	BucketAlgoMap map[string]map[string]string
}

// Run 执行一次查询。
//
// 幂等保证：相同 QueryState ⇒ 相同 queryHash ⇒ 相同 ResultSet（闸门 G1）。
func (o *Orchestrator) Run(ctx context.Context, q contracts.QueryState) (*ResultSet, error) {
	if q.V == "" {
		q.V = contracts.QueryStateVersion
	}
	hash, err := contracts.Hash(q)
	if err != nil {
		return nil, fmt.Errorf("query: hash: %w", err)
	}

	// 1) 缓存
	if o.Cache != nil {
		if rs, ok := o.Cache.Get(ctx, hash); ok && rs != nil {
			return rs, nil
		}
	}

	// 2) 择桶
	bucket, err := o.Policy.selectBucket(q)
	if err != nil {
		return nil, err
	}

	// 3) 桶状态校验（STALE 默认拒绝 = fail-closed）
	if o.Store != nil {
		meta, err := o.Store.BucketState(ctx, bucket)
		if err != nil {
			return nil, fmt.Errorf("query: bucket state: %w", err)
		}
		if meta.State != "FRESH" {
			if !o.Policy.AllowStaleBucket {
				return nil, fmt.Errorf("%w: bucket %s state=%s", ErrNoBucket, bucket, meta.State)
			}
		}
	}

	// 4) 读桶
	if o.Store == nil {
		return nil, errors.New("query: store not configured")
	}
	rows, err := o.Store.SelectBucket(ctx, bucket, q)
	if err != nil {
		return nil, fmt.Errorf("query: select bucket: %w", err)
	}

	// 5) 组装行 + AlgoTrace（派生列的值直接取桶，未重算；缺失即 null）
	rs := &ResultSet{
		QueryHash:   hash,
		Rows:        make([]contracts.Row, 0, len(rows)),
		Aggregates:  map[string]*float64{},
		Precomputed: true,
		GeneratedAt: time.Now().UTC(),
	}
	algoMap := o.Policy.BucketAlgoMap[bucket]

	for _, br := range rows {
		row := contracts.Row{}
		for k, v := range br.Keys {
			row[k] = contracts.NewStr(v)
		}
		for field, val := range br.Metrics {
			if val == nil {
				// 缺失 ⇒ null（渲染「待接入」），绝不用 0 占位
				row[field] = contracts.Null()
				rs.Gaps = appendGap(rs.Gaps, field, br, algoMap)
				continue
			}
			row[field] = contracts.NewNum(*val)
		}
		rs.Rows = append(rs.Rows, row)
	}

	// 6) AlgoTrace + Columns：为每个派生列记录来源算法与所用槽。
	//    列定义决定渲染层能看到哪些字段——**必须由查询层显式声明**，
	//    未声明的列下游（Gate）一律视为不可见（fail-closed）。
	algoKeys := make([]string, 0, len(algoMap))
	for field := range algoMap {
		algoKeys = append(algoKeys, field)
	}
	sort.Strings(algoKeys)
	for _, field := range algoKeys {
		algoID := algoMap[field]
		rs.AlgoTrace = append(rs.AlgoTrace, contracts.AlgoTrace{
			Field:  field,
			AlgoID: algoID,
		})
		rs.Columns = append(rs.Columns, contracts.ColumnDef{
			Key:     field,
			Label:   field,
			Perm:    defaultPermFor(field),
			Kind:    defaultKindFor(field),
			AlgoID:  algoID,
		})
	}

	// 7) 缓存
	if o.Cache != nil {
		_ = o.Cache.Set(ctx, hash, rs, o.Policy.CacheTTL)
	}
	return rs, nil
}

// defaultPermFor 返回派生列的默认密级（可被 api.FieldPolicy 覆盖）。
func defaultPermFor(field string) string {
	switch field {
	case "net_contrib":
		return "L4"
	default:
		return "L3" // 成本/利润类派生量默认 L3
	}
}

// defaultKindFor 返回派生列的默认展示类型。
func defaultKindFor(field string) string {
	switch field {
	case "gp", "cogs", "net_contrib":
		return "currency"
	default:
		return "number"
	}
}

func appendGap(gaps []contracts.DataGap, field string, br BucketRow, algoMap map[string]string) []contracts.DataGap {
	slot := ""
	gate := 0.80
	cov := 0.0
	switch field {
	case "cogs":
		if br.CovCogs != nil {
			cov = *br.CovCogs
		}
		slot = "slot.cogs"
	case "net_contrib":
		if br.CovAffiliate != nil {
			cov = *br.CovAffiliate
		}
		slot = "slot.affiliate"
	}
	return append(gaps, contracts.DataGap{
		Field:    field,
		Slot:     slot,
		Coverage: cov,
		Gate:     gate,
		Reason:   "依赖数据不足，已跳过（fail-closed）",
	})
}

// selectBucket 依据 precomputeHint 或规则匹配选择桶。
// 目前仅 pnl_month；后续按 grain + 所需字段匹配。
func (p Policy) selectBucket(q contracts.QueryState) (string, error) {
	if q.PrecomputeHint != nil && q.PrecomputeHint.Bucket != "" {
		return q.PrecomputeHint.Bucket, nil
	}
	// 粒度 → 桶
	switch q.Time.Grain {
	case "month":
		return "pnl_month", nil
	default:
		return "", fmt.Errorf("%w: unsupported grain %q", ErrNoBucket, q.Time.Grain)
	}
}

// BuildWhere 生成过滤下推的 SQL 片段（仅结构，不含业务公式）。
// 返回 (whereSQL, args)。
func BuildWhere(q contracts.QueryState) (string, []any) {
	var conds []string
	var args []any
	argN := 0
	add := func(expr string, v any) {
		argN++
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(expr, argN))
	}

	// 时间（月粒度）
	if q.Time.From != "" {
		add("month >= $%d", q.Time.From)
	}
	if q.Time.To != "" {
		add("month <= $%d", q.Time.To)
	}
	// 维度
	for _, v := range q.Dims.ChannelCode {
		add("channel_code = $%d", v)
	}
	for _, v := range q.Dims.StoreKey {
		add("shop_id = $%d", v)
	}
	for _, v := range q.Dims.Brand {
		add("brand = $%d", v)
	}
	// 通用 filters（字段白名单，防注入）
	for _, f := range q.Filters {
		col, ok := allowedFilterColumns[f.Field]
		if !ok {
			continue // 未知字段忽略（不拼接，杜绝注入）
		}
		switch f.Op {
		case "eq":
			add(col+" = $%d", f.Value)
		case "in":
			add(col+" = ANY($%d)", f.Value)
		case "gt":
			add(col+" > $%d", f.Value)
		case "gte":
			add(col+" >= $%d", f.Value)
		case "lt":
			add(col+" < $%d", f.Value)
		case "lte":
			add(col+" <= $%d", f.Value)
		}
	}

	if len(conds) == 0 {
		return "TRUE", nil
	}
	return strings.Join(conds, " AND "), args
}

// allowedFilterColumns 是过滤字段白名单（key → 列名）。
// **安全红线**：只允许白名单列参与 SQL 拼接，其余一律忽略。
var allowedFilterColumns = map[string]string{
	"channelCode": "channel_code",
	"storeKey":    "shop_id",
	"brand":       "brand",
	"month":       "month",
}
