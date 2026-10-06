package pnl

import (
	"errors"
	"strings"
	"testing"
)

func f(v float64) *float64 { return &v }

// ───────────────────────────── 口径解析 ─────────────────────────────

func TestParseCaliber_Valid(t *testing.T) {
	for _, s := range []string{"A", "B", " A ", "B\n"} {
		c, err := ParseCaliber(s)
		if err != nil {
			t.Fatalf("ParseCaliber(%q) 意外失败: %v", s, err)
		}
		if !IsValidCaliber(c) {
			t.Fatalf("ParseCaliber(%q) = %q 不合法", s, c)
		}
	}
}

func TestParseCaliber_InvalidReturnsErrorNotSilentFallback(t *testing.T) {
	// ★ 非法值必须报错：静默回退会让财务拿着 A 的数字以为在看 B。
	for _, s := range []string{"", "C", "a", "b", "AB", "1", "口径A"} {
		_, err := ParseCaliber(s)
		if err == nil {
			t.Fatalf("ParseCaliber(%q) 应报错，却通过了（静默回退是危险的）", s)
		}
		if !errors.Is(err, ErrInvalidCaliber) {
			t.Fatalf("ParseCaliber(%q) 错误类型不对: %v", s, err)
		}
	}
}

func TestResolve_RequestedWins(t *testing.T) {
	r := Resolve("A", ModulePnL)
	if r.Caliber != CaliberA || r.Source != "requested" || r.Downgraded {
		t.Fatalf("请求值合法时不应降级: %+v", r)
	}
}

func TestResolve_ModuleDefault_D10(t *testing.T) {
	// ★ D10「分模块各自默认」：经营报表=运营口径 A，P&L=财务口径 B。
	rep := Resolve("", ModuleReport)
	if rep.Caliber != CaliberA {
		t.Fatalf("经营报表默认口径应为 A，得 %s", rep.Caliber)
	}
	if rep.Source != "module_default" {
		t.Fatalf("来源应为 module_default，得 %s", rep.Source)
	}
	if rep.Downgraded {
		t.Fatalf("「未指定」是正常取默认，不应标记为降级: %+v", rep)
	}
	if !strings.Contains(rep.Reason, "分模块各自默认") {
		t.Fatalf("原因应说明 D10 依据，得 %q", rep.Reason)
	}

	p := Resolve("", ModulePnL)
	if p.Caliber != CaliberB {
		t.Fatalf("P&L 默认口径应为 B，得 %s", p.Caliber)
	}
}

func TestResolve_DirtyValueFallsBackAndIsMarked(t *testing.T) {
	r := Resolve("garbage", ModulePnL)
	if r.Caliber != CaliberB {
		t.Fatalf("脏值应回退到模块默认 B，得 %s", r.Caliber)
	}
	if !r.Downgraded {
		t.Fatal("脏值必须标记 Downgraded，否则无法告知用户")
	}
	if !strings.Contains(r.Reason, "非法") {
		t.Fatalf("原因应说明「非法」，得 %q", r.Reason)
	}
}

func TestResolve_UnknownModuleUsesConservativeFallback(t *testing.T) {
	r := Resolve("", "module.unknown")
	if r.Caliber != FallbackCaliber {
		t.Fatalf("未登记模块应兜底 %s，得 %s", FallbackCaliber, r.Caliber)
	}
	if r.Source != "fallback" || !r.Downgraded {
		t.Fatalf("兜底必须标记来源与降级: %+v", r)
	}
}

// ───────────────────────────── 折扣归属 ─────────────────────────────

func TestDiscountTarget_IsTheOnlyStructuralDifference(t *testing.T) {
	if DiscountTarget(CaliberA) != LineMarketing {
		t.Fatalf("口径 A 折扣应计入营销费用，得 %s", DiscountTarget(CaliberA))
	}
	if DiscountTarget(CaliberB) != LineSellerDiscount {
		t.Fatalf("口径 B 折扣应作收入抵减，得 %s", DiscountTarget(CaliberB))
	}
	if DiscountTarget(CaliberA) == DiscountTarget(CaliberB) {
		t.Fatal("★ 两口径的折扣归属必须不同，否则口径切换没有任何意义")
	}
}

func TestSellerDiscountRole_MatchesFrontendContract(t *testing.T) {
	// 与 contracts/pnl.ts 的 sellerDiscountRole 字面量对齐
	if SellerDiscountRole(CaliberA) != "marketing_expense" {
		t.Fatalf("口径 A 角色应为 marketing_expense，得 %s", SellerDiscountRole(CaliberA))
	}
	if SellerDiscountRole(CaliberB) != "contra_revenue" {
		t.Fatalf("口径 B 角色应为 contra_revenue，得 %s", SellerDiscountRole(CaliberB))
	}
}

