// g4_unit_test.go —— G4 第九侧判定函数本身的单元测试。
//
// 被测对象：
//
//	gate.ParseUnit                         唯一权威解析（只大小写归一 + 已知别名）
//	gate.CheckUnitDeclared                 声明必须非空且可解析
//	gate.CheckUnitMatchesFormulaKind       比率公式不得声明为金额（单向可疑提示）
//	gate.CheckUnitVocabularyMatchesDB      Go 词表 ↔ 0002 DDL / 0004 种子 同源
//
// ★ 反向自证（本仓纪律）：每条断言都配一个「**一个错误的实现能不能通过它**」的
// 反问用例 —— 参 G4 第八侧教训（断言的结果对 ≠ 断言测的东西对）。
package gate

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ───────────────────────── ParseUnit：唯一权威解析 ─────────────────────────

func TestUnit_ParseKnownForms(t *testing.T) {
	// 规范形 + 大小写变体 + 已知别名，全部应解析且**归一到规范形**。
	cases := []struct {
		raw  string
		want string
	}{
		{"THB", "THB"},
		{"thb", "THB"},
		{" Thb ", "THB"},
		{"%", "%"},
		{"percent", "%"},
		{"PERCENT", "%"},
		{"pct", "%"},
		{"ratio", "ratio"},
		{"RATIO", "ratio"},
		{"count", "count"},
		{"CNT", "count"},
		{"CNY", "CNY"}, // 种子在用的历史写法
	}
	for _, c := range cases {
		got, ok := ParseUnit(c.raw)
		if !ok {
			t.Fatalf("ParseUnit(%q) 应被接受（它是已知单位或已知别名）", c.raw)
		}
		if got != c.want {
			t.Fatalf("ParseUnit(%q) = %q，期望规范形 %q", c.raw, got, c.want)
		}
	}
}

func TestUnit_RejectsFreeText(t *testing.T) {
	// fail-closed：自然语言 / 复合单位 / 空串 / 未知码 一律拒。
	bad := []string{
		"",
		"   ",
		"货币",
		"元",
		"泰铢",
		"百分比",
		"件",
		"THB/件",  // 复合单位：本闸门不做量纲推导，果断拒（宁少不假）
		"币种",     // 自然语言
		"USD",    // 未在词表（本仓本位币是 THB，USD 需显式登记）
		"?",      // 未知
		"count个", // 混合
	}
	for _, raw := range bad {
		if u, ok := ParseUnit(raw); ok {
			t.Fatalf("ParseUnit(%q) 应被拒（自由文本/复合单位不得静默放行），却得到 %q", raw, u)
		}
	}
}

func TestUnit_VocabularySingleSource(t *testing.T) {
	// 词表自证：UnitVocabulary 的每一项都必须能被 ParseUnit 解析出来
	// （否则就是「词表列了、解析器不认」的分叉 —— 正是本仓反复踩的坑）。
	for _, u := range UnitVocabulary() {
		if _, ok := ParseUnit(u); !ok {
			t.Fatalf("词表列出了 %q，但 ParseUnit 不认它 —— 两侧分叉", u)
		}
	}
	// UnitCodes（解析器实际认得的规范集合）必须覆盖词表。
	codes := map[string]bool{}
	for _, c := range UnitCodes() {
		codes[c] = true
	}
	for _, u := range UnitVocabulary() {
		if !codes[u] {
			t.Fatalf("词表含 %q，但解析器的规范集合里没有它", u)
		}
	}
	if len(UnitVocabulary()) == 0 {
		t.Fatal("单位词表为空（真空表会让所有断言恒真）")
	}
}

// ────────────────────── CheckUnitDeclared：声明层闸门 ──────────────────────

