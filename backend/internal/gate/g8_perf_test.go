// g8_perf_test.go —— G8 性能闸门测试。
package gate_test

import (
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

func TestG8_PerfWithinBudget(t *testing.T) {
	b := gate.DefaultPerfBudget()
	ok := gate.PerfSample{TTIMs: 800, FilterP95HitMs: 60, FilterP95MissMs: 300, BundleGzipBytes: 150 * 1024}
	if v := gate.CheckPerf(b, ok); len(v) != 0 {
		t.Fatalf("G8 预算内不应报错: %v", v)
	}
}

func TestG8_PerfExceedsBudget(t *testing.T) {
	b := gate.DefaultPerfBudget()
	bad := gate.PerfSample{TTIMs: 1500, FilterP95HitMs: 60, FilterP95MissMs: 300, BundleGzipBytes: 150 * 1024}
	v := gate.CheckPerf(b, bad)
	if len(v) == 0 {
		t.Fatal("G8 应检出 TTI 超标")
	}
	// 未命中 P95 超标也应检出
	bad2 := gate.PerfSample{TTIMs: 800, FilterP95HitMs: 60, FilterP95MissMs: 900, BundleGzipBytes: 150 * 1024}
	if v := gate.CheckPerf(b, bad2); len(v) == 0 {
		t.Fatal("G8 应检出未命中 P95 超标")
	}
}

func TestG8_Percentile(t *testing.T) {
	samples := []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	// 最近秩法：P95 → index int(9*0.95)=8 → 90
	if got := gate.Percentile(samples, 0.95); got != 90 {
		t.Fatalf("G8 P95 计算错误：got %d want 90", got)
	}
	if got := gate.Percentile(nil, 0.95); got != 0 {
		t.Fatalf("G8 空样本应返回 0，got %d", got)
	}
}
