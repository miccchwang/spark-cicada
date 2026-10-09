package slot_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// G4 第十三侧接线：证明 `gate.CheckMissingPolicyDeclared` 在**生产路径**
// （LoadRegistry → Validate）里真的会被调用，且 `MissingPolicyDocs` 真的把
// 磁盘上 YAML 的**原始写法**带进来 —— 否则它会与前面十二例一样，
// 只是「有定义、没消费」的装饰函数。
//
// ★ 本文件所有断言都从 `slot.LoadRegistry` 入口进、以加载成败出。
//
// ★★ 本轮的核心发现（裸 null 陷阱）在这里做**行为级**验证：
//
//	docs 明写三值 `skip | null | error`，但作者按文档写 `missing_policy: null`
//	时，YAML 会把裸 `null` 解析成**空值** ⇒ 结构体得到 ""。若不区分「漏写」
//	与「写了裸 null」，这条静默语义反转（想要「置空」却得到「跳过」）将永远
//	无人发现。故本文件必须有「裸 null 加载失败且报错含『YAML 空值』」这一条。

// ── 行为级：从 LoadRegistry 进、以加载成败出 ────────────────────────────────

func TestMissingPolicy_Wiring_LoadRegistryRejectsFreeText(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", slotYAMLForMissing)
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\npermission: L3\n"+
			"missing_policy: ignore\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ missing_policy 写别名 ignore 仍加载成功 ⇒ 该字段无判定消费（恒真）")
	}
	if !strings.Contains(err.Error(), "missing_policy") {
		t.Fatalf("报错应指出 missing_policy，实际：%v", err)
	}
}

func TestMissingPolicy_Wiring_LoadRegistryRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", slotYAMLForMissing)
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\npermission: L3\n"+
			"depends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ 缺 missing_policy 仍加载成功 ⇒ 无缺失行为判据却无人拦")
	}
	if !strings.Contains(err.Error(), "missing_policy") {
		t.Fatalf("报错应指出 missing_policy，实际：%v", err)
	}
}

// ★ 本轮核心：裸 `null`（YAML 保留字）必须被**专门**报出，而不是当成「漏写」。
func TestMissingPolicy_Wiring_BareNullIsRejected(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", slotYAMLForMissing)
	// 作者按 docs 写 `null`，但 YAML 会把它解析成空值。
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\npermission: L3\n"+
			"missing_policy: null\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("★ 裸 `missing_policy: null` 被 YAML 吞成空值后应被拒（否则静默退化成 skip，语义反转）")
	}
	if !strings.Contains(err.Error(), "YAML 空值") {
		t.Fatalf("★ 报错应点明这是「YAML 空值」陷阱（而非普通漏写），实际：%v", err)
	}
	if !strings.Contains(err.Error(), `"null"`) {
		t.Fatalf("★ 报错应给出「写带引号的 \\\"null\\\"」的可行动提示，实际：%v", err)
	}
}

// 反向：带引号的 `"null"` 才是能表达 null 策略的写法，必须通过。
func TestMissingPolicy_Wiring_AcceptsQuotedNull(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml", slotYAMLForMissing)
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nversion: 1\nformula: a\nunit: THB\npermission: L3\n"+
			"missing_policy: \"null\"\ndepends_on_slots: [slot.a]\n")

	if _, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms")); err != nil {
		t.Fatalf("带引号的 \"null\" 是合法 null 策略，不应被拒：%v", err)
	}
}

// TestMissingPolicy_Wiring_RealRepoYAML —— 真仓库的 16 槽 + 4 算法必须全部通过。
//
// ★ 这条最重要：它保证闸门不是「只在合成夹具上绿」。若有人把某个算法 YAML 的
// missing_policy 改成自由文本、或误写成裸 null，本用例立即失败。
func TestMissingPolicy_Wiring_RealRepoYAML(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	for _, sub := range []string{"slots", "algorithms"} {
		copyDir(t, filepath.Join(root, sub), filepath.Join(dir, sub))
	}
	if _, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms")); err != nil {
		t.Fatalf("真仓库 YAML 的缺失策略声明未通过 G4 第十三侧：%v", err)
	}
	// 夹具自证：确实复制到了文件（否则上面的加载可能因「空目录」而以另一种错失败）。
	if n := countYAML(t, filepath.Join(dir, "algorithms")); n != 4 {
		t.Fatalf("夹具自证失败：复制到 %d 个算法文件，期望 4", n)
	}
}

// ── 静态接线：钉住生产调用点真的存在（防「摘掉调用点、只留说明注释」）──────

func TestMissingPolicy_Wiring_ProductionCallSite(t *testing.T) {
	body := readMethodBody(t, "slot.go", "func (r *Registry) Validate() error {")
	// 判定的输入必须来自**磁盘真值**（MissingPolicyDocs），而不是空切片。
	if !strings.Contains(body, "r.MissingPolicyDocs()") {
		t.Fatal("★ Validate 里没调用 r.MissingPolicyDocs() ⇒ 判定输入不是真 YAML（可能是空切片，恒放行）")
	}
	if !strings.Contains(body, "gate.CheckMissingPolicyDeclared") {
		t.Fatal("★ Validate 里没调用 gate.CheckMissingPolicyDeclared ⇒ 闸门无生产调用点（恒真）")
	}
	// 夹具自证：扫描器必须真的读到了方法体，否则上面两条可能因 body 为空而「假绿」。
	if len(body) < 400 {
		t.Fatalf("夹具自证失败：Validate 方法体只读到 %d 字节，扫描器可能没命中签名", len(body))
	}
}

// TestMissingPolicy_Wiring_DocsSourceIsDiskTruth —— 钉住 MissingPolicyDocs
// 真的回看 YAML 原始键（Declared），否则「裸 null」与「漏写」会无法区分。
func TestMissingPolicy_Wiring_DocsSourceIsDiskTruth(t *testing.T) {
	body := readMethodBody(t, "slot.go", "func (r *Registry) MissingPolicyDocs() []gate.MissingPolicyDoc {")
	if !strings.Contains(body, "rawMissingPolicyText") {
		t.Fatal("★ MissingPolicyDocs 没走 rawMissingPolicyText ⇒ 拿不到「键是否存在」，无法区分裸 null 与漏写")
	}
	raw := readMethodBody(t, "slot.go", "func rawMissingPolicyText(a Algorithm) (string, bool) {")
	if !strings.Contains(raw, `a.Raw["missing_policy"]`) {
		t.Fatal("★ rawMissingPolicyText 没读 a.Raw[\"missing_policy\"] ⇒ 无法察觉 YAML 空值")
	}
}

// slotYAMLForMissing 是一个字段完整的合法槽夹具。
const slotYAMLForMissing = "id: slot.a\nname: A\nsource_kind: api\nsource_ref: x.api\n" +
	"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n"
