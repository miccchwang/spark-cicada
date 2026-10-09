// g4_effectivity_test.go —— G4 第十五侧（规则项生效期 + 契约外键）的**判定函数**自测。
//
// 分层纪律（与其余各侧一致）：本文件只测判定函数本身；
// 「生产路径是否真的调用了它」由 `internal/rule/rule_test.go` 的
// `TestWiring_RuleGatesHaveProductionCallSite`（静态源码扫描）负责 —— 两者缺一不可。
//
// ★ 本文件的判别性设计（证明断言不是装饰，而非「看起来对」）：
//   - 契约外的键**必须被报出**（否则 `effective_to` 会被 yaml 静默丢弃而不自知）；
//   - 契约内的键**不得被报出**（否则白名单过窄 ⇒ 真实数据被误杀 ⇒ 人们会去关掉闸门）；
//   - 把 `[from, to)` 的任一端点判反 ⇒ 必须变红（证明语义自证真的在跑）；
//   - `Raw` 缺失 ⇒ 必须 fail-closed（「读不到」不得当成「没问题」）。
package gate_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// docWithItems 造一个带原始键的 RuleDoc（Raw 是「未知键探测」的唯一输入）。
//
// ★ 结构体侧（Items）由原始键**派生**：真实链路里两者同源（LoadRuleRegistry
// 先 yaml.Unmarshal 成结构体、再单独解一遍原始键）。测试夹具若让两者不一致，
// 就会造出真实链路里不可能出现的状态，从而测不出真问题。
func docWithItems(id string, items []map[string]any) gate.RuleDoc {
	list := make([]any, 0, len(items))
	typed := make([]gate.RuleItem, 0, len(items))
	for _, m := range items {
		list = append(list, m)
		it := gate.RuleItem{}
		if v, ok := m["id"]; ok {
			it.ID = fmt.Sprintf("%v", v)
		}
		if v, ok := m["effective_from"]; ok {
			it.EffectiveFrom = fmt.Sprintf("%v", v)
		}
		if v, ok := m["effective_to"]; ok {
			it.EffectiveTo = fmt.Sprintf("%v", v)
		}
		typed = append(typed, it)
	}
	return gate.RuleDoc{
		ID:    id,
		Raw:   map[string]any{"id": id, "items": list},
		Items: typed,
	}
}

// ───────────────────── 1. 契约外的键必须被报出（核心） ─────────────────────

// TestG4Effectivity_UnknownKeyIsReported ★ 本侧的核心断言。
//
// 场景就是真实事故形态：作者按 docs/02 的写法加 `effective_to`（在修复前
// Go 侧没有该字段），yaml.v3 静默丢弃它、费率永不到期、全程零报错。
func TestG4Effectivity_UnknownKeyIsReported(t *testing.T) {
	// 修复前 `effective_to` 也是「契约外」—— 这里用另一个真实拼写错误形态，
	// 保证断言与「当前白名单长什么样」无关。
	doc := docWithItems("rule.tk.fee", []map[string]any{
		{"id": "platform_commission", "rate": 0.10, "effective_form": "2026-07-01"},
	})
	got := gate.CheckRuleItemUnknownKeys(doc)
	if len(got) != 1 {
		t.Fatalf("应报出 1 条契约外键，得到 %d 条：%v", len(got), got)
	}
	if !strings.Contains(got[0], "effective_form") {
		t.Fatalf("报错必须点名出问题的键 %q，得到：%s", "effective_form", got[0])
	}
	if !strings.Contains(got[0], "platform_commission") {
		t.Fatalf("报错必须点名出问题的项，得到：%s", got[0])
	}
}

// TestG4Effectivity_KnownKeysAreNotReported 契约内的键一个都不许被误报。
//
// ★ 白名单过窄比过宽更危险：真实数据被误杀时，人的第一反应是**关掉闸门**。
func TestG4Effectivity_KnownKeysAreNotReported(t *testing.T) {
	item := map[string]any{}
	for _, k := range gate.RuleItemKnownKeys() {
		item[k] = "v"
	}
	doc := docWithItems("rule.tk.fee", []map[string]any{item})
	if got := gate.CheckRuleItemUnknownKeys(doc); len(got) != 0 {
		t.Fatalf("契约内的键不应被报出，得到：%v", got)
	}
}

