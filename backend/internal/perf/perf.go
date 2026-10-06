// Package perf —— G8 性能闸门的**执行侧**（判定器 + 实测报告的采信纪律）。
//
// 为什么需要这个包（★ 真实缺口）：
//
//	docs/05 的 G8 表把四条阈值写得很清楚，`gate.CheckPerf` 也实现了判定 ——
//	但全仓**没有任何非测试代码调用 `CheckPerf`**：
//	  * `grep -rn CheckPerf` 只命中它自己的 `_test.go`；
//	  * `.github/workflows/ci.yml` 里根本没有 `gate-perf` 作业
//	    （docs/05 尾注却写着「实测由 CI 的 gate-perf 阶段产出后喂给 CheckPerf」）。
//	于是四条阈值**一条都没有真的在把关**，而文档标着「✅ 已实现」。
//	这是同一个病在本仓库的**第七个变种**（前六见 docs/11 / docs/05 的收口记录）：
//	「闸门依赖的判定函数全仓没有任何非测试调用点」。
//
// 本包把 `gate.CheckPerf` 接上真实链路：
//
//  1. `Report` 显式声明**哪些指标真的被测过**（`Measured`）；
//  2. `Judge` 只采信 `Measured` 里的指标 —— 未测指标的字段即便填了值也**不采信**
//     （否则「填个 0 就通过」会让闸门再次形同虚设）；
//  3. `strict` 模式下，**未测指标一律判失败**（fail-closed）——
//     「没测」不能被当成「通过」。
//
// 实测数字由 `cmd/spark-perf` 产出（`--measure-bundle` 现在就能给出真实的
// 首屏产物 gzip 体积；TTI / 筛选 P95 需要无头浏览器 / 真库，见 docs/05 尾注）。
package perf

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// Metric 是 G8 的四项指标名（与 docs/05 G8 表逐行对应）。
type Metric string

const (
	// MetricTTI 首屏 TTI（ms）。
	MetricTTI Metric = "tti"
	// MetricFilterP95Hit 筛选 P95（命中预计算）（ms）。
	MetricFilterP95Hit Metric = "filter_p95_hit"
	// MetricFilterP95Miss 筛选 P95（未命中）（ms）。
	MetricFilterP95Miss Metric = "filter_p95_miss"
	// MetricBundleGzip 首屏产物 gzip（bytes）。
	MetricBundleGzip Metric = "bundle_gzip"
)

// AllMetrics 是 G8 的全部四项指标，顺序与 docs/05 G8 表一致。
//
// 「全部指标」必须有一处唯一定义：判定完备性（strict 模式下是否四项都测了）
// 依赖它，散落的字面量会让「新增一项却忘了接进闸门」再次发生。
var AllMetrics = []Metric{MetricTTI, MetricFilterP95Hit, MetricFilterP95Miss, MetricBundleGzip}

// unmeasuredSentinel 是喂给 `gate.CheckPerf` 的「未测」占位值。
//
// 取负数：`CheckPerf` 的判定是 `got > budget`，而预算被强制为正（见 Judge），
// 故哨兵**永远不会**被误判为超标 —— 未测指标不会凭空制造违规，
// 也不会被当成 0 而「悄悄通过」（0 也会通过，但那会掩盖「其实没测」）。
const unmeasuredSentinel = -1

// Report 是一次 G8 实测报告。
//
// ★ `Measured` 是**唯一**的「测过没」依据：不在其中的指标，
// 其数值字段**一律不采信**（哪怕填了值）。这样「漏测」与「测得为 0」
// 是两种可区分的事实，而不是同一个 0。
type Report struct {
	TTIMs           int      `json:"ttiMs"`
	FilterP95HitMs  int      `json:"filterP95HitMs"`
	FilterP95MissMs int      `json:"filterP95MissMs"`
	BundleGzipBytes int      `json:"bundleGzipBytes"`
	Measured        []string `json:"measured"`
}

// ParseReport 从 JSON 解析报告。
//
// 不允许未知字段：报告格式若悄悄漂移（例如有人把 `measured` 写成 `metrics`），
// 解析应**报错**而不是得到一个「什么都没测」的空报告 ——
// 后者会在非 strict 模式下变成一次静默通过。
func ParseReport(b []byte) (Report, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		return Report{}, fmt.Errorf("perf: 解析实测报告失败: %w", err)
	}
	return r, nil
}

// measuredSet 把 Measured 归一化为集合，并校验指标名合法。
func (r Report) measuredSet() (map[Metric]bool, error) {
	set := map[Metric]bool{}
	for _, raw := range r.Measured {
		name := Metric(strings.TrimSpace(raw))
		if !isKnownMetric(name) {
			return nil, fmt.Errorf("perf: 未知指标名 %q（合法值：%s）",
				raw, strings.Join(metricNames(), ", "))
		}
		set[name] = true
	}
	return set, nil
}

// valueOf 返回某指标在报告中的数值（仅当它被采信时才有意义）。
func (r Report) valueOf(m Metric) int {
	switch m {
	case MetricTTI:
		return r.TTIMs
	case MetricFilterP95Hit:
		return r.FilterP95HitMs
	case MetricFilterP95Miss:
		return r.FilterP95MissMs
	case MetricBundleGzip:
		return r.BundleGzipBytes
	default:
		return 0
	}
}

