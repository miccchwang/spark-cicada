package precomp_test

import (
	"context"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/precomp"
)

// fakeKernel 实现 compute.Kernel，用内置公式求值语义（仅用于测试 Go 侧编排逻辑）。
type fakeKernel struct{}

func (fakeKernel) EvalFormula(_ context.Context, formula string, vars map[string]*float64) (*float64, error) {
	// 只支持测试所需的最小公式集，模拟 Rust 内核的 fail-closed 语义
	get := func(k string) (*float64, bool) { v, ok := vars[k]; return v, ok }
	switch formula {
	case "rev - cogs":
		rev, ok1 := get("rev")
		cogs, ok2 := get("cogs")
		if !ok1 || !ok2 || rev == nil || cogs == nil {
			return nil, nil // 缺失 → nil（不补 0）
		}
		r := *rev - *cogs
		return &r, nil
	case "rev == 0 ? null : gp / rev":
		rev, ok1 := get("rev")
		gp, ok2 := get("gp")
		if !ok1 || !ok2 || rev == nil || gp == nil {
			return nil, nil
		}
		if *rev == 0 {
			return nil, nil // 分母 0 → null
		}
		r := *gp / *rev
		return &r, nil
	case "cm2 - overhead_alloc":
		a, ok1 := get("cm2")
		b, ok2 := get("overhead_alloc")
		if !ok1 || !ok2 || a == nil || b == nil {
			return nil, nil
		}
		r := *a - *b
		return &r, nil
	default:
		return nil, nil
	}
}

func (fakeKernel) Health(context.Context) (string, error) { return "test", nil }

func newBuilder(slots []precomp.SlotState) *precomp.Builder {
	return &precomp.Builder{
		Kernel: fakeKernel{},
		Slots: func(_ context.Context, ids []string) ([]precomp.SlotState, error) {
			var out []precomp.SlotState
			for _, id := range ids {
				for _, s := range slots {
					if s.ID == id {
						out = append(out, s)
					}
				}
			}
			return out, nil
		},
		Algos: func(_ context.Context, ids []string) ([]precomp.AlgoDef, error) {
			defs := map[string]precomp.AlgoDef{
				"algo.rev":   {ID: "algo.rev", Version: 1, Formula: "rev", MissingPolicy: "skip"},
				"algo.cogs":  {ID: "algo.cogs", Version: 3, Formula: "rev - cogs", DependsOnSlots: []string{"slot.qty", "slot.cost_unit"}, MissingPolicy: "skip"},
				"algo.gp":    {ID: "algo.gp", Version: 3, Formula: "rev - cogs", DependsOnSlots: []string{"slot.revenue", "slot.cogs"}, MissingPolicy: "skip"},
				"algo.gmp":   {ID: "algo.gmp", Version: 3, Formula: "rev == 0 ? null : gp / rev", DependsOnSlots: []string{"slot.revenue", "slot.gp"}, MissingPolicy: "skip"},
				"algo.net_contrib": {ID: "algo.net_contrib", Version: 1, Formula: "cm2 - overhead_alloc",
					DependsOnSlots: []string{"slot.platform_fee", "slot.ad_spend", "slot.affiliate"}, MissingPolicy: "skip"},
			}
			var out []precomp.AlgoDef
			for _, id := range ids {
				if d, ok := defs[id]; ok {
					out = append(out, d)
				}
			}
			return out, nil
		},
	}
}

func f(v float64) *float64 { return &v }

