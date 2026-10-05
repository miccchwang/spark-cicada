// G8 · 性能闸门 —— 把 docs/05 的阈值变成可失败的断言。
//
// 阈值（docs/05 G8）：
//   * 首屏 TTI            < 1000 ms
//   * 筛选 P95（命中预计算） < 100 ms
//   * 筛选 P95（未命中）    < 500 ms
//   * 首屏产物 gzip        < 200 KB
//
// 说明：本文件提供**阈值定义 + 判定函数**；真实测量（Playwright / Lighthouse）
// 在 CI 的 gate-perf 阶段产出数字后喂给这里的判定函数。
// 这样「阈值」只有一处定义，文档、CI、报表共用，不会漂移。
package gate

import "fmt"

// PerfBudget 性能预算（单位：ms / bytes）。
type PerfBudget struct {
	TTIMs               int
	FilterP95HitMs      int
	FilterP95MissMs     int
	BundleGzipBytes     int
}

// DefaultPerfBudget 返回 docs/05 G8 的默认阈值。
func DefaultPerfBudget() PerfBudget {
	return PerfBudget{
		TTIMs:           1000,
		FilterP95HitMs:  100,
		FilterP95MissMs: 500,
		BundleGzipBytes: 200 * 1024,
	}
}

// PerfSample 一次实测结果。
type PerfSample struct {
	TTIMs           int
	FilterP95HitMs  int
	FilterP95MissMs int
	BundleGzipBytes int
}

// CheckPerf 逐项对比实测与预算，返回所有超标项（空 = 通过）。
func CheckPerf(b PerfBudget, s PerfSample) []string {
	var out []string
	check := func(name string, got, budget int, unit string) {
		if got > budget {
			out = append(out, fmt.Sprintf("%s 超标：实测 %d%s > 预算 %d%s",
				name, got, unit, budget, unit))
		}
	}
	check("首屏 TTI", s.TTIMs, b.TTIMs, "ms")
	check("筛选 P95（命中预计算）", s.FilterP95HitMs, b.FilterP95HitMs, "ms")
	check("筛选 P95（未命中）", s.FilterP95MissMs, b.FilterP95MissMs, "ms")
	check("首屏产物 gzip", s.BundleGzipBytes, b.BundleGzipBytes, "B")
	return out
}

// Percentile 从一组耗时样本里取 P95（最近秩法，无需排序库）。
//
// 用于把多次采样汇总成一个可判定的数字；样本为空返回 0。
func Percentile(samples []int, p float64) int {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]int, len(samples))
	copy(cp, samples)
	// 插入排序（样本量小，够用且零依赖）
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	idx := int(float64(len(cp)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}