// Verdict 是判定结果。
type Verdict struct {
	// Passed 为 true 当且仅当：没有任何**已测**指标超标，
	// 且（strict 时）没有任何指标未测。
	Passed bool `json:"passed"`
	// Violations 是已测指标的超标说明（来自 `gate.CheckPerf`，人类可读）。
	Violations []string `json:"violations"`
	// Unmeasured 是**未测**的指标名（按 AllMetrics 顺序）。
	// 它们既不贡献违规、也不贡献通过 —— 只是被如实列出。
	Unmeasured []string `json:"unmeasured"`
	// Measured 是**已测**的指标名（按 AllMetrics 顺序）。
	Measured []string `json:"measured"`
	// Strict 记录本次判定是否启用了 fail-closed。
	Strict bool `json:"strict"`
	// Summary 是一句人类可读的结论。
	Summary string `json:"summary"`
}

// Judge 用预算 b 判定报告 r。
//
// 纪律（缺一不可）：
//  1. 只把 `Measured` 里的指标喂给 `gate.CheckPerf` —— 未测字段不采信；
//  2. 已测指标的值不得为负（负值是「无效测量」，不是「很低」）；
//  3. 预算必须为正（零/负预算没有意义，且会让哨兵失去安全性）；
//  4. strict ⇒ 有任一未测指标即判失败（「没测」不等于「通过」）。
func Judge(r Report, b gate.PerfBudget, strict bool) (Verdict, error) {
	if err := validateBudget(b); err != nil {
		return Verdict{}, err
	}
	set, err := r.measuredSet()
	if err != nil {
		return Verdict{}, err
	}
	// 已测指标不得为负：负值无法与「未测哨兵」区分，必须显式拒绝。
	for _, m := range AllMetrics {
		if set[m] && r.valueOf(m) < 0 {
			return Verdict{}, fmt.Errorf("perf: 指标 %s 已声明为已测，但值为 %d（不得为负）",
				m, r.valueOf(m))
		}
	}

	// 只采信已测指标；未测用哨兵（见 unmeasuredSentinel 的说明）。
	sample := gate.PerfSample{
		TTIMs:           unmeasuredSentinel,
		FilterP95HitMs:  unmeasuredSentinel,
		FilterP95MissMs: unmeasuredSentinel,
		BundleGzipBytes: unmeasuredSentinel,
	}
	if set[MetricTTI] {
		sample.TTIMs = r.TTIMs
	}
	if set[MetricFilterP95Hit] {
		sample.FilterP95HitMs = r.FilterP95HitMs
	}
	if set[MetricFilterP95Miss] {
		sample.FilterP95MissMs = r.FilterP95MissMs
	}
	if set[MetricBundleGzip] {
		sample.BundleGzipBytes = r.BundleGzipBytes
	}

	// ★ 这里是 `gate.CheckPerf` 的**真实生产调用点**（此前全仓没有）。
	violations := gate.CheckPerf(b, sample)
	if violations == nil {
		violations = []string{}
	}

	var measuredNames, unmeasuredNames []string
	for _, m := range AllMetrics {
		if set[m] {
			measuredNames = append(measuredNames, string(m))
		} else {
			unmeasuredNames = append(unmeasuredNames, string(m))
		}
	}
	if measuredNames == nil {
		measuredNames = []string{}
	}
	if unmeasuredNames == nil {
		unmeasuredNames = []string{}
	}

	passed := len(violations) == 0
	if strict && len(unmeasuredNames) > 0 {
		passed = false
	}

	return Verdict{
		Passed:     passed,
		Violations: violations,
		Unmeasured: unmeasuredNames,
		Measured:   measuredNames,
		Strict:     strict,
		Summary:    summarize(passed, len(measuredNames), violations, unmeasuredNames, strict),
	}, nil
}

// summarize 产出一句人类可读结论（不含换行，便于写进 CI 单行日志）。
func summarize(passed bool, measuredCount int, violations, unmeasured []string, strict bool) string {
	var sb strings.Builder
	if passed {
		sb.WriteString("G8 通过")
	} else {
		sb.WriteString("G8 未通过")
	}
	sb.WriteString(fmt.Sprintf("（已测 %d 项", measuredCount))
	if len(violations) > 0 {
		sb.WriteString(fmt.Sprintf("，超标 %d 项", len(violations)))
	}
	sb.WriteString("）")
	if len(unmeasured) > 0 {
		mode := "非严格：未测项不判失败"
		if strict {
			mode = "严格：未测项判失败"
		}
		sb.WriteString(fmt.Sprintf("；未测 %d 项[%s]（%s）",
			len(unmeasured), strings.Join(unmeasured, ","), mode))
	}
	return sb.String()
}

// validateBudget 强制预算为正 —— 零/负预算没有意义，
// 且会让「未测哨兵（-1）」有被误判为超标的可能。
func validateBudget(b gate.PerfBudget) error {
	fields := []struct {
		name  string
		value int
	}{
		{"首屏 TTI", b.TTIMs},
		{"筛选 P95（命中预计算）", b.FilterP95HitMs},
		{"筛选 P95（未命中）", b.FilterP95MissMs},
		{"首屏产物 gzip", b.BundleGzipBytes},
	}
	for _, f := range fields {
		if f.value <= 0 {
			return fmt.Errorf("perf: 预算 %q 必须为正，实际 %d", f.name, f.value)
		}
	}
	return nil
}

func isKnownMetric(m Metric) bool {
	for _, k := range AllMetrics {
		if k == m {
			return true
		}
	}
	return false
}

func metricNames() []string {
	names := make([]string, 0, len(AllMetrics))
	for _, m := range AllMetrics {
		names = append(names, string(m))
	}
	sort.Strings(names)
	return names
}
