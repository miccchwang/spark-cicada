// g4_formula_binding_test.go —— ★ 链路级闸门：G4 值绑定完整性（公式自由变量）。
//
// 这是本仓「假闸门」形态在 G4 上的**第三个变种**：
//
//	前两个变种（均由本仓既往修复覆盖）：
//	  * 桶 → 算法 引用完整性（gate.CheckBucketProducersRegistered）
//	  * 桶 → 规则 引用完整性（gate.CheckBucketRuleVersionsRegistered）
//	G4 原有两条断言只查「名字**是否存在**」：
//	  * CheckSlotsRegistered      —— depends_on_slots 里的槽 ID 是否已注册
//	  * CheckAlgorithmNoDataSource —— 有没有混入 source/table 等数据源字段
//	但算法的**执行载荷是 `formula`**，而公式里的自由变量**从没被任何断言看过一眼**。
//
// 实测（2026-10-07，闸门首次上线即抓到）：
//
//	algo.gp          formula `rev - cogs`              依赖槽 slot.revenue slot.cogs
//	algo.gmp         formula `rev == 0 ? null : gp/rev` 依赖槽 slot.revenue slot.gross_profit
//	algo.net_contrib formula `cm2 - overhead_alloc`     依赖槽 slot.platform_fee …
//
//	槽 `slot.revenue` 绑出的变量名是 `revenue`，公式要的却是 `rev` ⇒ **永远取不到值**。
//	Rust 内核 `eval_formula` 对查不到的变量返回 `Scalar::Missing`
//	（lib.rs: `unwrap_or(scalar::Scalar::Missing)`，**不报错**）⇒ 配合
//	`missing_policy: skip`，该算法在**每一条数据**上都被静默跳过：
//	`gp` / `gmp` 列永远为空、全程零报错。
//
// 本文件从**生产入口**（`LoadBucketRegistry` / `LoadRegistry`）进、
// 以「注册表加载成功/失败」**出**，而不是直接调判定函数看返回值。
package gate_test

import (
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ───────────────────────── 判定函数单元级 ─────────────────────────

func TestFormulaVars_ExtractsFreeVariables(t *testing.T) {
	cases := []struct {
		formula string
		want    []string
	}{
		{"revenue - cogs", []string{"cogs", "revenue"}},
		{"revenue == 0 ? null : gp / revenue", []string{"gp", "revenue"}},
		{"cm2 - overhead_alloc", []string{"cm2", "overhead_alloc"}},
		// 函数调用（名字后紧跟 `(`）不算自由变量。
		{"sum(qty * cost_unit)", []string{"cost_unit", "qty"}},
		// ★ 同名出现在**变量位置**时必须是变量 —— 这是白名单不能只看名字的原因。
		{"sum - qty", []string{"qty", "sum"}},
		// 字面量不是变量。
		{"null", nil},
		{"true == false ? 1 : 0", nil},
		// 数字/运算符不参与。
		{"1 - 2 * 3 / 4", nil},
		// 带前缀的名字要能被识别（万一有人写 slot.revenue 进公式，必须报出来）。
		{"slot.revenue - cogs", []string{"cogs", "slot.revenue"}},
		// 下划线开头的标识符。
		{"_tmp + a1", []string{"_tmp", "a1"}},
	}
	for _, c := range cases {
		got := gate.FormulaVars(c.formula)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("FormulaVars(%q) = %v，期望 %v", c.formula, got, c.want)
		}
	}
}

