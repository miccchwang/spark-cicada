package slot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// G4 第八侧接线：证明 `gate.CheckPermissionDeclared` 在**生产路径**
// （LoadRegistry → Validate）里真的会被调用 —— 否则它会与前十五例一样，
// 只是「有定义、没消费」的装饰函数。
//
// ★ 本文件所有断言都从 `slot.LoadRegistry` 入口进、以加载成败出，
// 不直接调 gate 判定函数 —— 那是 gate 包测试的职责（分层：闸门测判定、
// 本包测**接线是否真的存在**）。

// TestPermission_Wiring_LoadRegistryRejectsFreeText
// 生产入口：把一个槽的 permission 改成自由文本，LoadRegistry 必须失败。
func TestPermission_Wiring_LoadRegistryRejectsFreeText(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: api\nsource_ref: x.api\n"+
			"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: 高密\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\npermission: L3\nmissing_policy: skip\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ permission 写自由文本仍加载成功 ⇒ 该字段无判定消费（恒真）")
	}
	if !strings.Contains(err.Error(), "permission") {
		t.Fatalf("报错应指出 permission，实际：%v", err)
	}
}

// TestPermission_Wiring_LoadRegistryRejectsAlgorithmFreeText
// 生产入口：算法的 permission 写自由文本，也要失败（槽与算法两侧都收）。
func TestPermission_Wiring_LoadRegistryRejectsAlgorithmFreeText(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: api\nsource_ref: x.api\n"+
			"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\npermission: 核心\nmissing_policy: skip\n"+
			"depends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ 算法 permission 写自由文本仍加载成功 ⇒ 算法的密级列无判定消费")
	}
	if !strings.Contains(err.Error(), "permission") {
		t.Fatalf("报错应指出 permission，实际：%v", err)
	}
}

// TestPermission_Wiring_LoadRegistryRejectsMissing 删掉声明必须失败。
func TestPermission_Wiring_LoadRegistryRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: api\nsource_ref: x.api\n"+
			"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\npermission: L3\nmissing_policy: skip\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ 缺 permission 仍加载成功 ⇒ 无密级判据却无人拦")
	}
	if !strings.Contains(err.Error(), "permission") {
		t.Fatalf("报错应指出 permission，实际：%v", err)
	}
}

// TestPermission_Wiring_RealRepoYAML —— 真仓库的 16 槽 + 4 算法必须全部通过。
//
// ★ 这条最重要：它保证闸门不是「只在合成夹具上绿」。若有人把某个 YAML 的
// permission 改成自由文本，本用例立即失败。
func TestPermission_Wiring_RealRepoYAML(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	// 复制真仓库的 slots/algorithms 到临时目录（避免污染）。
	for _, sub := range []string{"slots", "algorithms"} {
		copyDir(t, filepath.Join(root, sub), filepath.Join(dir, sub))
	}
	if _, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms")); err != nil {
		t.Fatalf("真仓库 YAML 的密级声明未通过 G4 第八侧：%v", err)
	}
	// 夹具自证：确实复制到了文件，否则上面的加载可能因「空目录」而以另一种错失败。
	if n := countYAML(t, filepath.Join(dir, "slots")); n != 16 {
		t.Fatalf("夹具自证失败：复制到 %d 个槽文件，期望 16", n)
	}
	if n := countYAML(t, filepath.Join(dir, "algorithms")); n != 4 {
		t.Fatalf("夹具自证失败：复制到 %d 个算法文件，期望 4", n)
	}
}

// ── 工具 ────────────────────────────────────────────────────────────────────

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dst, err)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", src, err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", e.Name(), err)
		}
	}
}

func countYAML(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			n++
		}
	}
	return n
}
