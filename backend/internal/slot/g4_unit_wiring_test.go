package slot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// G4 第九侧接线：证明 `gate.CheckUnitDeclared` / `CheckUnitMatchesFormulaKind`
// 在**生产路径**（LoadRegistry → Validate）里真的会被调用 —— 否则它会与前十六例
// 一样，只是「有定义、没消费」的装饰函数。
//
// ★ 本文件所有断言都从 `slot.LoadRegistry` 入口进、以加载成败出，
// 不直接调 gate 判定函数 —— 那是 gate 包测试的职责（分层：闸门测判定、
// 本包测**接线是否真的存在**）。

const validSlotForUnit = "id: slot.a\nname: A\nsource_kind: api\nsource_ref: x.api\n" +
	"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n"

// TestUnit_Wiring_LoadRegistryRejectsMissing
// 生产入口：算法**不写 unit** ⇒ 必须加载失败（字段缺失就是无判据）。
func TestUnit_Wiring_LoadRegistryRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", validSlotForUnit)
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\npermission: L3\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ 算法缺 unit 仍加载成功 ⇒ 该字段无判定消费（恒真）")
	}
	if !strings.Contains(err.Error(), "unit") {
		t.Fatalf("报错应指出 unit，实际：%v", err)
	}
}

// TestUnit_Wiring_LoadRegistryRejectsFreeText
// 生产入口：unit 写自由文本 ⇒ 必须加载失败。
func TestUnit_Wiring_LoadRegistryRejectsFreeText(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", validSlotForUnit)
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: 货币\npermission: L3\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ unit 写自由文本（货币）仍加载成功 ⇒ 量纲无判据却无人拦")
	}
	if !strings.Contains(err.Error(), "unit") {
		t.Fatalf("报错应指出 unit，实际：%v", err)
	}
}

// TestUnit_Wiring_LoadRegistryRejectsRatioAsAmount
// 生产入口：比率公式却声明金额单位 ⇒ 必须加载失败（量纲自相矛盾）。
func TestUnit_Wiring_LoadRegistryRejectsRatioAsAmount(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", validSlotForUnit)
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: \"gp / revenue\"\nunit: THB\n"+
			"permission: L3\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ 比率公式声明 THB 仍加载成功 ⇒ 单位与公式形态的相称判定没接线")
	}
	if !strings.Contains(err.Error(), "unit") {
		t.Fatalf("报错应指出 unit，实际：%v", err)
	}
}

// TestUnit_Wiring_AcceptsKnownForms
// 正向：规范写法与已知别名都必须能加载（防「闸门过严 ⇒ 正确写法也报」）。
//
// ★ 注意 `%` 在 YAML 里是保留起始字符，必须加引号 —— 这是**夹具书写**要求，
// 与闸门无关（真仓 algorithms/gmp.yaml 写的是 `percent` 无此问题）。
func TestUnit_Wiring_AcceptsKnownForms(t *testing.T) {
	for _, u := range []string{"THB", "percent", "%", "count", "ratio"} {
		dir := t.TempDir()
		writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", validSlotForUnit)
		writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
			"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: \""+u+"\"\n"+
				"permission: L3\ndepends_on_slots: [slot.a]\n")
		if _, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms")); err != nil {
			t.Fatalf("unit=%q 是已知单位，不应被拒：%v", u, err)
		}
	}
}

// TestUnit_Wiring_RealRepoYAML —— 真仓库的 16 槽 + 4 算法必须全部通过。
//
// ★ 这条最重要：它保证闸门不是「只在合成夹具上绿」。若有人把某个算法 YAML 的
// unit 改成自由文本、或把比率算法的单位改成 THB，本用例立即失败。
func TestUnit_Wiring_RealRepoYAML(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	for _, sub := range []string{"slots", "algorithms"} {
		copyDir(t, filepath.Join(root, sub), filepath.Join(dir, sub))
	}
	if _, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms")); err != nil {
		t.Fatalf("真仓库 YAML 的单位声明未通过 G4 第九侧：%v", err)
	}
	// 夹具自证：确实复制到了文件，否则上面的加载可能因「空目录」而以另一种错失败。
	if n := countYAML(t, filepath.Join(dir, "slots")); n != 16 {
		t.Fatalf("夹具自证失败：复制到 %d 个槽文件，期望 16", n)
	}
	if n := countYAML(t, filepath.Join(dir, "algorithms")); n != 4 {
		t.Fatalf("夹具自证失败：复制到 %d 个算法文件，期望 4", n)
	}
}

// TestUnit_Wiring_SourceScanPinsProductionCallSites
//
// ★ 静态源码扫描断言：`slot.go` 的 Validate 里**必须真的**调用这两个判定函数。
//
// 为什么需要它（而不是只靠上面的加载用例）：上面的用例只能证明「改成自由文本
// 会失败」，但**不能区分**「被这两个闸门拦下」还是「被别的既有校验顺带拦下」。
// 本断言直接钉住调用点存在，让「有人把这两行删掉」变成一个可被检出的破坏。
//
// ★ 断言不得自指（本仓纪律）：needle 在**运行时拼接**，不把被查的名字
// 以字面量写进断言自己的常量里 —— 否则扫描器会命中自己、恒真。
func TestUnit_Wiring_SourceScanPinsProductionCallSites(t *testing.T) {
	root := repoRoot(t)
	body := readFileOrFail(t, filepath.Join(root, "backend", "internal", "slot", "slot.go"))

	// Validate 方法体：从 `func (r *Registry) Validate() error {` 起，到下一个顶格 func 前。
	start := strings.Index(body, "func (r *Registry) Validate() error {")
	if start < 0 {
		t.Fatal("未找到 Registry.Validate —— 生产接线点消失了")
	}
	rest := body[start:]
	if end := strings.Index(rest[1:], "\nfunc "); end >= 0 {
		rest = rest[:end+1]
	}

	// needle 运行时拼接（防自指恒真）。
	for _, fn := range []string{"CheckUnit" + "Declared", "CheckUnit" + "MatchesFormulaKind"} {
		if !strings.Contains(rest, fn) {
			t.Fatalf("★ Validate 里没有调用 %s ⇒ 该判定函数无生产调用点（恒真装饰）", fn)
		}
	}

	// 反向自证：扫描器**必须能**在明显无调用的地方返回「找不到」。
	// 用同一个 needle 的变形在别处搜，确认 Contains 不是恒真。
	bogus := "CheckUnit" + "NeverExistedFunction"
	if strings.Contains(rest, bogus) {
		t.Fatal("扫描器恒真（凭空命中不存在的函数）—— 本断言无意义")
	}
}

// readFileOrFail 读文件（失败即 Fatal）。
func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return string(b)
}