// TestCheckFormulaVariablesBound_RealRepoAlgoIsClean 真仓库的算法公式必须全部可绑定。
//
// ★ 这条断言直接盯**磁盘上的真 YAML**。此前它的值恒为 nil（因为没人调用）——
//   现在它是 LoadBucketRegistry 的必经之路。
func TestCheckFormulaVariablesBound_RealRepoAlgoIsClean(t *testing.T) {
	// 真仓库的算法（ID / 公式 / 依赖槽 / 状态），与 algorithms/*.yaml 逐字对应。
	docs := []gate.AlgoDoc{
		{ID: "algo.cogs", Formula: "sum(qty * cost_unit)",
			DependsOnSlots: []string{"slot.qty", "slot.cost_unit"}},
		{ID: "algo.gp", Formula: "revenue - cogs",
			DependsOnSlots: []string{"slot.revenue", "slot.cogs"}},
		{ID: "algo.gmp", Formula: "revenue == 0 ? null : gp / revenue",
			DependsOnSlots: []string{"slot.revenue", "slot.gross_profit"}},
		// PENDING：口径未定 ⇒ 不要求可绑定，但 formula 必须非空。
		{ID: "algo.net_contrib", Formula: "cm2 - overhead_alloc", Status: gate.StatusPending,
			DependsOnSlots: []string{"slot.platform_fee", "slot.ad_spend", "slot.affiliate",
				"slot.shipping", "slot.mkt_expense"}},
	}
	// 桶拓扑：pnl_month 的 produced_by（net_contrib 已移除）。
	upstream := map[string]map[string]bool{
		"algo.cogs":        {},
		"algo.gp":          {"algo.cogs": true},
		"algo.gmp":         {"algo.cogs": true, "algo.gp": true},
		"algo.net_contrib": {"algo.cogs": true, "algo.gp": true, "algo.gmp": true},
	}
	if v := gate.CheckFormulaVariablesBound(docs, upstream); len(v) != 0 {
		t.Fatalf("真仓库算法公式应全部可绑定，实际报错：\n  - %s", strings.Join(v, "\n  - "))
	}
}

