// Package contracts —— Go 侧的契约镜像（与 contracts/*.ts 对齐）。
//
// 纪律：Go 只做编排与 I/O；本包**不含任何业务公式**。
// 任何数值计算都必须经 compute（Rust）完成（docs/01 §4.1）。
//
// 版本一致性：若 contracts/*.ts 升版本，本文件必须同步，否则 CI 闸门 G1 失败。
package contracts

// QueryStateVersion 与 contracts/query-state.ts 的 QUERY_STATE_VERSION 对齐。
const QueryStateVersion = "1.0"

// DataContractVersion 与 contracts/data-contract.ts 对齐。
const DataContractVersion = "1.0"

// ───────────────────────────── QueryState（M-FILTER 唯一产出） ─────────────────────────────

// QueryState 是筛选模块的唯一产出，必须是纯可序列化数据。
type QueryState struct {
	V              string          `json:"v"`
	Time           TimeRange       `json:"time"`
	Filters        []FilterClause  `json:"filters"`
	Dims           DimSelection    `json:"dims"`
	Order          *OrderClause    `json:"order,omitempty"`
	Page           *PageClause     `json:"page,omitempty"`
	PrecomputeHint *PrecomputeHint `json:"precomputeHint,omitempty"`
	ProfileID      string          `json:"profileId,omitempty"`
}

type TimeRange struct {
	Mode     string `json:"mode"` // preset | custom | slider
	Preset   string `json:"preset,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Grain    string `json:"grain"` // day|week|month|quarter|half|year
	Timezone string `json:"timezone"`
}

type FilterClause struct {
	Field string `json:"field"`
	Op    string `json:"op"` // eq|in|gt|gte|lt|lte|between|contains
	Value any    `json:"value"`
}

type DimSelection struct {
	Level         string   `json:"level"` // overview|domain|channel|store|sku
	Brand         []string `json:"brand,omitempty"`
	Domain        []string `json:"domain,omitempty"`
	ChannelCode   []string `json:"channelCode,omitempty"`
	StoreKey      []string `json:"storeKey,omitempty"`
	Category      []string `json:"category,omitempty"`
	ProductStatus []string `json:"productStatus,omitempty"`
}

type OrderClause struct {
	Field string `json:"field"`
	Dir   string `json:"dir"` // asc|desc
}

type PageClause struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

type PrecomputeHint struct {
	Bucket      string `json:"bucket"`
	AlgoVersion *int   `json:"algoVersion,omitempty"`
	RuleVersion *int   `json:"ruleVersion,omitempty"`
}

// ───────────────────────────── DataContract（M-RENDER 唯一入参） ─────────────────────────────

// Row 单行数据：字符串键 → 字符串/数字/null。
// 注意：null 表示「无数据」，渲染为「待接入」；**不得用 0 占位**。
type Row map[string]*Value

// Value 是可空数值/文本的窄类型，区分「有值」与「null」。
type Value struct {
	Str   *string
	Num   *float64
	IsNum bool
}

// NewStr 构造字符串值。
func NewStr(s string) *Value { return &Value{Str: &s} }

// NewNum 构造数值。
func NewNum(f float64) *Value { return &Value{Num: &f, IsNum: true} }

// Null 构造空值（渲染为「待接入」）。
func Null() *Value { return &Value{} }

type DataContract struct {
	V            string                   `json:"v"`
	QueryHash    string                   `json:"queryHash"`
	Rows         []Row                    `json:"rows"`
	Columns      []ColumnDef              `json:"columns"`
	Aggregates   map[string]*float64      `json:"aggregates"` // null 允许
	Levels       []LevelSummary           `json:"levels"`
	AlgoTrace    []AlgoTrace              `json:"algoTrace"`
	Gaps         []DataGap                `json:"gaps"`
	Precomputed  bool                     `json:"precomputed"`
	GeneratedAt  string                   `json:"generatedAt"`
}

type ColumnDef struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Perm        string `json:"perm"` // L1..L4
	Kind        string `json:"kind"` // text|number|percent|currency|date
	Pin         string `json:"pin,omitempty"`
	Collapsible bool   `json:"collapsible,omitempty"`
	AlgoID      string `json:"algoId,omitempty"`
}

type LevelSummary struct {
	Level           string              `json:"level"` // L0..L4
	Key             string              `json:"key"`
	Label           string              `json:"label"`
	Metrics         map[string]*float64 `json:"metrics"`
	ChildCount      int                 `json:"childCount"`
	DefaultExpanded bool                `json:"defaultExpanded"`
}

type AlgoTrace struct {
	Field     string   `json:"field"`
	AlgoID    string   `json:"algoId"`
	DataSlots []string `json:"dataSlots"`
	Skipped   bool     `json:"skipped"`
	Reason    string   `json:"reason,omitempty"`
}

type DataGap struct {
	Field    string  `json:"field"`
	Slot     string  `json:"slot"`
	Coverage float64 `json:"coverage"`
	Gate     float64 `json:"gate"`
	Reason   string  `json:"reason"`
}

// DefaultExpanded 默认收起契约（硬性）——仅 L0 常驻展开。
// 见 contracts/data-contract.ts 的 DEFAULT_EXPANDED。
var DefaultExpanded = map[string]bool{
	"L0": true, "L1": false, "L2": false, "L3": false, "L4": false,
}
