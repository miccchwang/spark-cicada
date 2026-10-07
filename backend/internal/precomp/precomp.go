// Package precomp —— M-PRECOMP 预计算模块。
//
// 职责（docs/02 M-PRECOMP）：按桶定义把算法结果物化到 DB；增量刷新；版本管理。
//
// **关键纪律**：
//   * 桶内派生量一律经 compute 内核（Rust）计算，Go 不实现公式。
//   * 依赖槽不可用（状态/覆盖率不达标）⇒ 该字段跳过（写 NULL + 记 skipped_fields），
//     绝不写 0 冒充。
//   * 桶记录 algo_versions / rule_versions，用于版本漂移检测与重算触发。
package precomp

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/compute"
)

// BucketDef 桶定义（对应 buckets/*.yaml）。
type BucketDef struct {
	ID           string
	Grain        []string
	ProducedBy   []string // 算法 ID 序列（按依赖顺序）
	Refresh      string
	AlgoVersions map[string]int
	RuleVersions map[string]int
	Indexes      [][]string
}

// AlgoDef 算法定义（对应 algorithms/*.yaml）。
type AlgoDef struct {
	ID             string
	Version        int
	Formula        string
	Unit           string
	Permission     string
	DependsOnSlots []string
	MissingPolicy  string // skip | null | error
}

// SlotState 依赖槽运行态。
type SlotState struct {
	ID       string
	Status   string // ACTIVE | MISSING | DEGRADED | DISABLED
	Coverage float64
	Gate     float64
}

// Builder 执行桶构建。
type Builder struct {
	Kernel compute.Kernel
	// Slots 提供槽运行态。
	Slots func(ctx context.Context, ids []string) ([]SlotState, error)
	// Algos 提供算法定义。
	Algos func(ctx context.Context, ids []string) ([]AlgoDef, error)
}

// CellResult 单个派生单元格的结果。
type CellResult struct {
	Field   string
	Value   compute.Scalar // nil = 缺失（写 NULL）
	Skipped bool
	Reason  string
	Slots   []string
}

// RowResult 一行的构建结果。
type RowResult struct {
	Keys          map[string]string
	Cells         []CellResult
	SkippedFields []string
}

