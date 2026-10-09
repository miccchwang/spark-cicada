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
	"strings"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/compute"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// BucketDef 桶定义（对应 buckets/*.yaml）。
//
// ★ 2026-10-08 移除 `Indexes` 字段：它曾是**结构性死字段** ——
// precomp 只负责「按桶定义把算法结果物化进已有表」，**从不建表、也不建索引**，
// 所以这个字段被解析进来后没有任何读取点（全仓 `grep "\.Indexes"` 命中 0）。
// 桶的索引声明**只有一个权威位置**：`slot.Bucket.Indexes`（加载即过
// `gate.CheckBucketIndexesDeclared`），并与迁移 DDL 双向对平
// （`gate.CheckBucketIndexesMatchDDL`）。此处不留第二份副本 —— 留了就会分叉。
type BucketDef struct {
	ID           string
	Grain        []string
	ProducedBy   []string // 算法 ID 序列（按依赖顺序）
	Refresh      string
	AlgoVersions map[string]int
	RuleVersions map[string]int
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
	// Unit 该字段的计量单位（规范形，来自 AlgoDef.Unit）。
	//
	// ★ 为什么必须带出来（2026-10-08）：`AlgoDef.Unit` 在此之前是**结构性死字段** ——
	// 它被解析、被写进 registry_algorithm.unit，但 precomp.go 从头到尾**没有读过一次**
	// （BuildRow 只消费 Formula / DependsOnSlots / MissingPolicy）。于是
	// 「algo.gmp 的单位是比率」这条事实**永远到不了**下游渲染/导出层，
	// 页面上就会出现 `0.42 THB` 这种静默错误口径。
	// 现在单位随单元格一起产出，成为「哪些列可相加 / 该怎么渲染」的**唯一判据来源**。
	//
	// 归一：经 gate.ParseUnit 归一（`percent`/`pct`/`%` 同一形）；不可解析时
	// 保留原文（不静默丢弃，让下游看得见异常，而不是拿到空字符串当作「无单位」）。
	Unit string
}

// normalizedUnit 归一算法声明的计量单位。
//
// ★ 委托 gate.ParseUnit —— 归一表**只有一份权威**（gate.unitAliases），
// 防止「校验器认 percent、渲染器不认 percent」这类分叉静默发生
// （与 slot.Slot.FreshnessMinutes 委托 gate.ParseFreshness 同纪律）。
//
// 不可解析时**保留原文**而不是丢弃：下游看得见异常值，好过拿到空字符串
// 被当作「无单位」而静默按默认格式渲染。
func normalizedUnit(raw string) string {
	if u, ok := gate.ParseUnit(raw); ok {
		return u
	}
	return strings.TrimSpace(raw)
}

type RowResult struct {
	Keys          map[string]string
	Cells         []CellResult
	SkippedFields []string
}

// Units 返回「字段名 → 计量单位」的映射（仅含已产出的单元格）。
//
// ★ 这是「单位」第一次成为**可被下游消费的结构化输出**：
// 导出/看板按它决定数值格式与可加性，而不是靠字段名的字符串约定去猜。
func (r *RowResult) Units() map[string]string {
	out := make(map[string]string, len(r.Cells))
	for _, c := range r.Cells {
		if c.Unit != "" {
			out[c.Field] = c.Unit
		}
	}
	return out
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
				Slots: a.DependsOnSlots, Unit: normalizedUnit(a.Unit),
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
		// ── missing_policy 三值语义（G4 第十三侧 / docs/03 §3.1）─────────────
		//
		// ★ 此前这里唯一的判定是 `val == nil && a.MissingPolicy == "skip"`：
		//   其余一切取值（null / error / 任何拼错的值）统统落到下面的默认分支 ——
		//   写 NULL、不报错。于是 `missing_policy: error`（声明「缺失即报错」）
		//   与 `null` 与「随便写」**行为完全等价** ⇒ 声明等于装饰。
		//
		//   现在按声明**真的分三路**执行；策略不可解析则 fail-closed 拒绝构建
		//   （绝不猜一个默认值 —— 猜错方向会让「该报错」静默变「写空」）。
		policy, ok := gate.ParseMissingPolicy(a.MissingPolicy)
		if !ok {
			return nil, fmt.Errorf(
				"precomp: 算法 %s 的 missing_policy=%q 不是已知策略（允许：%s）—— fail-closed 拒绝构建",
				a.ID, a.MissingPolicy, strings.Join(gate.MissingPolicies(), "/"))
		}
		if val == nil {
			switch policy {
			case gate.MissingPolicySkip:
				// 跳过该字段（不写、记 skipped_fields，绝不写 0 冒充）。
				out.Cells = append(out.Cells, CellResult{
					Field: fieldName(a.ID), Skipped: true,
					Reason: "依赖值缺失（missing_policy=skip，fail-closed）",
					Slots:  a.DependsOnSlots, Unit: normalizedUnit(a.Unit),
				})
				out.SkippedFields = append(out.SkippedFields, fieldName(a.ID))
				vars[fieldName(a.ID)] = nil
				continue
			case gate.MissingPolicyError:
				// 缺失即报错：让构建失败，而不是静默产出空值。
				return nil, fmt.Errorf(
					"precomp: 算法 %s 依赖值缺失且 missing_policy=error ⇒ 拒绝构建（fail-closed）", a.ID)
			case gate.MissingPolicyNull:
				// 显式置空：写 NULL（Value=nil），**不**计入 skipped_fields
				// （下游据此区分「跳过」与「确实为空」）。
				out.Cells = append(out.Cells, CellResult{
					Field: fieldName(a.ID), Value: nil,
					Reason: "依赖值缺失（missing_policy=null，显式置空）",
					Slots:  a.DependsOnSlots, Unit: normalizedUnit(a.Unit),
				})
				vars[fieldName(a.ID)] = nil
				continue
			}
		}
		out.Cells = append(out.Cells, CellResult{
			Field: fieldName(a.ID), Value: val, Slots: a.DependsOnSlots,
			Unit: normalizedUnit(a.Unit),
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
