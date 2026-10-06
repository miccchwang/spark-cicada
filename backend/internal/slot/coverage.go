// coverage.go —— 覆盖率观测 → gate.DecideCoverage 的**真实生产链路**（G5）。
//
// docs/05 G5 的两条断言：
//
//  1. 覆盖率 < 门限 ⇒ 依赖算法 skip
//  2. 不补 0、不摊分（输出为 null）
//
// 在它出现之前，`gate.DecideCoverage` 的唯一非测试调用点就是它自己的测试 ——
// 也就是说「覆盖率不足就跳过」这条红线上**从没有过一个真实的槽状态**。
//
// 本文件把「注册表里的槽（静态门限 + 声明状态）」与「观测到的实测覆盖率」
// 合成 `gate.CoverageCase`，交给 `gate.DecideCoverage` 判定，
// 并给出**逐槽的跳过原因**（原因可审计，而不是一个真假值）。
package slot

import (
	"fmt"
	"sort"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// Observation 一次覆盖率观测（来自采集/对账，键 = 槽 ID）。
//
//	coverage: 实测覆盖率 0.0–1.0；未观测到的槽不出现（缺省即未知）。
//	status:   运行时的槽状态（ACTIVE/MISSING/DEGRADED/DISABLED）；
//	          留空则由注册表的静态 status 推导（AVAILABLE→ACTIVE，PARTIAL→DEGRADED，MISSING→MISSING）。
type Observation struct {
	Coverage map[string]float64
	Status   map[string]string
}

// GateVerdict 一个槽的门控结论。
type GateVerdict struct {
	SlotID   string
	Skip     bool
	Reason   string
	Coverage float64
	Gate     float64
	Status   string
}

// Cases 把注册表与观测合成为 `gate.CoverageCase`（按槽 ID 升序，确定性）。
//
// ★ 关键设计：**未观测的槽按 MISSING 处理**（而非「默认通过」）——
// 观测缺口的默认动作必须是 fail-closed，否则「没采到数据」会伪装成「数据齐全」。
// 这与 docs/03 §2.1「MISSING ⇒ skip」同源。
func (r *Registry) Cases(obs Observation) []gate.CoverageCase {
	var out []gate.CoverageCase
	for _, s := range r.Slots() {
		cov, observed := obs.Coverage[s.ID]
		status := obs.Status[s.ID]

		if !observed && status == "" {
			// 未观测 ⇒ 保守视为 MISSING（硬跳过），而不是拿门限去比一个假 0
			status = StatusMissing
		}
		if status == "" {
			status = runtimeStatus(s.Status, cov, s.CoverageGate)
		}
		out = append(out, gate.CoverageCase{
			SlotID:   s.ID,
			Coverage: cov,
			Gate:     s.CoverageGate,
			Status:   status,
		})
	}
	return out
}

// runtimeStatus 由静态状态 + 实测覆盖率推导运行时状态。
//
// 规则（docs/03 §2.1）：
//   - 注册表标 MISSING ⇒ 运行时 MISSING（无权威来源，覆盖率无意义）；
//   - 覆盖率 ≥ 门限 ⇒ ACTIVE；否则 DEGRADED（低于门限即视为不可用）。
func runtimeStatus(static string, cov, gateValue float64) string {
	if static == StatusMissing {
		return StatusMissing
	}
	if cov >= gateValue {
		return "ACTIVE"
	}
	return "DEGRADED"
}

// Judge 用 `gate.DecideCoverage`（G5 的判定函数）给出**逐槽**结论。
//
// ★ 这是 G5 闸门第一次被生产代码调用。返回顺序与 Cases 一致（槽 ID 升序）。
//
// 注意：`gate.DecideCoverage` 是**任一槽不合格即整体跳过**的语义（它按算法为单位
// 收集依赖槽）；本方法对**单槽**调用它，等价于「这个槽自己能不能用」。
// 多槽并集（一个算法的全部依赖槽）由 JudgesForAlgorithm 负责。
func (r *Registry) Judge(obs Observation) []GateVerdict {
	cases := r.Cases(obs)
	out := make([]GateVerdict, 0, len(cases))
	for _, c := range cases {
		v := gate.DecideCoverage([]gate.CoverageCase{c})
		out = append(out, GateVerdict{
			SlotID:   c.SlotID,
			Skip:     v.Skip,
			Reason:   v.Reason,
			Coverage: c.Coverage,
			Gate:     c.Gate,
			Status:   c.Status,
		})
	}
	return out
}

// AlgoVerdict 一个算法的可用性结论（其依赖槽的并集）。
type AlgoVerdict struct {
	AlgoID    string
	Skip      bool
	Reason    string
	SkipSlots []string // 触发跳过的槽（可审计）
}

// JudgesForAlgorithm 判断每个算法是否可执行：只要**任一**依赖槽被跳过，
// 整个算法就 skip（docs/03 §5 第 3 步「任一 NOT AVAILABLE ⇒ skip」）。
//
// 缺依赖槽定义（未注册）也判 skip：不能在「槽不存在」时假装数据可用。
//
// ★ 这是「覆盖率门控 ⇒ 算法跳过」这条链路的真实实现：
//
//	RegisterYAML → Cases/Judge → gate.DecideCoverage → Algorithm.skip
func (r *Registry) JudgesForAlgorithm(obs Observation) []AlgoVerdict {
	perSlot := map[string]GateVerdict{}
	for _, v := range r.Judge(obs) {
		perSlot[v.SlotID] = v
	}

	algos := r.Algorithms()
	out := make([]AlgoVerdict, 0, len(algos))
	for _, a := range algos {
		av := AlgoVerdict{AlgoID: a.ID}
		var reasons []string
		for _, sid := range a.DependsOnSlots {
			v, ok := perSlot[sid]
			if !ok {
				av.Skip = true
				av.SkipSlots = append(av.SkipSlots, sid)
				reasons = append(reasons, fmt.Sprintf("槽 %s 未注册", sid))
				continue
			}
			if v.Skip {
				av.Skip = true
				av.SkipSlots = append(av.SkipSlots, sid)
				reasons = append(reasons, v.Reason)
			}
		}
		sort.Strings(av.SkipSlots)
		if av.Skip {
			av.Reason = "skip: " + joinReasons(reasons)
		}
		out = append(out, av)
	}
	return out
}

// joinReasons 把多槽原因拼成单行（确定性，便于日志与告警去重）。
func joinReasons(reasons []string) string {
	sort.Strings(reasons)
	out := ""
	for i, s := range reasons {
		if i > 0 {
			out += " | "
		}
		out += s
	}
	return out
}
