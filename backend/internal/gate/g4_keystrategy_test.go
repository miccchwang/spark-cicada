package gate_test

import (
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// G4 第七侧：槽的**匹配键策略**必须可解析。
//
// 病：`key_strategy` 被读进来、写进 DB，却没有任何判定消费（第十五个变种）。
// 本文件覆盖：① 声明层解析；② 键↔来源相称；③ 共用来源的键口径一致性；
// ④ 反向「不误伤」；⑤ 注入破坏必须被拦。
//
// ★ 命名同源：本文件断言用到的键策略名，必须与 gate 包权威表一致 ——
// 若权威表改名而本文件没改，TestKeyStrategy_AuthoritativeTable 会先失败。

// ── 1. 声明层：可解析 / 不可解析 ────────────────────────────────────────────

func TestKeyStrategy_Declared_AcceptsKnownStrategies(t *testing.T) {
	known := []string{
		"direct", "store_sku", "sku", "spu",
		"channel_day", "shop_txn", "date_pair", "barcode_then_sku",
	}
	docs := make([]gate.KeyStrategyDoc, 0, len(known))
	for _, k := range known {
		docs = append(docs, gate.KeyStrategyDoc{SlotID: "slot." + k, Raw: k})
	}
	if v := gate.CheckKeyStrategyDeclared(docs); len(v) != 0 {
		t.Fatalf("已知键策略不应报违规，得到：%v", v)
	}
	// 夹具自证：每一个 *声明的* 名字都必须真的被 ParseKeyStrategy 认出，
	// 否则上一条断言可能因「全被跳过」而恒真。
	for _, k := range known {
		if _, ok := gate.ParseKeyStrategy(k); !ok {
			t.Fatalf("夹具自证失败：%q 应是已知键策略", k)
		}
	}
}

func TestKeyStrategy_Declared_RejectsEmptyAndFreeText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 期望出现在报错里的片段
	}{
		{"空声明", "", "未声明"},
		{"纯空白", "   ", "未声明"},
		{"中文自由文本", "智能匹配", "不是已知匹配键策略"},
		{"英文自由文本", "auto", "不是已知匹配键策略"},
		{"近似但不合法", "store+sku+extra", "不是已知匹配键策略"},
		{"顺序错", "sku_store", "不是已知匹配键策略"},
		{"拼写错", "strore_sku", "不是已知匹配键策略"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := gate.CheckKeyStrategyDeclared([]gate.KeyStrategyDoc{
				{SlotID: "slot.x", Raw: tc.raw},
			})
			if len(v) != 1 {
				t.Fatalf("期望 1 条违规，得到 %d 条：%v", len(v), v)
			}
			if !strings.Contains(v[0], tc.want) {
				t.Fatalf("报错应含 %q（人类可读原因），实际：%q", tc.want, v[0])
			}
			if !strings.Contains(v[0], "slot.x") {
				t.Fatalf("报错应定位到槽，实际：%q", v[0])
			}
		})
	}
}

// ── 2. 分隔符归一（只归一分隔符，不做语义猜测）─────────────────────────────

func TestKeyStrategy_ParseNormalizesSeparatorsOnly(t *testing.T) {
	// 这些写法是同一个策略的不同「排版」，应归一为同一个规范名。
	same := []string{"store_sku", "store+sku", "store-sku", "store sku", "STORE_SKU"}
	for _, raw := range same {
		got, ok := gate.ParseKeyStrategy(raw)
		if !ok || got != "store_sku" {
			t.Fatalf("%q 应归一为 store_sku，得到 (%q,%v)", raw, got, ok)
		}
	}
	// ★ 但**不得**做语义猜测：顺序反了就是声明错，不该被静默纠正。
	if got, ok := gate.ParseKeyStrategy("sku_store"); ok {
		t.Fatalf("★ sku_store 与 store_sku 顺序不同 ⇒ 是不同声明，必须拒绝；却得到 (%q,true)", got)
	}
	// 文档写法 `barcode→sku` 归一为 barcode_then_sku（docs/03 §2.2 用箭头）。
	if got, ok := gate.ParseKeyStrategy("barcode→sku"); !ok || got != "barcode_then_sku" {
		t.Fatalf("barcode→sku 应归一为 barcode_then_sku，得到 (%q,%v)", got, ok)
	}
}

// ── 3. 键 ↔ 来源相称 ────────────────────────────────────────────────────────

