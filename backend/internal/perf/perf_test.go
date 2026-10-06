package perf_test

import (
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/perf"
)

// 全部四项都在预算内，且都声明为已测 ⇒ 通过。
func TestJudge_AllMeasuredWithinBudget_Pass(t *testing.T) {
	r := perf.Report{
		TTIMs:           800,
		FilterP95HitMs:  50,
		FilterP95MissMs: 300,
		BundleGzipBytes: 120 * 1024,
		Measured:        []string{"tti", "filter_p95_hit", "filter_p95_miss", "bundle_gzip"},
	}
	v, err := perf.Judge(r, gate.DefaultPerfBudget(), true)
	if err != nil {
		t.Fatalf("Judge 报错: %v", err)
	}
	if !v.Passed {
		t.Fatalf("应通过，却未通过: %+v", v)
	}
	if len(v.Unmeasured) != 0 {
		t.Fatalf("不应有未测项，得到 %v", v.Unmeasured)
	}
	if len(v.Violations) != 0 {
		t.Fatalf("不应有超标项，得到 %v", v.Violations)
	}
}

// 已测指标超标 ⇒ 必须失败，且违规说明来自 gate.CheckPerf。
//
// 这条断言同时证明「gate.CheckPerf 被真的调用了」——
// 若判定器把 CheckPerf 摘掉（退化成永远通过），本用例会失败。
func TestJudge_MeasuredOverBudget_Fails(t *testing.T) {
	r := perf.Report{
		TTIMs:    5000, // > 1000ms
		Measured: []string{"tti"},
	}
	v, err := perf.Judge(r, gate.DefaultPerfBudget(), false)
	if err != nil {
		t.Fatalf("Judge 报错: %v", err)
	}
	if v.Passed {
		t.Fatal("TTI 5000ms 超预算，却判通过 —— 判定器没有真的调用 gate.CheckPerf")
	}
	if len(v.Violations) == 0 {
		t.Fatal("应给出违规说明")
	}
	if !strings.Contains(v.Violations[0], "超标") {
		t.Fatalf("违规说明应来自 gate.CheckPerf（含「超标」），得到 %q", v.Violations[0])
	}
}

// ★ 核心纪律：未在 Measured 里的指标，其数值字段**不采信**。
//
// 若判定器改成「看字段有没有填」而不是「看 Measured」，
// 那么一个巨大但未测的 TTI 会凭空造出违规（假红）；
// 反过来，若判定器把未测当 0 喂进去，就会掩盖「其实没测」（假绿）。
// 本用例钉死「未测 = 不采信，且如实列出」。
func TestJudge_UnmeasuredFieldsAreNotCredited(t *testing.T) {
	r := perf.Report{
		// 字段里塞了巨大的值，但没有声明为已测 —— 不得采信。
		TTIMs:           999999,
		FilterP95HitMs:  999999,
		FilterP95MissMs: 999999,
		BundleGzipBytes: 120 * 1024,
		Measured:        []string{"bundle_gzip"},
	}
	v, err := perf.Judge(r, gate.DefaultPerfBudget(), false)
	if err != nil {
		t.Fatalf("Judge 报错: %v", err)
	}
	if len(v.Violations) != 0 {
		t.Fatalf("未测字段不得产生违规，得到 %v", v.Violations)
	}
	if !v.Passed {
		t.Fatalf("仅 bundle_gzip 已测且达标，非严格模式应通过: %+v", v)
	}
	want := []string{"tti", "filter_p95_hit", "filter_p95_miss"}
	if strings.Join(v.Unmeasured, ",") != strings.Join(want, ",") {
		t.Fatalf("未测项应为 %v，得到 %v", want, v.Unmeasured)
	}
}

// strict ⇒ 有未测项即失败（「没测」不能当「通过」）。
func TestJudge_StrictFailsOnUnmeasured(t *testing.T) {
	r := perf.Report{BundleGzipBytes: 10, Measured: []string{"bundle_gzip"}}
	v, err := perf.Judge(r, gate.DefaultPerfBudget(), true)
	if err != nil {
		t.Fatalf("Judge 报错: %v", err)
	}
	if v.Passed {
		t.Fatal("strict 模式下有未测项应判失败")
	}
	if len(v.Violations) != 0 {
		t.Fatalf("未测项不应产生「超标」违规（它们是「没测」不是「超标」），得到 %v", v.Violations)
	}
	if len(v.Unmeasured) == 0 {
		t.Fatal("应列出未测项")
	}
}

// 未知指标名 ⇒ 报错（防止报告格式漂移后静默变成「什么都没测」）。
func TestJudge_UnknownMetric_Error(t *testing.T) {
	r := perf.Report{Measured: []string{"bundle_size"}} // 拼错的名字
	if _, err := perf.Judge(r, gate.DefaultPerfBudget(), false); err == nil {
		t.Fatal("未知指标名应报错")
	}
}

// 已测指标为负 ⇒ 报错（负值是无效测量，且会与未测哨兵混淆）。
func TestJudge_NegativeMeasuredValue_Error(t *testing.T) {
	r := perf.Report{TTIMs: -5, Measured: []string{"tti"}}
	if _, err := perf.Judge(r, gate.DefaultPerfBudget(), false); err == nil {
		t.Fatal("已测指标为负应报错")
	}
}

// 预算非正 ⇒ 报错（零/负预算没有意义，且会让哨兵失去安全性）。
func TestJudge_NonPositiveBudget_Error(t *testing.T) {
	for _, b := range []gate.PerfBudget{
		{TTIMs: 0, FilterP95HitMs: 100, FilterP95MissMs: 500, BundleGzipBytes: 200},
		{TTIMs: 1000, FilterP95HitMs: -1, FilterP95MissMs: 500, BundleGzipBytes: 200},
	} {
		if _, err := perf.Judge(perf.Report{}, b, false); err == nil {
			t.Fatalf("预算 %+v 非正，应报错", b)
		}
	}
}

// 解析：未知字段应报错（格式漂移不许静默通过）。
func TestParseReport_RejectsUnknownField(t *testing.T) {
	good := []byte(`{"bundleGzipBytes": 123, "measured": ["bundle_gzip"]}`)
	r, err := perf.ParseReport(good)
	if err != nil {
		t.Fatalf("合法报告解析失败: %v", err)
	}
	if r.BundleGzipBytes != 123 || len(r.Measured) != 1 {
		t.Fatalf("解析结果不对: %+v", r)
	}

	bad := []byte(`{"bundle_size": 123, "measured": ["bundle_gzip"]}`) // 拼错的键
	if _, err := perf.ParseReport(bad); err == nil {
		t.Fatal("未知字段应报错")
	}
}

// AllMetrics 是唯一权威清单：四项、顺序稳定。
func TestAllMetrics_Stable(t *testing.T) {
	got := strings.Join(func() []string {
		out := make([]string, 0, len(perf.AllMetrics))
		for _, m := range perf.AllMetrics {
			out = append(out, string(m))
		}
		return out
	}(), ",")
	want := "tti,filter_p95_hit,filter_p95_miss,bundle_gzip"
	if got != want {
		t.Fatalf("AllMetrics 顺序/内容漂移: got %q want %q", got, want)
	}
}
