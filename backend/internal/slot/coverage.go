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
//	coverage:  实测覆盖率 0.0–1.0；未观测到的槽不出现（缺省即未知）。
//	status:    运行时的槽状态（ACTIVE/MISSING/DEGRADED/DISABLED）；
//	           留空则由注册表的静态 status 推导（AVAILABLE→ACTIVE，PARTIAL→DEGRADED，MISSING→MISSING）。
//	ageMinutes: 距**上次成功采集**的分钟数（键 = 槽 ID）；
//	           未出现的槽按「不知龄」处理（见 StaleUnknown），不是「新鲜」。
type Observation struct {
	Coverage   map[string]float64
	Status     map[string]string
	AgeMinutes map[string]int
}

// StaleUnknown 是「槽声明了时效、但本次没有它的新鲜度读数」时的判活口径。
//
// ★ 为什么必须 fail-closed：`coverage` 侧已有同款纪律（未观测 ⇒ MISSING ⇒ 硬跳过）。
// 若这边反过来把「没读到龄」当作「新鲜」，那么采集器**整个挂掉**这条通道
// 反而会让所有槽都判活 —— 失效方向恰好反了。
// 因此：**无法确认新鲜 ⇒ 不视为新鲜**。要放行，就得交出读数。
const StaleUnknown = true

// StaleVerdict 新鲜度判定结果。
type StaleVerdict struct {
	SlotID  string
	Stale   bool
	Reason  string
	Age     int // 分钟；未知时 < 0
	Allowed int // 时效上限（分钟）
}

// CheckFreshness 用注册表的 `freshness` 声明判定每个槽是否**已过期**。
//
// ★ 这是 `freshness` 字段**第一次被真正消费**。在它出现之前：
//
//	slots/*.yaml 的 16 个槽全都写着 freshness: 1d 或 7d，
//	而全仓没有任何代码读它 —— 一份停在三个月前的槽快照会被照常采用，
//	报表给出的是**旧的**数字，且没有任何信号（覆盖率可能还是 100%）。
//
// 判活口径：
//   - 槽在注册表里 → 用它的 freshness 作为上限；
//   - **未注册**的槽：无法知道上限 ⇒ 视为过期（fail-closed，与 JudgesForAlgorithm 同源）；
//   - 声明不可解析 ⇒ 视为过期（注册表加载时本就会报错，这里是第二道防线）；
//   - 没有读数（AgeMinutes 里不存在）⇒ **过期**（StaleUnknown），不是新鲜；
//   - 年龄为负 ⇒ 视为过期（时钟回拨/脏数据不该被当作「很新鲜」）。
func (r *Registry) CheckFreshness(obs Observation) []StaleVerdict {
	ids := make([]string, 0, len(r.order))
	ids = append(ids, r.order...)

	out := make([]StaleVerdict, 0, len(ids))
	for _, id := range ids {
		s, ok := r.slots[id]
		if !ok {
			continue
		}
		limit, parseOK := s.FreshnessMinutes()
		v := StaleVerdict{SlotID: id, Age: -1, Allowed: limit}
		if !parseOK {
			v.Stale, v.Reason = true, fmt.Sprintf(
				"槽 %s 的 freshness=%q 不可解析 ⇒ 无法确认时效，按过期处理", id, s.Freshness)
			out = append(out, v)
			continue
		}
		age, measured := obs.AgeMinutes[id]
		if !measured {
			v.Stale, v.Reason = StaleUnknown, fmt.Sprintf(
				"槽 %s 无新鲜度读数（声明时效 %s）⇒ 无法确认新鲜，按过期处理（fail-closed）",
				id, s.Freshness)
			out = append(out, v)
			continue
		}
		v.Age = age
		if age < 0 {
			v.Stale, v.Reason = true, fmt.Sprintf(
				"槽 %s 的龄为负（%d 分钟）⇒ 时钟/数据异常，按过期处理", id, age)
			out = append(out, v)
			continue
		}
		if age > limit {
			v.Stale, v.Reason = true, fmt.Sprintf(
				"槽 %s 数据已过期：距上次采集 %d 分钟 > 时效上限 %s（%d 分钟）",
				id, age, s.Freshness, limit)
			out = append(out, v)
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SlotID < out[j].SlotID })
	return out
}

// Cases 把注册表与观测合成为 `gate.CoverageCase`（按槽 ID 升序，确定性）。
//
// ★ 关键设计：**未观测的槽按 MISSING 处理**（而非「默认通过」）——
// 观测缺口的默认动作必须是 fail-closed，否则「没采到数据」会伪装成「数据齐全」。
// 这与 docs/03 §2.1「MISSING ⇒ skip」同源。
//
// ★ 新鲜度并入（2026-10-07）：**过期的槽按 DEGRADED 处理**，与「覆盖率低于门限」
// 是同一个后果（跳过硬不可用）。理由：一份覆盖率 100% 但停在三个月前快照的槽，
// 跑出来的 P&L 是**旧的**，而覆盖率单看是「完好的」—— 这正是 freshness
// 此前无人消费时最危险的失效形态。
func (r *Registry) Cases(obs Observation) []gate.CoverageCase {
	stale := map[string]StaleVerdict{}
	for _, v := range r.CheckFreshness(obs) {
		stale[v.SlotID] = v
	}

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
		// 过期 ⇒ 降级为 DEGRADED（硬跳过）。注意此处**不覆盖** MISSING：
		// 无权威来源的槽本来就该是 MISSING，报 MISSING 比报 DEGRADED 更准确。
		if sv, ok := stale[s.ID]; ok && sv.Stale && status != StatusMissing {
			status = "DEGRADED"
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

// GateVerdict 一个槽的门控结论。
type GateVerdict struct {
	SlotID   string
	Skip     bool
	Reason   string
	Coverage float64
	Gate     float64
	Status   string
	// Stale / StaleReason：新鲜度判活的结论（可审计）。
	// ★ 单列这两个字段而不是把原因拼进 Reason：调用方常需要
	// 「因覆盖率跳过」与「因过期跳过」分开统计/告警，合在一起就无法区分。
	Stale       bool
	StaleReason string
	// AgeMinutes 参与判定的龄（分钟）；无读数时为 -1。
	AgeMinutes int
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
	stale := map[string]StaleVerdict{}
	for _, v := range r.CheckFreshness(obs) {
		stale[v.SlotID] = v
	}
	out := make([]GateVerdict, 0, len(cases))
	for _, c := range cases {
		v := gate.DecideCoverage([]gate.CoverageCase{c})
		gv := GateVerdict{
			SlotID:     c.SlotID,
			Skip:       v.Skip,
			Reason:     v.Reason,
			Coverage:   c.Coverage,
			Gate:       c.Gate,
			Status:     c.Status,
			AgeMinutes: -1,
		}
		if sv, ok := stale[c.SlotID]; ok {
			gv.Stale = sv.Stale
			gv.StaleReason = sv.Reason
			gv.AgeMinutes = sv.Age
		}
		out = append(out, gv)
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
