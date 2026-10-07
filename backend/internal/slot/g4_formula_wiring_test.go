// g4_formula_wiring_test.go —— ★ 接线闸门：G4 值绑定判定函数必须有**非测试**调用点。
//
// 本仓反复出现的病灶是：「判定函数全仓唯一的调用者是它自己的 _test.go」⇒
// 断言测的是返回值，而**生产路径从不调用它** ⇒ 恒真。
// 本轮新增的两条 G4 判定函数若只写在 gate 包里配个单测，就是同一个病的复发。
//
// 本文件用**静态源码扫描**钉住：
//   * gate.CheckFormulaVariablesBound 在 backend/ 下有非测试调用点；
//   * gate.CheckPendingAlgosNotProducing 同上；
//   * 调用点必须位于 `slot/bucket.go`（BucketRegistry.validate）—— 即真实装载入口；
//   * 并有**扫描器自证**：一个故意不存在的函数名必须扫不到任何调用点。
package slot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nonTestCallSites 扫描 backend 下所有非 _test.go，返回调用 fn( 的文件（相对路径）。
//
// ★ 与 rule_test.go 里的同名辅助逻辑一致；此处独立实现以免跨包依赖，
// 并额外跳过 `//` 注释行（否则「在注释里提一句函数名」就会被误认为调用点）。
func nonTestCallSites(t *testing.T, backend, fn string) []string {
	t.Helper()
	var sites []string
	def := "func " + fn + "("
	err := filepath.WalkDir(backend, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(backend, path)
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, def) {
				continue
			}
			if strings.Contains(line, fn+"(") {
				sites = append(sites, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites
}

// TestWiring_FormulaVariablesBoundHasProductionCallSite
// ★ 若这条失败，说明「G4 值绑定」又退回了「只在测试里被调用」= 恒真。
func TestWiring_FormulaVariablesBoundHasProductionCallSite(t *testing.T) {
	sites := nonTestCallSites(t, backendRoot(t), "CheckFormulaVariablesBound")
	if len(sites) == 0 {
		t.Fatal("★ gate.CheckFormulaVariablesBound 没有任何非测试调用点 ⇒ 该断言恒真")
	}
	found := false
	for _, s := range sites {
		if strings.HasSuffix(s, "slot/bucket.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("★ 期望调用点位于 slot/bucket.go（真装载入口），实际: %v", sites)
	}
}

// TestWiring_PendingAlgosNotProducingHasProductionCallSite 同上。
func TestWiring_PendingAlgosNotProducingHasProductionCallSite(t *testing.T) {
	sites := nonTestCallSites(t, backendRoot(t), "CheckPendingAlgosNotProducing")
	if len(sites) == 0 {
		t.Fatal("★ gate.CheckPendingAlgosNotProducing 没有任何非测试调用点 ⇒ 恒真")
	}
	found := false
	for _, s := range sites {
		if strings.HasSuffix(s, "slot/bucket.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("★ 期望调用点位于 slot/bucket.go，实际: %v", sites)
	}
}

// TestWiring_ScannerSelfProof 扫描器自证：一个不存在的函数名必须扫不到调用点。
//
// ★ 防「扫描器恒返回命中」：若它无条件返回文件，上面两条断言就也是恒真。
func TestWiring_ScannerSelfProof(t *testing.T) {
	if sites := nonTestCallSites(t, backendRoot(t),
		"CheckThisFunctionDefinitelyDoesNotExistXyz"); len(sites) != 0 {
		t.Fatalf("★ 扫描器自证失败：不存在的函数名竟扫到调用点 %v ⇒ 上面两条接线断言不可信", sites)
	}
	// 反向自证：一个**确实存在且有生产调用点**的函数必须能扫到。
	// （ValidateWithRules 由 sparkd 调用，见 rule_test 的同名接线断言。）
	if sites := nonTestCallSites(t, backendRoot(t), "CheckBucketProducersRegistered"); len(sites) == 0 {
		t.Fatal("★ 扫描器自证失败：已知有生产调用点的函数却扫不到 ⇒ 扫描逻辑有误")
	}
}

// TestBuildRow_UsesSameBareNameConventionAsGate
// ★ 横向一致性：运行时（precomp）与静态闸门（gate）必须用**同一条**命名约定。
//
// 若两者分叉（如 gate 用 BareName 校验而 precomp 绑定别的名字），
// 就会出现「静态全绿、运行时全空」的最坏组合 —— 本仓最贵的一类失效。
func TestBuildRow_UsesSameBareNameConventionAsGate(t *testing.T) {
	root := repoRoot(t)
	// precomp 绑定槽变量处必须调用 compute.BareName。
	pc, err := os.ReadFile(filepath.Join(root, "backend", "internal", "precomp", "precomp.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pc), "compute.BareName(") {
		t.Error("★ precomp 未用 compute.BareName 折算槽变量名 ⇒ 运行时与静态闸门的约定可能分叉")
	}
	// gate 侧的 BareName 与 compute 侧实现必须一致（gate 是权威，compute 复刻）。
	gc, err := os.ReadFile(filepath.Join(root, "backend", "internal", "gate", "gate.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gc), "func BareName(") {
		t.Error("★ gate 未导出 BareName ⇒ 无法作为命名约定的权威来源")
	}
}