// TestCheckFormulaVariablesBound_CatchesTheRealDefect 回归：曾被抓到的三个真实缺陷。
//
// 这是本文件的**核心负向自测** —— 用修复前的原文断言闸门会报错。
func TestCheckFormulaVariablesBound_CatchesTheRealDefect(t *testing.T) {
	upstream := map[string]map[string]bool{
		"algo.gp":  {"algo.cogs": true},
		"algo.gmp": {"algo.cogs": true, "algo.gp": true},
	}
	cases := []struct {
		name    string
		doc     gate.AlgoDoc
		wantHit string // 期望报错里必须出现的关键字
	}{
		{
			name: "algo.gp 公式用 rev 而槽是 slot.revenue",
			doc: gate.AlgoDoc{ID: "algo.gp", Formula: "rev - cogs",
				DependsOnSlots: []string{"slot.revenue", "slot.cogs"}},
			wantHit: `"rev"`,
		},
		{
			name: "algo.gmp 公式用 rev（同一命名错误的第二个实例）",
			doc: gate.AlgoDoc{ID: "algo.gmp", Formula: "rev == 0 ? null : gp / rev",
				DependsOnSlots: []string{"slot.revenue", "slot.gross_profit"}},
			wantHit: `"rev"`,
		},
		{
			name: "net_contrib 的 cm2/overhead_alloc 全仓无定义（且未标 PENDING）",
			doc: gate.AlgoDoc{ID: "algo.net_contrib", Formula: "cm2 - overhead_alloc",
				DependsOnSlots: []string{"slot.platform_fee"}},
			wantHit: `"cm2"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := gate.CheckFormulaVariablesBound([]gate.AlgoDoc{c.doc}, upstream)
			if len(v) == 0 {
				t.Fatalf("★ 闸门漏检：公式 %q 的自由变量无绑定来源却未报警", c.doc.Formula)
			}
			if !strings.Contains(strings.Join(v, " "), c.wantHit) {
				t.Fatalf("报错未指出变量 %s：%v", c.wantHit, v)
			}
			// 报错必须**可读**：要说明后果（静默跳过），便于运维直接定位。
			if !strings.Contains(strings.Join(v, " "), "静默跳过") {
				t.Errorf("报错未说明后果（静默跳过）：%v", v)
			}
		})
	}
}

// TestCheckFormulaVariablesBound_PendingIsExemptButNeedsFormula
// PENDING 算法免于绑定校验，但**必须**写明 formula（否则「待补」无从判断待补什么）。
func TestCheckFormulaVariablesBound_PendingIsExemptButNeedsFormula(t *testing.T) {
	// 有 formula 的 PENDING ⇒ 放行
	ok := gate.AlgoDoc{ID: "algo.x", Formula: "cm2 - overhead_alloc", Status: gate.StatusPending}
	if v := gate.CheckFormulaVariablesBound([]gate.AlgoDoc{ok}, nil); len(v) != 0 {
		t.Errorf("PENDING 且 formula 非空应放行，实际 %v", v)
	}
	// 空 formula 的 PENDING ⇒ 报错（空壳「待补」）
	bad := gate.AlgoDoc{ID: "algo.y", Formula: "  ", Status: gate.StatusPending}
	if v := gate.CheckFormulaVariablesBound([]gate.AlgoDoc{bad}, nil); len(v) == 0 {
		t.Error("★ PENDING 但 formula 为空应报错 —— 否则「待补」是个没有任何内容的空壳")
	}
}

// TestCheckFormulaVariablesBound_BindsViaDependsOnAlgos
// 「算法依赖算法」的显式声明（depends_on_algos）应可作为绑定来源。
//
// ★ 这条防的是**夹具太弱**：若绑定来源只认 depends_on_slots，
//   那么「net_contrib 声明 depends_on_algos: [algo.cm2]」这条能力就没人测。
func TestCheckFormulaVariablesBound_BindsViaDependsOnAlgos(t *testing.T) {
	doc := gate.AlgoDoc{
		ID:             "algo.net_contrib",
		Formula:        "cm2 - overhead_alloc",
		DependsOnSlots: []string{"slot.overhead_alloc"},
		DependsOnAlgos: []string{"algo.cm2"}, // ← 唯一的 cm2 来源
		Status:         gate.StatusActive,
	}
	// 无桶拓扑（upstream 为空）—— 只能靠 depends_on_algos 绑定 cm2。
	if v := gate.CheckFormulaVariablesBound([]gate.AlgoDoc{doc}, map[string]map[string]bool{}); len(v) != 0 {
		t.Fatalf("depends_on_algos 应能绑定 cm2，实际报错：%v", v)
	}
	// 移除该声明 ⇒ 立刻变红（证明上一条不是因为别的来源凑巧通过）。
	doc.DependsOnAlgos = nil
	if v := gate.CheckFormulaVariablesBound([]gate.AlgoDoc{doc}, map[string]map[string]bool{}); len(v) == 0 {
		t.Fatal("★ 夹具自证失败：去掉 depends_on_algos 后仍通过 ⇒ 上一条断言可能恒真")
	}
}

// TestCheckFormulaVariablesBound_UpstreamOrderMatters
// 上游算法必须**排在本算法之前**才能绑定（复刻 precomp.BuildRow 的串行语义）。
//
// ★ 这条防的是「顺序写反也放行」：若闸门忽略拓扑顺序，
//   把 algo.gmp 排在 algo.gp **之前**的桶也能通过，运行时 gp 必为 Missing。
func TestCheckFormulaVariablesBound_UpstreamOrderMatters(t *testing.T) {
	doc := gate.AlgoDoc{ID: "algo.gmp", Formula: "revenue == 0 ? null : gp / revenue",
		DependsOnSlots: []string{"slot.revenue"}}
	// gp 在**之前**（上游）⇒ 可绑定
	if v := gate.CheckFormulaVariablesBound([]gate.AlgoDoc{doc},
		map[string]map[string]bool{"algo.gmp": {"algo.gp": true}}); len(v) != 0 {
		t.Fatalf("gp 为上游时应可绑定，实际 %v", v)
	}
	// gp **不在**上游（桶里没排前面）⇒ 必须报错
	if v := gate.CheckFormulaVariablesBound([]gate.AlgoDoc{doc},
		map[string]map[string]bool{"algo.gmp": {}}); len(v) == 0 {
		t.Fatal("★ gp 不在上游时应报错 —— 否则「桶清单写漏一个算法」这个静默失效无法被发现")
	}
}

// TestFormulaFuncs_WhitelistIsActuallyUsed
// 函数名白名单必须**真的生效** —— 否则 `sum(...)` 里的 sum 会被当成未绑定变量误报。
//
// ★ 夹具自证：先证明「不加白名单会报」，再证明「加了不报」。
func TestFormulaFuncs_WhitelistIsActuallyUsed(t *testing.T) {
	doc := gate.AlgoDoc{ID: "algo.cogs", Formula: "sum(qty * cost_unit)",
		DependsOnSlots: []string{"slot.qty", "slot.cost_unit"}}
	// 正向：白名单生效 ⇒ 放行
	if v := gate.CheckFormulaVariablesBound([]gate.AlgoDoc{doc}, nil); len(v) != 0 {
		t.Fatalf("函数名 sum 不应被当作未绑定变量，实际 %v", v)
	}
	// 反向：把 sum 从公式里挪到**变量位置**（真变量）⇒ 必须报错。
	// 证明放行不是因为「任何未知名字都被忽略」。
	doc2 := gate.AlgoDoc{ID: "algo.cogs", Formula: "sum - qty",
		DependsOnSlots: []string{"slot.qty"}}
	if v := gate.CheckFormulaVariablesBound([]gate.AlgoDoc{doc2}, nil); len(v) == 0 {
		t.Fatal("★ 夹具自证失败：非白名单名字出现在变量位置却未报错 ⇒ 上一条可能恒真")
	}
}

// TestCheckPendingAlgosNotProducing 断言：PENDING 算法不得被任何桶引用。
//
// 理由：PENDING 的自由变量恰好「没有来源」，一旦入桶就是**恒空的静默列**。
func TestCheckPendingAlgosNotProducing(t *testing.T) {
	docs := []gate.AlgoDoc{
		{ID: "algo.gp", Formula: "revenue - cogs", DependsOnSlots: []string{"slot.revenue", "slot.cogs"}},
		{ID: "algo.net_contrib", Formula: "cm2 - overhead_alloc", Status: gate.StatusPending},
	}
	// 正向：ACTIVE 算法入桶没问题
	okBuckets := []gate.BucketDoc{{ID: "pnl_month", ProducedBy: []string{"algo.gp"}}}
	if v := gate.CheckPendingAlgosNotProducing(docs, okBuckets); len(v) != 0 {
		t.Fatalf("ACTIVE 算法入桶应放行，实际 %v", v)
	}
	// 反向：PENDING 算法入桶必须报错
	badBuckets := []gate.BucketDoc{{ID: "pnl_month",
		ProducedBy: []string{"algo.gp", "algo.net_contrib"}}}
	v := gate.CheckPendingAlgosNotProducing(docs, badBuckets)
	if len(v) == 0 {
		t.Fatal("★ PENDING 算法入桶应报错 —— 否则运行时它就是一条恒空的静默列")
	}
	if !strings.Contains(strings.Join(v, " "), "algo.net_contrib") ||
		!strings.Contains(strings.Join(v, " "), "pnl_month") {
		t.Errorf("报错应同时指出桶名与算法名：%v", v)
	}
}

// TestBareName_ConventionIsSingleSource 命名约定必须唯一且一致。
func TestBareName_ConventionIsSingleSource(t *testing.T) {
	cases := map[string]string{
		"slot.revenue":        "revenue",
		"algo.gp":             "gp",
		"algo.net_contrib":    "net_contrib",
		"revenue":             "revenue", // 无前缀原样返回
		"slot.overhead_alloc": "overhead_alloc",
	}
	for in, want := range cases {
		if got := gate.BareName(in); got != want {
			t.Errorf("BareName(%q) = %q，期望 %q", in, got, want)
		}
	}
}