func TestMisreadingRisk_BothNonEmptyAndDistinct(t *testing.T) {
	a := MisreadingRisk(CaliberA)
	b := MisreadingRisk(CaliberB)
	if a == "" || b == "" {
		t.Fatal("两口径都必须给出误读风险提示（KODP 教训）")
	}
	if a == b {
		t.Fatal("两口径的误读风险不应相同")
	}
	if !strings.Contains(a, "费用失控") {
		t.Fatalf("口径 A 的误读风险应提到「费用失控」，得 %q", a)
	}
}

// ───────────────────────────── ★ 不变量 ─────────────────────────────

func TestInvariant_NetRevenueEqualPasses(t *testing.T) {
	a := map[string]*float64{"gross_listing": f(1_000_000), "net_revenue": f(920_000)}
	b := map[string]*float64{"gross_listing": f(1_000_000), "net_revenue": f(920_000)}
	if v := CheckNetRevenueInvariant(a, b); len(v) != 0 {
		t.Fatalf("★ 净收入相同却报违规: %+v", v)
	}
}

func TestInvariant_NetRevenueDifferentIsViolation(t *testing.T) {
	// 模拟「有人在 B 分支里又扣了一次折扣」——必须被拦下。
	a := map[string]*float64{"gross_listing": f(1_000_000), "net_revenue": f(920_000)}
	b := map[string]*float64{"gross_listing": f(1_000_000), "net_revenue": f(860_000)}
	v := CheckNetRevenueInvariant(a, b)
	if len(v) != 1 {
		t.Fatalf("净收入不同应产生恰好 1 条违规，得 %d 条: %+v", len(v), v)
	}
	if v[0].Line != "net_revenue" {
		t.Fatalf("违规行应为 net_revenue，得 %s", v[0].Line)
	}
	if !strings.Contains(v[0].Note, "公式被改坏") {
		t.Fatalf("提示应点明「是 bug 不是新口径」，得 %q", v[0].Note)
	}
}

func TestInvariant_BothMissingIsConsistent(t *testing.T) {
	a := map[string]*float64{"gross_listing": f(1), "net_revenue": nil}
	b := map[string]*float64{"gross_listing": f(1), "net_revenue": nil}
	if v := CheckNetRevenueInvariant(a, b); len(v) != 0 {
		t.Fatalf("两边都缺失应视为一致（缺的是同一件事）: %+v", v)
	}
}

func TestInvariant_OneMissingOnePresentIsViolation(t *testing.T) {
	a := map[string]*float64{"gross_listing": f(1), "net_revenue": nil}
	b := map[string]*float64{"gross_listing": f(1), "net_revenue": f(100)}
	if len(CheckNetRevenueInvariant(a, b)) != 1 {
		t.Fatal("一边缺失一边有值必须判违规")
	}
}

func TestInvariant_GrossListingAlsoInvariant(t *testing.T) {
	a := map[string]*float64{"gross_listing": f(100), "net_revenue": f(90)}
	b := map[string]*float64{"gross_listing": f(101), "net_revenue": f(90)}
	v := CheckNetRevenueInvariant(a, b)
	if len(v) != 1 || v[0].Line != "gross_listing" {
		t.Fatalf("挂牌总额不同也必须被拦下: %+v", v)
	}
}

func TestInvariant_NilMapsAreSafe(t *testing.T) {
	// 两边都没数据 ⇒ 无违规（而不是 panic）
	if v := CheckNetRevenueInvariant(nil, nil); len(v) != 0 {
		t.Fatalf("nil map 不应产生违规，得 %+v", v)
	}
}

func TestInvariant_FloatNoiseWithinEpsilon(t *testing.T) {
	// 浮点噪声（1e-9 量级）不应被误判为违规
	a := map[string]*float64{"net_revenue": f(920_000.0)}
	b := map[string]*float64{"net_revenue": f(920_000.0 + 1e-9)}
	if len(CheckNetRevenueInvariant(a, b)) != 0 {
		t.Fatal("浮点噪声不应判违规")
	}
	// 但真实差异（1 泰铢）必须判违规 —— 不能把 epsilon 放太宽
	c := map[string]*float64{"net_revenue": f(920_001.0)}
	if len(CheckNetRevenueInvariant(a, c)) != 1 {
		t.Fatal("★ 1 THB 的真实差异必须判违规（epsilon 过宽会掩盖真实的公式回归）")
	}
}