// TestG4Effectivity_KnownKeysMatchContract 白名单必须含契约里声明的键。
//
// `effective_to` 来自 `contracts/compute.proto: RuleItem`（field 7）；
// 缺了它，本闸门就会把「合法的到期声明」当成契约外键 —— 两头都是病。
func TestG4Effectivity_KnownKeysMatchContract(t *testing.T) {
	want := map[string]bool{
		"id": true, "name": true, "rate": true, "flat_per_order": true,
		"vat_included": true, "effective_from": true, "effective_to": true,
	}
	have := map[string]bool{}
	for _, k := range gate.RuleItemKnownKeys() {
		have[k] = true
	}
	for k := range want {
		if !have[k] {
			t.Errorf("白名单缺契约字段 %q（proto: RuleItem）—— 该键会被静默丢弃", k)
		}
	}
}

// TestG4Effectivity_RawMissingFailsClosed 读不到原始键 ⇒ 必须报违规。
//
// 「读不到」不得当成「没有契约外的键」—— 那正是「空集合让校验恒真」。
func TestG4Effectivity_RawMissingFailsClosed(t *testing.T) {
	doc := gate.RuleDoc{ID: "rule.x", Items: []gate.RuleItem{{ID: "a"}}}
	if got := gate.CheckRuleItemUnknownKeys(doc); len(got) != 1 {
		t.Fatalf("Raw 为空时必须 fail-closed 报 1 条，得到 %v", got)
	}
	// Raw 存在但 items 不是映射列表（形态异常）⇒ 同样 fail-closed。
	bad := gate.RuleDoc{ID: "rule.x", Raw: map[string]any{"items": "not-a-list"},
		Items: []gate.RuleItem{{ID: "a"}}}
	if got := gate.CheckRuleItemUnknownKeys(bad); len(got) != 1 {
		t.Fatalf("items 形态异常时必须 fail-closed 报 1 条，得到 %v", got)
	}
	// Raw 存在、无 items 键、结构体也没有 items ⇒ 合法（纯门限规则），不报。
	ok := gate.RuleDoc{ID: "rule.x", Raw: map[string]any{"id": "rule.x"}}
	if got := gate.CheckRuleItemUnknownKeys(ok); len(got) != 0 {
		t.Fatalf("无 items 的规则不应被报，得到 %v", got)
	}
	// Raw 存在、无 items 键，但结构体有 items ⇒ 两侧对不上，fail-closed。
	mismatch := gate.RuleDoc{ID: "rule.x", Raw: map[string]any{"id": "rule.x"},
		Items: []gate.RuleItem{{ID: "a"}}}
	if got := gate.CheckRuleItemUnknownKeys(mismatch); len(got) != 1 {
		t.Fatalf("Raw 与结构体的 items 不一致时必须 fail-closed，得到 %v", got)
	}
}

// ───────────────────── 2. 生效期：形态、次序、语义 ─────────────────────

func TestG4Effectivity_WindowFormatIsRejected(t *testing.T) {
	cases := []struct{ name, from, to, wantSub string }{
		{"from 非零填充", "2026-7-1", "", "effective_from"},
		{"to 非零填充", "", "2026-7-1", "effective_to"},
		{"from 非日期", "去年七月", "", "effective_from"},
		{"to 非日期", "", "2026/07/01", "effective_to"},
	}
	for _, c := range cases {
		doc := gate.RuleDoc{ID: "rule.x", Items: []gate.RuleItem{
			{ID: "a", EffectiveFrom: c.from, EffectiveTo: c.to},
		}}
		got := gate.CheckRuleItemWindowFields(doc)
		if len(got) == 0 {
			t.Errorf("%s：应被报出，实际全绿", c.name)
			continue
		}
		if !strings.Contains(strings.Join(got, "|"), c.wantSub) {
			t.Errorf("%s：报错应点名 %q，得到 %v", c.name, c.wantSub, got)
		}
	}
}