// BuildRow 计算一行的所有派生字段。
//
// inputs 提供该行可用的原子量（来自事实表 / 对齐后的主数据）。
func (b *Builder) BuildRow(ctx context.Context, def BucketDef, inputs map[string]compute.Scalar) (*RowResult, error) {
	algos, err := b.Algos(ctx, def.ProducedBy)
	if err != nil {
		return nil, fmt.Errorf("precomp: load algos: %w", err)
	}
	byID := map[string]AlgoDef{}
	for _, a := range algos {
		byID[a.ID] = a
	}

	// 收集所有依赖槽并一次性取状态
	slotSet := map[string]bool{}
	for _, a := range algos {
		for _, s := range a.DependsOnSlots {
			slotSet[s] = true
		}
	}
	var slotIDs []string
	for s := range slotSet {
		slotIDs = append(slotIDs, s)
	}
	sort.Strings(slotIDs)
	states, err := b.Slots(ctx, slotIDs)
	if err != nil {
		return nil, fmt.Errorf("precomp: load slots: %w", err)
	}
	health := map[string]SlotState{}
	for _, s := range states {
		health[s.ID] = s
	}

	out := &RowResult{Keys: map[string]string{}}
	// 计算上下文：已算出的上游算法结果也作为变量，供下游公式引用
	//
	// ★ 变量命名约定（本函数与 gate.CheckFormulaVariablesBound 必须一致）：
	//   * 槽 `slot.revenue`   → 变量名 `revenue`（BareName）
	//   * 算法 `algo.gp`      → 变量名 `gp`（fieldName）
	//   此前 **槽侧没有任何绑定**：`inputs` 由调用方给出，其键名是事实表的列名，
	//   与公式里的名字毫无约束关系 ⇒ 像 `algo.gp` 公式 `rev - cogs` 里的 `rev`
	//   永远取不到值，内核按 Missing 处理，配合 missing_policy=skip 让整列静默为空。
	//   现按 BareName 显式绑定一遍槽变量（同名时以调用方 inputs 为准，保持既有语义）。
	vars := map[string]compute.Scalar{}
	for k, v := range inputs {
		vars[k] = v
	}
	for _, a := range algos {
		for _, s := range a.DependsOnSlots {
			name := compute.BareName(s)
			if _, given := vars[name]; given {
				continue // 调用方显式给了同名原子量 ⇒ 尊重
			}
			if v, ok := inputs[s]; ok {
				// 也接受「用完整槽 ID 作键」的调用形态
				vars[name] = v
			}
		}
	}

	// 按定义顺序执行（依赖在前）
	for _, algoID := range def.ProducedBy {
		a, ok := byID[algoID]
		if !ok {
			continue
		}
		// 依赖槽门控：任一不可用 ⇒ 跳过该字段
		if bad, reason := firstBlocked(a.DependsOnSlots, health); bad != "" {
			out.Cells = append(out.Cells, CellResult{
				Field: fieldName(a.ID), Skipped: true, Reason: reason,
				Slots: a.DependsOnSlots,
			})
			out.SkippedFields = append(out.SkippedFields, fieldName(a.ID))
			// 明确写入缺失，供下游传播（不写 0）
			vars[fieldName(a.ID)] = nil
			continue
		}
		val, err := b.Kernel.EvalFormula(ctx, a.Formula, vars)
		if err != nil {
			return nil, fmt.Errorf("precomp: eval %s: %w", a.ID, err)
		}
		// missing_policy=skip 且结果为缺失 ⇒ 记录跳过（不写 0）
		if val == nil && a.MissingPolicy == "skip" {
			out.Cells = append(out.Cells, CellResult{
				Field: fieldName(a.ID), Skipped: true,
				Reason: "依赖值缺失（fail-closed）", Slots: a.DependsOnSlots,
			})
			out.SkippedFields = append(out.SkippedFields, fieldName(a.ID))
			vars[fieldName(a.ID)] = nil
			continue
		}
		out.Cells = append(out.Cells, CellResult{
			Field: fieldName(a.ID), Value: val, Slots: a.DependsOnSlots,
		})
		vars[fieldName(a.ID)] = val
	}
	return out, nil
}

// fieldName 算法 ID → 桶列名：algo.gp → gp，algo.net_contrib → net_contrib。
func fieldName(algoID string) string {
	const p = "algo."
	if len(algoID) > len(p) && algoID[:len(p)] == p {
		return algoID[len(p):]
	}
	return algoID
}

// FieldName 导出供测试与其他包使用。
func FieldName(algoID string) string { return fieldName(algoID) }

func firstBlocked(slots []string, health map[string]SlotState) (string, string) {
	for _, s := range slots {
		h, ok := health[s]
		if !ok {
			return s, fmt.Sprintf("slot %s not registered", s)
		}
		switch h.Status {
		case "MISSING", "DISABLED":
			return s, fmt.Sprintf("slot %s status=%s (hard unavailable)", s, h.Status)
		}
		if h.Coverage < h.Gate {
			return s, fmt.Sprintf("slot %s coverage %.4f < gate %.4f", s, h.Coverage, h.Gate)
		}
	}
	return "", ""
}

// ───────────────────────────── 版本漂移检测 ─────────────────────────────

// Drift 检测桶记录的版本与当前算法/规则版本是否一致。
// 返回漂移项（空 = 一致）。
func Drift(def BucketDef, currentAlgo, currentRule map[string]int) []string {
	var drift []string
	for id, want := range def.AlgoVersions {
		if got, ok := currentAlgo[id]; ok && got != want {
			drift = append(drift, fmt.Sprintf("algo %s: bucket=%d current=%d", id, want, got))
		}
	}
	for id, want := range def.RuleVersions {
		if got, ok := currentRule[id]; ok && got != want {
			drift = append(drift, fmt.Sprintf("rule %s: bucket=%d current=%d", id, want, got))
		}
	}
	sort.Strings(drift)
	return drift
}

// BuildLog 构建记录。
type BuildLog struct {
	BucketID    string
	StartedAt   time.Time
	FinishedAt  time.Time
	RowsWritten int
	RowsSkipped int
	Status      string
	Error       string
}