func TestKeyStrategy_Declared_SourceKindCongruence(t *testing.T) {
	// 当前权威表里没有限定来源的策略 ⇒ 全部策略都不因来源种类报错。
	// 夹具自证：先确认「无限定」这一前提没被悄悄改成限定（否则本断言会恒真）。
	all := gate.KnownKeyStrategies()
	for _, k := range all {
		v := gate.CheckKeyStrategyDeclared([]gate.KeyStrategyDoc{
			{SlotID: "slot." + k, Raw: k, SourceKind: "api"},
		})
		if len(v) != 0 {
			t.Fatalf("策略 %s 在 api 来源下不应报不相称，得到：%v", k, v)
		}
	}
	// 未知来源种类本身不由本函数报错（那是 CheckSlotSourceResolvable 的职责，
	// 避免重复噪音）—— 用一条显式断言钉住这个分工。
	if v := gate.CheckKeyStrategyDeclared([]gate.KeyStrategyDoc{
		{SlotID: "slot.x", Raw: "store_sku", SourceKind: "想写什么写什么"},
	}); len(v) != 0 {
		t.Fatalf("来源种类合法性不由本函数判定（应交给 CheckSlotSourceResolvable），得到：%v", v)
	}
}

// ── 4. 共用来源的键口径一致性 ──────────────────────────────────────────────

func TestKeyStrategy_Cohesion_SameSourceMustShareKeys(t *testing.T) {
	// 三个槽共用 derived/channel_sales，前两个 store_sku、第三个 sku ⇒ 冲突。
	docs := []gate.KeyStrategyDoc{
		{SlotID: "slot.revenue", Raw: "store_sku", SourceKind: "derived", SourceRef: "channel_sales"},
		{SlotID: "slot.qty", Raw: "store_sku", SourceKind: "derived", SourceRef: "channel_sales"},
		{SlotID: "slot.return_qty", Raw: "sku", SourceKind: "derived", SourceRef: "channel_sales"},
	}
	v := gate.CheckKeyStrategyCohesion(docs)
	if len(v) != 1 {
		t.Fatalf("期望 1 条口径冲突，得到 %d 条：%v", len(v), v)
	}
	for _, want := range []string{"slot.return_qty", "channel_sales", "store_sku", "sku"} {
		if !strings.Contains(v[0], want) {
			t.Fatalf("报错应含 %q 以可定位，实际：%q", want, v[0])
		}
	}
}

func TestKeyStrategy_Cohesion_DirectCoexistsWithDetailKeys(t *testing.T) {
	// product_master 被 product_id/barcode/brand 三个主数据槽共用，全是 direct
	// ⇒ 一致；再混一个 store_sku 的明细槽 ⇒ 仍**允许**（direct 整行直达，
	// 与明细按维度对齐是两回事，不能算冲突）。
	docs := []gate.KeyStrategyDoc{
		{SlotID: "slot.product_id", Raw: "direct", SourceKind: "master_table", SourceRef: "product_master"},
		{SlotID: "slot.barcode", Raw: "direct", SourceKind: "master_table", SourceRef: "product_master"},
		{SlotID: "slot.brand", Raw: "direct", SourceKind: "master_table", SourceRef: "product_master"},
	}
	if v := gate.CheckKeyStrategyCohesion(docs); len(v) != 0 {
		t.Fatalf("三槽同为 direct 不应冲突，得到：%v", v)
	}
	mixed := append(docs, gate.KeyStrategyDoc{
		SlotID: "slot.product_detail", Raw: "store_sku",
		SourceKind: "master_table", SourceRef: "product_master",
	})
	if v := gate.CheckKeyStrategyCohesion(mixed); len(v) != 0 {
		t.Fatalf("direct 与明细键应允许共存，得到：%v", v)
	}
}

func TestKeyStrategy_Cohesion_DifferentSourcesDoNotConflict(t *testing.T) {
	// 两个槽键不同但**来源不同** ⇒ 不构成冲突（各自对齐各自来源）。
	docs := []gate.KeyStrategyDoc{
		{SlotID: "slot.stock", Raw: "sku", SourceKind: "master_table", SourceRef: "inventory_master"},
		{SlotID: "slot.shipping", Raw: "sku", SourceKind: "master_table", SourceRef: "logistics_master"},
		{SlotID: "slot.revenue", Raw: "store_sku", SourceKind: "derived", SourceRef: "channel_sales"},
	}
	if v := gate.CheckKeyStrategyCohesion(docs); len(v) != 0 {
		t.Fatalf("来源不同不应报冲突，得到：%v", v)
	}
}