// TestG4Effectivity_EmptyWindowIsRejected 空窗 ⇒ 该费率在任何日期都不生效。
func TestG4Effectivity_EmptyWindowIsRejected(t *testing.T) {
	for _, tc := range []struct{ from, to string }{
		{"2026-06-01", "2026-06-01"}, // from == to ⇒ 空
		{"2026-06-01", "2026-05-01"}, // from > to  ⇒ 空
	} {
		doc := gate.RuleDoc{ID: "rule.x", Items: []gate.RuleItem{
			{ID: "a", EffectiveFrom: tc.from, EffectiveTo: tc.to},
		}}
		got := gate.CheckRuleItemWindowFields(doc)
		if len(got) != 1 || !strings.Contains(got[0], "空窗") {
			t.Errorf("from=%s to=%s 应报「空窗」，得到 %v", tc.from, tc.to, got)
		}
	}
}

// TestG4Effectivity_ValidWindowPasses 合法窗口必须全绿（否则真实数据会被误杀）。
func TestG4Effectivity_ValidWindowPasses(t *testing.T) {
	doc := gate.RuleDoc{ID: "rule.x", Items: []gate.RuleItem{
		{ID: "a", EffectiveFrom: "2026-07-01", EffectiveTo: "2026-12-31"},
		{ID: "b", EffectiveFrom: "2026-05-01"}, // 无上界
		{ID: "c", EffectiveTo: "2027-01-01"},   // 无下界
		{ID: "d"},                              // 无窗口
	}}
	if got := gate.CheckRuleItemWindowFields(doc); len(got) != 0 {
		t.Fatalf("合法生效期不应被报出，得到 %v", got)
	}
}

// TestG4Effectivity_SemanticsIsHalfOpen ★ 语义对平：区间是 `[from, to)`。
//
// 与 `compute/src/gate.rs::is_effective` 同源（含起不含止）。这条断言的作用是
// **把语义钉死**：任何人把任一端点判反（`>=` 写成 `>`、`<` 写成 `<=`），
// 这里必须变红。
func TestG4Effectivity_SemanticsIsHalfOpen(t *testing.T) {
	it := gate.RuleItem{ID: "a", EffectiveFrom: "2026-07-01", EffectiveTo: "2026-12-31"}
	cases := []struct {
		asOf string
		want bool
	}{
		{"2026-06-30", false}, // 早于 from
		{"2026-07-01", true},  // == from ⇒ 含起
		{"2026-12-30", true},
		{"2026-12-31", false}, // == to   ⇒ 不含止
		{"2027-01-01", false},
	}
	for _, c := range cases {
		got, why := gate.RuleItemEffective(it, c.asOf)
		if got != c.want {
			t.Errorf("as_of=%s 应 %v，实际 %v（%s）", c.asOf, c.want, got, why)
		}
	}
	// 单侧窗口。
	open := gate.RuleItem{ID: "b", EffectiveFrom: "2026-07-01"}
	if ok, _ := gate.RuleItemEffective(open, "2030-01-01"); !ok {
		t.Error("无上界应永不失效")
	}
	closed := gate.RuleItem{ID: "c", EffectiveTo: "2026-12-31"}
	if ok, _ := gate.RuleItemEffective(closed, "2026-12-31"); ok {
		t.Error("无下界但有上界时，== to 必须失效")
	}
	// 空基准日 ⇒ fail-closed。
	if ok, why := gate.RuleItemEffective(it, ""); ok || why == "" {
		t.Error("空 as_of 必须 fail-closed 且给出原因")
	}
}

// TestG4Effectivity_WindowBoundarySelfCheckIsLive 证明「语义自证」真的在跑：
// 当窗口两端都存在时，`CheckRuleItemWindowFields` 会实际调用
// `RuleItemEffective` 核对端点 —— 用一个「只有边界语义被破坏才会红」的
// 构造来验证（from 与 to 只差一天，端点判定若写成闭区间即被拦下）。
func TestG4Effectivity_WindowBoundarySelfCheckIsLive(t *testing.T) {
	doc := gate.RuleDoc{ID: "rule.x", Items: []gate.RuleItem{
		{ID: "a", EffectiveFrom: "2026-07-01", EffectiveTo: "2026-07-02"},
	}}
	if got := gate.CheckRuleItemWindowFields(doc); len(got) != 0 {
		t.Fatalf("相差一天的合法窗口不应被报出，得到 %v", got)
	}
}