// ───────────────────────────── 审计记录 ─────────────────────────────

func TestCaliberChange_Validate(t *testing.T) {
	ok := CaliberChange{Account: "u.a", Module: ModulePnL, From: CaliberA, To: CaliberB}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法记录不应报错: %v", err)
	}

	if err := (CaliberChange{To: CaliberB}).Validate(); err == nil {
		t.Fatal("缺 Module 应报错（否则无法解释默认口径来源）")
	}
	if err := (CaliberChange{Module: ModulePnL, To: "C"}).Validate(); err == nil {
		t.Fatal("非法 To 应报错")
	}
	if err := (CaliberChange{Module: ModulePnL, From: "X", To: CaliberB}).Validate(); err == nil {
		t.Fatal("非法 From 应报错")
	}
}

func TestCaliberChange_SummaryExplainsFallback(t *testing.T) {
	c := CaliberChange{
		Account: "u.fin", Module: ModulePnL, From: CaliberA, To: CaliberB,
		Requested: "garbage", Source: "module_default", Dirty: true,
	}
	s := c.ChangeSummary()
	if !strings.Contains(s, "A → B") {
		t.Fatalf("摘要应含切换方向，得 %q", s)
	}
	if !strings.Contains(s, ModulePnL) {
		t.Fatalf("摘要应含模块，得 %q", s)
	}
	if !strings.Contains(s, "非法") {
		t.Fatalf("★ 脏值回退必须在摘要里说明（否则后人无法解释为何用了 B），得 %q", s)
	}
}

func TestCaliberChange_SummaryNoPrevious(t *testing.T) {
	c := CaliberChange{Module: ModulePnL, To: CaliberB}
	if !strings.Contains(c.ChangeSummary(), "(未设置)") {
		t.Fatalf("首次设置应显示 (未设置)，得 %q", c.ChangeSummary())
	}
}

// ───────────────────────────── 分层顺序 ─────────────────────────────

func TestLineIDs_OrderIsContract(t *testing.T) {
	want := []string{
		"gross_listing", "seller_discount", "platform_discount", "net_revenue",
		"cogs", "gp", "gmp", "marketing", "cm1", "platform_fee", "cm2",
		"overhead_alloc", "net_contrib",
	}
	if len(LineIDs) != len(want) {
		t.Fatalf("分层数量不符：得 %d，期望 %d", len(LineIDs), len(want))
	}
	for i := range want {
		if LineIDs[i] != want[i] {
			t.Fatalf("第 %d 行应为 %s，得 %s（行序是契约，财务有肌肉记忆）", i, want[i], LineIDs[i])
		}
	}
}

func TestSectionOf(t *testing.T) {
	cases := map[string]string{
		"gross_listing":   "revenue",
		"net_revenue":     "revenue",
		"seller_discount": "revenue",
		"gmp":             "gross",
		"gp":              "gross",
		"marketing":       "contribution",
		"net_contrib":     "contribution",
	}
	for line, want := range cases {
		if got := SectionOf(line); got != want {
			t.Fatalf("SectionOf(%s) = %s，期望 %s", line, got, want)
		}
	}
}

// ───────────────────────────── 元数据 ─────────────────────────────

func TestMetaFor_AllFieldsPresent(t *testing.T) {
	for _, c := range Calibers {
		m := MetaFor(c)
		if m.ID != c {
			t.Fatalf("MetaFor(%s).ID = %s", c, m.ID)
		}
		if m.Short == "" || m.Label == "" || m.Definition == "" || m.MisreadingRisk == "" || m.SellerDiscountRole == "" {
			t.Fatalf("MetaFor(%s) 字段不完整: %+v", c, m)
		}
	}
	// 与前端契约 contracts/pnl.ts 的 CALIBER_META 字面量对齐
	if MetaFor(CaliberA).Definition != "Seller Discount 计入营销费用" {
		t.Fatalf("口径 A 定义与契约不一致: %q", MetaFor(CaliberA).Definition)
	}
	if !strings.Contains(MetaFor(CaliberB).Definition, "contra-revenue") {
		t.Fatalf("口径 B 定义应含 contra-revenue: %q", MetaFor(CaliberB).Definition)
	}
}

func TestAllMeta_Ordered(t *testing.T) {
	all := AllMeta()
	if len(all) != 2 || all[0].ID != CaliberA || all[1].ID != CaliberB {
		t.Fatalf("AllMeta 应有序 A,B: %+v", all)
	}
}