func TestKeyStrategy_Cohesion_SkipsInvalidDeclarations(t *testing.T) {
	// 非法声明由 CheckKeyStrategyDeclared 报；一致性检查应跳过它们
	// —— 否则同一条问题会被报两次（重复噪音会让人忽略真问题）。
	docs := []gate.KeyStrategyDoc{
		{SlotID: "slot.a", Raw: "自动", SourceKind: "derived", SourceRef: "channel_sales"},
		{SlotID: "slot.b", Raw: "store_sku", SourceKind: "derived", SourceRef: "channel_sales"},
	}
	if v := gate.CheckKeyStrategyCohesion(docs); len(v) != 0 {
		t.Fatalf("非法声明应交由声明层报，一致性层应跳过，得到：%v", v)
	}
}

func TestKeyStrategy_Cohesion_IgnoresEmptySourceRef(t *testing.T) {
	// 来源引用为空 ⇒ 对齐语义无从谈起（例如 derived 槽 ref 可空），
	// 不应把两个「无来源」的槽硬凑成一组来报冲突。
	docs := []gate.KeyStrategyDoc{
		{SlotID: "slot.a", Raw: "store_sku", SourceKind: "derived"},
		{SlotID: "slot.b", Raw: "sku", SourceKind: "derived"},
	}
	if v := gate.CheckKeyStrategyCohesion(docs); len(v) != 0 {
		t.Fatalf("无来源引用不应分组比较，得到：%v", v)
	}
}

// ── 5. 权威表与文档/实现同源 ─────────────────────────────────────────────────

func TestKeyStrategy_AuthoritativeTable(t *testing.T) {
	// 本文件断言用到的名字必须都在权威表里 —— 防止权威表改动后本文件静默失效。
	for _, k := range []string{
		"direct", "store_sku", "sku", "spu",
		"channel_day", "shop_txn", "date_pair", "barcode_then_sku",
	} {
		if !gate.KeyStrategies()[k] {
			t.Fatalf("权威表缺少 %q（若有意改名，请同步更新本测试）", k)
		}
	}
	// 键序列必须非空（否则「键口径一致」退化为「名字一致」）。
	for k := range gate.KeyStrategies() {
		keys, ok := gate.KeyStrategyKeys(k)
		if !ok || len(keys) == 0 {
			t.Fatalf("键策略 %q 的规范键序列为空 ⇒ 对齐判据不可用", k)
		}
	}
}

// ── 6. 注入破坏（必须被准确拦下）─────────────────────────────────────────

// TestKeyStrategy_Injection_MissingDeclaration 注入：删掉某槽的 key_strategy。
func TestKeyStrategy_Injection_MissingDeclaration(t *testing.T) {
	base := []gate.KeyStrategyDoc{
		{SlotID: "slot.a", Raw: "store_sku", SourceKind: "derived", SourceRef: "channel_sales"},
		{SlotID: "slot.b", Raw: "store_sku", SourceKind: "derived", SourceRef: "channel_sales"},
	}
	if v := gate.CheckKeyStrategyDeclared(base); len(v) != 0 {
		t.Fatalf("基线应无违规，得到：%v", v)
	}
	broken := append([]gate.KeyStrategyDoc{}, base...)
	broken[1].Raw = ""
	if v := gate.CheckKeyStrategyDeclared(broken); len(v) != 1 {
		t.Fatalf("★ 注入「删掉 key_strategy」未被拦下，得到：%v", v)
	}
}

// TestKeyStrategy_Injection_CohesionBypass 注入：让一致性检查恒放行。
func TestKeyStrategy_Injection_CohesionBypass(t *testing.T) {
	// 若一致性检查恒返 nil，则下面这个真冲突不会被发现。
	conflict := []gate.KeyStrategyDoc{
		{SlotID: "a", Raw: "store_sku", SourceKind: "derived", SourceRef: "channel_sales"},
		{SlotID: "b", Raw: "sku", SourceKind: "derived", SourceRef: "channel_sales"},
	}
	if v := gate.CheckKeyStrategyCohesion(conflict); len(v) == 0 {
		t.Fatal("★ 真冲突未被拦下 ⇒ 一致性检查恒真")
	}
}
