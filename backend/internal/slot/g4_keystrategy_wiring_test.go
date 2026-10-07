package slot_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// G4 第七侧接线：证明 `gate.CheckKeyStrategyDeclared` / `CheckKeyStrategyCohesion`
// 在**生产路径**（LoadRegistry → Validate）里真的会被调用 —— 否则它们与
// 前十四例一样，只是「有定义、没消费」的装饰函数。
//
// ★ 本文件所有断言都从 `slot.LoadRegistry` 入口进、以加载成败出，
// 不直接调 gate 判定函数 —— 那是 gate 包测试的职责（分层：闸门测判定、
// 本包测**接线是否真的存在**）。

// TestKeyStrategy_Wiring_LoadRegistryRejectsFreeText
// 生产入口：把一个槽的 key_strategy 改成自由文本，LoadRegistry 必须失败。
func TestKeyStrategy_Wiring_LoadRegistryRejectsFreeText(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: api\nsource_ref: x.api\n"+
			"key_strategy: 智能匹配\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ key_strategy 写自由文本仍加载成功 ⇒ 该字段无判定消费（恒真）")
	}
	if !strings.Contains(err.Error(), "key_strategy") {
		t.Fatalf("报错应指出 key_strategy，实际：%v", err)
	}
}

// TestKeyStrategy_Wiring_LoadRegistryRejectsMissing 删掉声明必须失败。
func TestKeyStrategy_Wiring_LoadRegistryRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: api\nsource_ref: x.api\n"+
			"coverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ 缺 key_strategy 仍加载成功 ⇒ 无对齐判据却无人拦")
	}
	if !strings.Contains(err.Error(), "未声明 key_strategy") {
		t.Fatalf("报错应说明「未声明」，实际：%v", err)
	}
}

// TestKeyStrategy_Wiring_LoadRegistryRejectsCohesionConflict
// 两个槽共用来源却键口径不同 ⇒ 逐条合法、合起来是错的，必须拦。
func TestKeyStrategy_Wiring_LoadRegistryRejectsCohesionConflict(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: derived\nsource_ref: channel_sales\n"+
			"key_strategy: store_sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "slots"), "slot.b.yaml",
		"id: slot.b\nname: B\nsource_kind: derived\nsource_ref: channel_sales\n"+
			"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\ndepends_on_slots: [slot.a, slot.b]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ 共用来源却键口径冲突仍加载成功 ⇒ 合并结果会对不上且无人知")
	}
	if !strings.Contains(err.Error(), "却按不同键对齐") {
		t.Fatalf("报错应指出键口径冲突，实际：%v", err)
	}
}

// TestKeyStrategy_Wiring_RealSpecsLoad 真实定义必须仍然全绿
// （防止本闸门上线即把仓库既有定义判红 —— 若真判红，说明定义确实有问题，
//
//	那应当去修定义，而不是放宽闸门；此测试即那条红线的守门人）。
func TestKeyStrategy_Wiring_RealSpecsLoad(t *testing.T) {
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(
		filepath.Join(root, "slots"),
		filepath.Join(root, "algorithms"),
	)
	if err != nil {
		t.Fatalf("真实 slots/algorithms 未通过 key_strategy 校验：%v", err)
	}
	docs := reg.KeyStrategyDocs()
	if len(docs) == 0 {
		t.Fatal("夹具自证：真实定义读到的槽数量为 0 ⇒ 本断言恒真")
	}
	// 夹具自证：每个真实槽都必须有非空 key_strategy，否则「全绿」无意义。
	for _, d := range docs {
		if strings.TrimSpace(d.Raw) == "" {
			t.Fatalf("真实槽 %s 缺 key_strategy ⇒ 闸门放过了它", d.SlotID)
		}
	}
}

// TestKeyStrategy_Wiring_HasProductionCallSite
// 静态源码扫描：钉住「判定函数有非测试调用点」。
//
// ★ 这是本仓「判定函数全仓无生产调用点」这个病的**通用解药**：
// 只断言行为不够 —— 如果调用点被整段摘掉，行为断言也可能因为别的路径而全绿。
func TestKeyStrategy_Wiring_HasProductionCallSite(t *testing.T) {
	backend := filepath.Join(repoRoot(t), "backend")
	for _, fn := range []string{"CheckKeyStrategyDeclared", "CheckKeyStrategyCohesion"} {
		sites := nonTestCallSitesIn(t, backend, fn)
		joined := strings.Join(sites, ",")
		if !strings.Contains(joined, "internal/slot/slot.go") {
			t.Fatalf("★ gate.%s 没有任何**非测试**调用点 ⇒ 该闸门在生产侧恒真；实际调用点：%v",
				fn, sites)
		}
	}
	// 扫描器自证：拿一个**必然不存在**的函数名扫描，结果必须为空
	// —— 否则说明扫描器在乱命中，上面的断言可能就是被它自己骗绿的。
	//
	// ★ needle 运行时拼接（不写成字面量）：若以字面量写进本文件，
	// `strings.Contains(line, needle+"(")` 会命中本文件自己 ⇒ 自指恒真。
	needle := "Check" + "KeyStrategyNoSuchFunctionZZZ"
	if sites := nonTestCallSitesIn(t, backend, needle); len(sites) != 0 {
		t.Fatalf("★ 扫描器自证失败：不存在的函数 %q 也扫出了调用点 %v", needle, sites)
	}
}
