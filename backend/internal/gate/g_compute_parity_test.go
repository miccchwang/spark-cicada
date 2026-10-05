// g_compute_parity_test.go —— 计算内核「语义对等」测试。
//
// 目的：Rust 内核（compute/）在缺工具链的环境（如本机）无法编译时，
// 仍能用 Go 侧**等价实现**验证同一组语义断言，确保：
//   1. 关键纪律（Missing≠0、分母 0→缺失、覆盖率门控、银行家舍入）在任何语言下都成立；
//   2. Rust 侧 --selftest 的用例与 Go 侧镜像**一一对应**，将来 Rust 编译通过后可直接对拍。
//
// 注意：本文件是**独立的参考实现**，复刻 compute/src 的语义，不依赖 Rust 构建。
// 它不替代 Rust 自检（那是权威口径），而是让 CI 在没有 Rust 时也能守住红线。
package gate_test

import (
	"math"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ── 参考实现：与 compute/src/scalar.rs、formula.rs 语义一致 ──

type sc struct {
	missing bool
	v       float64
}

func num(v float64) sc    { return sc{v: v} }
func missing() sc         { return sc{missing: true} }
func (s sc) get() (float64, bool) { return s.v, !s.missing }

// map2：任一缺失 ⇒ 缺失（fail-closed 传播）
func map2(a, b sc, f func(x, y float64) float64) sc {
	if a.missing || b.missing {
		return missing()
	}
	return num(f(a.v, b.v))
}

// safeDiv：任一缺失或分母 0 ⇒ 缺失（不返回 0）
func safeDiv(a, b sc) sc {
	if a.missing || b.missing || b.v == 0 {
		return missing()
	}
	return num(a.v / b.v)
}

// bankerRound：银行家舍入（四舍六入五成双）
func bankerRound(v float64, decimals int) float64 {
	m := math.Pow(10, float64(decimals))
	x := v * m
	diff := math.Abs(x - math.Trunc(x))
	if math.Abs(diff-0.5) < 1e-12 {
		tr := math.Trunc(x)
		if math.Mod(tr, 2) == 0 {
			return tr / m
		}
		return (tr + math.Copysign(1, x)) / m
	}
	return math.Round(x) / m
}

// ── 对等断言 ──

// compute/src/main.rs selftest #1/#2：GP 正常 / 依赖缺失 ⇒ 缺失
func TestComputeParity_GP(t *testing.T) {
	gp := map2(num(100), num(40), func(a, b float64) float64 { return a - b })
	if v, ok := gp.get(); !ok || v != 60 {
		t.Fatalf("gp 应 = 60，实际 %+v", gp)
	}
	// cogs 缺失 ⇒ gp 缺失（不补 0）
	gpMissing := map2(num(100), missing(), func(a, b float64) float64 { return a - b })
	if !gpMissing.missing {
		t.Fatal("cogs 缺失时 gp 必须缺失（不得补 0）")
	}
}

// compute/src/main.rs selftest #3/#4：GMP 分母 0 ⇒ 缺失；正常 ⇒ 0.25
func TestComputeParity_GMP(t *testing.T) {
	if r := safeDiv(num(30), num(0)); !r.missing {
		t.Fatal("gmp(rev=0) 必须缺失（不是 0）")
	}
	if r := safeDiv(num(50), num(200)); r.missing || r.v != 0.25 {
		t.Fatalf("gmp 应 = 0.25，实际 %+v", r)
	}
}

// compute/src/gate.rs：覆盖率门控（MISSING 硬淘汰 / 0.79<0.80 淘汰 / 0.80==0.80 通过）
func TestComputeParity_CoverageGate(t *testing.T) {
	// MISSING 硬淘汰
	if v := gate.DecideCoverage([]gate.CoverageCase{
		{SlotID: "s", Coverage: 0.99, Gate: 0.80, Status: "MISSING"},
	}); !v.Skip {
		t.Fatal("MISSING 槽必须硬淘汰")
	}
	// 0.79 < 0.80 淘汰
	if v := gate.DecideCoverage([]gate.CoverageCase{
		{SlotID: "s", Coverage: 0.79, Gate: 0.80, Status: "ACTIVE"},
	}); !v.Skip {
		t.Fatal("覆盖率 0.79 < 0.80 必须淘汰")
	}
	// 0.80 == 0.80 通过
	if v := gate.DecideCoverage([]gate.CoverageCase{
		{SlotID: "s", Coverage: 0.80, Gate: 0.80, Status: "ACTIVE"},
	}); v.Skip {
		t.Fatal("覆盖率 0.80 == 0.80 必须通过")
	}
}

// compute/src/scalar.rs：Missing != Num(0.0) 且银行家舍入
func TestComputeParity_MissingAndRounding(t *testing.T) {
	if missing() == num(0) {
		t.Fatal("Missing 与 Num(0) 必须可区分")
	}
	// 银行家舍入
	if got := bankerRound(2.5, 0); got != 2 {
		t.Fatalf("bankerRound(2.5)=%v want 2", got)
	}
	if got := bankerRound(3.5, 0); got != 4 {
		t.Fatalf("bankerRound(3.5)=%v want 4", got)
	}
}

// compute/src/gate.rs：net_contrib 被 MISSING 的 affiliate 阻断
func TestComputeParity_AffiliateBlocksNetContrib(t *testing.T) {
	// net_contrib 依赖 ["platform_fee","ad_spend","affiliate"]，affiliate MISSING
	if v := gate.DecideCoverage([]gate.CoverageCase{
		{SlotID: "slot.platform_fee", Coverage: 0.95, Gate: 0.80, Status: "ACTIVE"},
		{SlotID: "slot.ad_spend", Coverage: 0.95, Gate: 0.80, Status: "ACTIVE"},
		{SlotID: "slot.affiliate", Coverage: 0.0, Gate: 0.80, Status: "MISSING"},
	}); !v.Skip {
		t.Fatal("affiliate MISSING 必须阻断 net_contrib")
	}
}