func TestUnit_DeclaredRejectsMissingAndFreeText(t *testing.T) {
	docs := []UnitDoc{
		{Kind: "算法", ID: "algo.a", Raw: ""},      // 缺声明
		{Kind: "算法", ID: "algo.b", Raw: "   "},   // 空白
		{Kind: "算法", ID: "algo.c", Raw: "货币"},    // 自由文本
		{Kind: "算法", ID: "algo.d", Raw: "THB/件"}, // 复合
	}
	v := CheckUnitDeclared(docs)
	if len(v) != len(docs) {
		t.Fatalf("应逐条报出 %d 条违规，实际 %d 条：\n%s", len(docs), len(v), strings.Join(v, "\n"))
	}
	// 报错必须**定位到具体算法**且带允许词表（否则人类无法修）。
	joined := strings.Join(v, "\n")
	for _, want := range []string{"algo.a", "algo.b", "algo.c", "algo.d", "THB"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("报错信息缺少 %q，无法定位/修复：\n%s", want, joined)
		}
	}
	// 排序（确定性）
	if !sort.StringsAreSorted(v) {
		t.Fatalf("违规清单应有序（便于告警去重）：%v", v)
	}
}

func TestUnit_DeclaredAcceptsKnownForms(t *testing.T) {
	docs := []UnitDoc{
		{Kind: "算法", ID: "algo.a", Raw: "THB"},
		{Kind: "算法", ID: "algo.b", Raw: "percent"},
		{Kind: "算法", ID: "algo.c", Raw: "%"},
		{Kind: "算法", ID: "algo.d", Raw: "count"},
	}
	if v := CheckUnitDeclared(docs); len(v) != 0 {
		t.Fatalf("已知单位不应报违规，实际：\n%s", strings.Join(v, "\n"))
	}
}

// ──────────────── CheckUnitMatchesFormulaKind：单位与公式相称 ────────────────

func TestUnit_FormulaKindMismatch(t *testing.T) {
	// 比率公式（顶层裸名相除）却声明为金额 ⇒ 必须报。
	docs := []UnitDoc{
		{Kind: "算法", ID: "algo.gmp", Raw: "THB"},  // 公式 gp / revenue ⇒ 矛盾
		{Kind: "算法", ID: "algo.gp", Raw: "THB"},   // 公式 revenue - cogs ⇒ 无矛盾
		{Kind: "算法", ID: "algo.ratio2", Raw: "%"}, // 比率公式 + 比率单位 ⇒ 无矛盾
	}
	formulas := map[string]string{
		"algo.gmp":    "gp / revenue",
		"algo.gp":     "revenue - cogs",
		"algo.ratio2": "a / b",
	}
	v := CheckUnitMatchesFormulaKind(docs, formulas)
	if len(v) != 1 {
		t.Fatalf("应恰好报 1 条（algo.gmp），实际 %d 条：\n%s", len(v), strings.Join(v, "\n"))
	}
	if !strings.Contains(v[0], "algo.gmp") {
		t.Fatalf("应定位到 algo.gmp，实际：%s", v[0])
	}
}

func TestUnit_SelfDivisionIsNotRatio(t *testing.T) {
	// x / x 恒为 1，不是比率语义 ⇒ 不应被当成比率形态而误报。
	docs := []UnitDoc{{Kind: "算法", ID: "algo.same", Raw: "THB"}}
	formulas := map[string]string{"algo.same": "qty / qty"}
	if v := CheckUnitMatchesFormulaKind(docs, formulas); len(v) != 0 {
		t.Fatalf("自除（x/x）不应被判为比率，实际误报：\n%s", strings.Join(v, "\n"))
	}
}

func TestUnit_AggregateDivisionNotFlagged(t *testing.T) {
	// sum(a)/sum(b) 两侧同为聚合，量纲由被聚合列决定 —— 静态判不了，**保守放过**。
	// ★ 这条用例固化「本闸门刻意很窄」的设计：宁漏不误伤（参 G4 第八侧教训）。
	docs := []UnitDoc{{Kind: "算法", ID: "algo.avg", Raw: "THB"}}
	formulas := map[string]string{"algo.avg": "sum(revenue) / sum(qty)"}
	if v := CheckUnitMatchesFormulaKind(docs, formulas); len(v) != 0 {
		t.Fatalf("聚合相除不应被静态判为比率（会误伤），实际：\n%s", strings.Join(v, "\n"))
	}
}

