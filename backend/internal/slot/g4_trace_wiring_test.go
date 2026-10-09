package slot_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/precomp"
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// G4 第十四侧接线：证明 `gate.CheckTraceDeclared` 与 `gate.CheckAlgoTraceCoversColumns`
// 在**生产路径**（LoadRegistry → Validate、ContractGuard.Validate）里真的会被调用，
// 且 TraceDocs 真的把磁盘上 YAML 的 trace 声明带进来 —— 否则它会与前十三例一样，
// 只是「有定义、没消费」的装饰函数。
//
// ★ 本文件所有行为断言都从 `slot.LoadRegistry` 入口进、以加载成败出。

// ── 行为级：从 LoadRegistry 进、以加载成败出 ────────────────────────────────

// 物化算法（writes_bucket 非空）声明 trace: false ⇒ 加载必须失败。
//
// 这是本侧的核心断言：物化输出必须可回溯（docs/02 验收「所有输出可沿 AlgoTrace 回溯」），
// 若允许 trace: false，该列的值将永远追不回是哪条算法算的。
func TestTrace_Wiring_LoadRegistryRejectsFalseForMaterializingAlgo(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", slotYAMLForTrace)
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\npermission: L3\n"+
			"depends_on_slots: [slot.a]\nwrites_bucket: pnl_month\nmissing_policy: skip\ntrace: false\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ 物化算法声明 trace: false 仍加载成功 ⇒ trace 声明无判定消费（恒真）")
	}
	if !strings.Contains(err.Error(), "trace") {
		t.Fatalf("报错应指出 trace，实际：%v", err)
	}
}

// 反向：物化算法声明 trace: true 必须通过。
func TestTrace_Wiring_AcceptsTrueForMaterializingAlgo(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", slotYAMLForTrace)
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\npermission: L3\n"+
			"depends_on_slots: [slot.a]\nwrites_bucket: pnl_month\nmissing_policy: skip\ntrace: true\n")

	if _, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms")); err != nil {
		t.Fatalf("trace: true 的物化算法应通过：%v", err)
	}
}

// TestTrace_Wiring_RealRepoYAML —— 真仓库的 16 槽 + 4 算法必须全部通过。
//
// ★ 这条最重要：它保证闸门不是「只在合成夹具上绿」。若有人把某个算法 YAML 的
// trace 改成 false / 非布尔，本用例立即失败。
func TestTrace_Wiring_RealRepoYAML(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	for _, sub := range []string{"slots", "algorithms"} {
		copyDir(t, filepath.Join(root, sub), filepath.Join(dir, sub))
	}
	reg, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err != nil {
		t.Fatalf("真仓库 YAML 的 trace 声明未通过 G4 第十四侧：%v", err)
	}
	// 夹具自证：确实复制到了文件（否则上面的加载可能因「空目录」而以另一种错失败）。
	if n := countYAML(t, filepath.Join(dir, "algorithms")); n != 4 {
		t.Fatalf("夹具自证失败：复制到 %d 个算法文件，期望 4", n)
	}
	// ★ 命名约定同源：slot.FieldNameOf 必须与 precomp.FieldName 对全部真实算法逐字一致，
	//   否则 query 组装的 AlgoTrace.field 与桶列名会静默分叉（列在、溯源指向另一列）。
	for _, a := range reg.Algorithms() {
		if got, want := slot.FieldNameOf(a.ID), precomp.FieldName(a.ID); got != want {
			t.Fatalf("列名约定分叉：slot.FieldNameOf(%q)=%q，precomp.FieldName=%q", a.ID, got, want)
		}
	}
}

// ── 静态接线：钉住生产调用点真的存在（防「摘掉调用点、只留说明注释」）──────

func TestTrace_Wiring_ValidateCallsTraceDeclared(t *testing.T) {
	body := readMethodBody(t, "slot.go", "func (r *Registry) Validate() error {")
	if !strings.Contains(body, "r.TraceDocs()") {
		t.Fatal("★ Validate 里没调用 r.TraceDocs() ⇒ 判定输入不是真 YAML（可能是空切片，恒放行）")
	}
	// ★ 断言**完整调用语句**（violations = append(..., fn(...)...)），不是光有函数名 ——
	//   否则 `_ = gate.CheckTraceDeclared(...)` 这种「引用了但结果被丢弃」的写法
	//   会骗过 Contains（本轮注入实测的教训：注入 F 首轮即因此误报 MISSED）。
	if !strings.Contains(body, "violations = append(violations, gate.CheckTraceDeclared(r.TraceDocs())...)") {
		t.Fatal("★ Validate 没以调用语句形态使用 gate.CheckTraceDeclared(r.TraceDocs()) ⇒ 闸门无生产调用点（恒真）")
	}
	if len(body) < 400 {
		t.Fatalf("夹具自证失败：Validate 方法体只读到 %d 字节，扫描器可能没命中签名", len(body))
	}
}

// TraceDocs 必须回看 YAML 原始键（Raw），否则拿不到「是否声明」与「值是否布尔」。
func TestTrace_Wiring_DocsSourceIsDiskTruth(t *testing.T) {
	body := readMethodBody(t, "slot.go", "func (r *Registry) TraceDocs() []gate.TraceDoc {")
	if !strings.Contains(body, "rawTraceValue") {
		t.Fatal("★ TraceDocs 没走 rawTraceValue ⇒ 拿不到「键是否存在 / 值是否布尔」")
	}
	raw := readMethodBody(t, "slot.go", "func rawTraceValue(a Algorithm) (val bool, declared bool, valid bool) {")
	if !strings.Contains(raw, `a.Raw["trace"]`) {
		t.Fatal("★ rawTraceValue 没读 a.Raw[\"trace\"] ⇒ 无法察觉「声明了但不可判定」")
	}
}

// 出站路径：ContractGuard.Validate 必须调用 CheckAlgoTraceCoversColumns。
func TestTrace_Wiring_ContractGuardCallsCoverageCheck(t *testing.T) {
	body := readMethodBody(t, "contract_guard.go", "func (g *ContractGuard) Validate(dc *contracts.DataContract) []string {")
	// ★ 断言完整调用语句，防「引用但结果被丢弃」（`_ = gate.CheckAlgoTraceCoversColumns(dc)`）。
	if !strings.Contains(body, "violations = append(violations, gate.CheckAlgoTraceCoversColumns(dc)...)") {
		t.Fatal("★ ContractGuard.Validate 没以调用语句形态使用 gate.CheckAlgoTraceCoversColumns(dc) ⇒ 出站路径不校验可回溯性")
	}
	if len(body) < 200 {
		t.Fatalf("夹具自证失败：Validate 方法体只读到 %d 字节", len(body))
	}
}

// slotYAMLForTrace 是一个字段完整的合法槽夹具。
const slotYAMLForTrace = "id: slot.a\nname: A\nsource_kind: api\nsource_ref: x.api\n" +
	"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n"