// 正常路径：槽达标 ⇒ gp 正确算出。
func TestBuildRow_GP(t *testing.T) {
	b := newBuilder([]precomp.SlotState{
		{ID: "slot.revenue", Status: "ACTIVE", Coverage: 0.99, Gate: 0.80},
		{ID: "slot.cogs", Status: "ACTIVE", Coverage: 0.95, Gate: 0.80},
	})
	def := precomp.BucketDef{ID: "pnl_month", ProducedBy: []string{"algo.gp"}}
	row, err := b.BuildRow(context.Background(), def, map[string]*float64{
		"rev": f(1000), "cogs": f(620),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Cells) != 1 || row.Cells[0].Value == nil {
		t.Fatalf("gp should be computed, got %+v", row.Cells)
	}
	if *row.Cells[0].Value != 380 {
		t.Fatalf("gp = %v, want 380", *row.Cells[0].Value)
	}
}

// 覆盖率不足 ⇒ 字段跳过（skipped_fields 记录），**不写 0**。
func TestBuildRow_SkipsWhenCoverageLow(t *testing.T) {
	b := newBuilder([]precomp.SlotState{
		{ID: "slot.revenue", Status: "ACTIVE", Coverage: 0.99, Gate: 0.80},
		{ID: "slot.cogs", Status: "ACTIVE", Coverage: 0.60, Gate: 0.80}, // 低于门限
	})
	def := precomp.BucketDef{ID: "pnl_month", ProducedBy: []string{"algo.gp"}}
	row, err := b.BuildRow(context.Background(), def, map[string]*float64{"rev": f(1000), "cogs": f(620)})
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Cells) != 1 || !row.Cells[0].Skipped {
		t.Fatalf("gp should be skipped when cogs coverage < gate, got %+v", row.Cells)
	}
	if row.Cells[0].Value != nil {
		t.Fatalf("skipped field must be nil (NULL), never 0")
	}
	if len(row.SkippedFields) != 1 || row.SkippedFields[0] != "gp" {
		t.Fatalf("skipped_fields should record gp, got %v", row.SkippedFields)
	}
}

// 槽 MISSING ⇒ 硬跳过（如 net_contrib 依赖 slot.affiliate）。
func TestBuildRow_NetContribBlockedByAffiliate(t *testing.T) {
	b := newBuilder([]precomp.SlotState{
		{ID: "slot.platform_fee", Status: "ACTIVE", Coverage: 0.95, Gate: 0.80},
		{ID: "slot.ad_spend", Status: "ACTIVE", Coverage: 0.95, Gate: 0.80},
		{ID: "slot.affiliate", Status: "MISSING", Coverage: 0.0, Gate: 0.80},
	})
	def := precomp.BucketDef{ID: "pnl_month", ProducedBy: []string{"algo.net_contrib"}}
	row, err := b.BuildRow(context.Background(), def, map[string]*float64{
		"cm2": f(80), "overhead_alloc": f(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !row.Cells[0].Skipped {
		t.Fatalf("net_contrib must skip when affiliate MISSING (expected behavior, docs)")
	}
}

// 毛利率分母 0 ⇒ 缺失（不是 0）。
func TestBuildRow_GMPZeroDenominator(t *testing.T) {
	b := newBuilder([]precomp.SlotState{
		{ID: "slot.revenue", Status: "ACTIVE", Coverage: 0.99, Gate: 0.80},
		{ID: "slot.gp", Status: "ACTIVE", Coverage: 0.99, Gate: 0.80},
	})
	def := precomp.BucketDef{ID: "pnl_month", ProducedBy: []string{"algo.gmp"}}
	row, err := b.BuildRow(context.Background(), def, map[string]*float64{"rev": f(0), "gp": f(30)})
	if err != nil {
		t.Fatal(err)
	}
	if row.Cells[0].Value != nil {
		t.Fatalf("gmp with rev=0 must be NULL not 0, got %v", *row.Cells[0].Value)
	}
}

// 版本漂移检测。
func TestDrift(t *testing.T) {
	def := precomp.BucketDef{
		AlgoVersions: map[string]int{"algo.gp": 3, "algo.gmp": 3},
	}
	// 一致
	if d := precomp.Drift(def, map[string]int{"algo.gp": 3, "algo.gmp": 3}, nil); len(d) != 0 {
		t.Fatalf("expected no drift, got %v", d)
	}
	// gp 升到 4 ⇒ 漂移
	d := precomp.Drift(def, map[string]int{"algo.gp": 4, "algo.gmp": 3}, nil)
	if len(d) != 1 {
		t.Fatalf("expected 1 drift, got %v", d)
	}
}

func TestFieldName(t *testing.T) {
	cases := map[string]string{
		"algo.gp": "gp", "algo.net_contrib": "net_contrib", "algo.cogs": "cogs",
	}
	for in, want := range cases {
		if got := precomp.FieldName(in); got != want {
			t.Fatalf("FieldName(%q)=%q want %q", in, got, want)
		}
	}
}