func TestUnit_MismatchSkippedWhenUnitUnparsable(t *testing.T) {
	// 单位本身不可解析时，只由 CheckUnitDeclared 报一次，不重复报（避免噪音淹没有效信号）。
	docs := []UnitDoc{{Kind: "算法", ID: "algo.x", Raw: "货币"}}
	formulas := map[string]string{"algo.x": "gp / revenue"}
	if v := CheckUnitMatchesFormulaKind(docs, formulas); len(v) != 0 {
		t.Fatalf("不可解析的单位不应在相称判定里重复报，实际：\n%s", strings.Join(v, "\n"))
	}
	if v := CheckUnitDeclared(docs); len(v) != 1 {
		t.Fatalf("应由 CheckUnitDeclared 恰好报 1 条，实际 %d 条", len(v))
	}
}

// ───────────── CheckUnitVocabularyMatchesDB：与 DB 静态同源 ─────────────

func TestUnit_VocabularyMatchesDB_RealMigrations(t *testing.T) {
	// ★ 用**仓库里的真迁移文本**核对（不是手搓串）—— 这才是「同源」的真检验。
	root := repoRoot(t)
	ddl, err := os.ReadFile(filepath.Join(root, "sql", "migrations", "0002_slots_and_rules.sql"))
	if err != nil {
		t.Fatalf("读 0002 迁移失败: %v", err)
	}
	seed, err := os.ReadFile(filepath.Join(root, "sql", "migrations", "0004_registry_seed.sql"))
	if err != nil {
		t.Fatalf("读 0004 种子失败: %v", err)
	}
	if v := CheckUnitVocabularyMatchesDB(string(ddl), string(seed)); len(v) != 0 {
		t.Fatalf("真迁移应同源（种子里的 unit 字面量必须都能被解析）：\n%s", strings.Join(v, "\n"))
	}
	// 夹具自证：确实读到了内容（否则「空文件也绿」）。
	if len(ddl) < 1000 || len(seed) < 1000 {
		t.Fatalf("迁移文本过短（%d/%d 字节）—— 夹具没读到真文件，本用例是假的", len(ddl), len(seed))
	}
	if !strings.Contains(string(seed), "registry_algorithm") {
		t.Fatalf("0004 种子里没有 registry_algorithm —— 夹具盯错文件了")
	}
}

func TestUnit_VocabularyMatchesDB_DetectsSeedDrift(t *testing.T) {
	// 反向自证：种子里塞一个**解析器不认**的单位字面量 ⇒ 必须被检出。
	seed := "INSERT INTO registry_algorithm (id, unit) VALUES ('algo.x', 'USD');"
	v := CheckUnitVocabularyMatchesDB("", seed)
	if len(v) == 0 {
		t.Fatal("种子里出现未登记单位 USD 应被检出（否则种子与算法 YAML 的口径分叉无人知）")
	}
	if !strings.Contains(strings.Join(v, "\n"), "USD") {
		t.Fatalf("报错应点名 USD，实际：\n%s", strings.Join(v, "\n"))
	}
}

func TestUnit_VocabularyMatchesDB_DetectsDDLCheckDrift(t *testing.T) {
	// 若 0002 的 unit 列加了 CHECK 且含 Go 侧不认的档 ⇒ 必须被检出。
	ddl := "unit text NOT NULL CHECK (unit IN ('THB','JPY')),"
	v := CheckUnitVocabularyMatchesDB(ddl, "")
	if len(v) == 0 {
		t.Fatal("0002 的 unit CHECK 含 JPY 而 Go 侧不认 ⇒ 应被检出")
	}
	if !strings.Contains(strings.Join(v, "\n"), "JPY") {
		t.Fatalf("报错应点名 JPY，实际：\n%s", strings.Join(v, "\n"))
	}
	// 反向：DDL 与本包词表**完全一致**时不应报（防「恒真报警」）。
	if v := CheckUnitVocabularyMatchesDB(
		"unit text NOT NULL CHECK (unit IN ('THB','%','count','ratio')),", ""); len(v) != 0 {
		t.Fatalf("同源 DDL 不应报违规，实际：\n%s", strings.Join(v, "\n"))
	}
}

// ─────────────────────────── 辅助：仓库根目录 ───────────────────────────

// repoRoot 从测试文件位置向上找含 sql/migrations 的目录。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd() // backend/internal/gate
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "sql", "migrations")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("找不到仓库根（含 sql/migrations 的目录）")
	return ""
}