// ───────────────────── 3. 聚合入口 ─────────────────────

func TestG4Effectivity_AggregateReportsBothClasses(t *testing.T) {
	doc := docWithItems("rule.x", []map[string]any{
		{"id": "a", "rate": 0.1, "effective_from": "2026-7-1"},
	})
	got := gate.CheckRuleItems(doc)
	if len(got) != 1 {
		t.Fatalf("聚合入口应报出 1 条（格式），得到 %v", got)
	}
	// 契约外键 + 窗口问题同时存在 ⇒ 两类都要报（聚合不得互相吞掉）。
	doc2 := docWithItems("rule.x", []map[string]any{
		{"id": "a", "rate": 0.1, "bogus": 1, "effective_from": "2026-7-1"},
	})
	if got := gate.CheckRuleItems(doc2); len(got) != 2 {
		t.Fatalf("应同时报出契约外键与窗口问题，得到 %v", got)
	}
}

// TestG4Effectivity_CiRunsGate ★ 接线断言：本闸门必须真的在 CI 里被执行。
//
// 静态闸门没有运行时调用点（`rules/*.yaml` 只在启动期被读），
// 「有没有真的接上 CI」只能靠这条断言钉住，而不是靠一句注释里的承诺。
func TestG4Effectivity_CiRunsGate(t *testing.T) {
	yml := repoFile(t, ".github/workflows/ci.yml")
	goJob, ok := yamlJobBlock(yml, "go")
	if !ok {
		t.Fatal("CI 缺少 `go` 作业 —— 规则项契约外键闸门不会被任何路径执行")
	}
	if !strings.Contains(goJob, "Effectivity|RuleItem") {
		t.Errorf("`go` 作业里没有跑规则项契约外键闸门（找不到 `Effectivity|RuleItem`）。" +
			"若删掉该步骤，G4 第十五侧就退回「代码里有、CI 里没有」的状态")
	}
}

// ───────────────────── 4. 真实数据不得被误杀（白名单覆盖性） ─────────────────────

// TestG4Effectivity_RealRulesYamlItemKeysAreAllKnown 断言**真实** `rules/*.yaml`
// 里用到的每个 items 键都在白名单内。
//
// 为什么单列一条：白名单是「契约字段集」，而真实 YAML 是「数据」——
// 若数据用了白名单外的键（例如某天有人加了 `unit`），本闸门会报错。
// 那条报错是**正确的**（该键确实会被静默丢弃），但必须由人来决定
// 「补进白名单 + 补进 rule.Item」还是「从 YAML 删掉」。本测试的作用是
// 把这件事**钉在测试里**，而不是让它在生产启动时才发现。
func TestG4Effectivity_RealRulesYamlItemKeysAreAllKnown(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "rules")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 rules/ 失败: %v", err)
	}
	known := map[string]bool{}
	for _, k := range gate.RuleItemKnownKeys() {
		known[k] = true
	}
	itemKeyRe := regexp.MustCompile(`^\s{4,}([A-Za-z_][A-Za-z0-9_]*)\s*:`)
	files := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		files++
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读取 %s: %v", e.Name(), err)
		}
		// 只在 `items:` 段内扫键（缩进 ≥4 的 `key:` 行即明细项字段）。
		inItems := false
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "items:") {
				inItems = true
				continue
			}
			if inItems && trimmed != "" && !strings.HasPrefix(trimmed, "#") &&
				!strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(line, " ") {
				inItems = false // 回到顶层键
			}
			if !inItems {
				continue
			}
			m := itemKeyRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if !known[m[1]] {
				t.Errorf("%s 的 items 用了白名单外的键 %q —— 它会被 yaml **静默丢弃**；"+
					"请决定：补进 gate.RuleItemKnownKeys() + rule.Item（真消费），"+
					"还是从 YAML 删掉", e.Name(), m[1])
			}
		}
	}
	if files == 0 {
		t.Fatal("rules/ 下没有 *.yaml —— 本测试的夹具前提不成立（目录被删空？）")
	}
}
