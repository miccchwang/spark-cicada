// g9_manifest_test.go —— G9 插槽模块清单（SlotManifest）声明层闸门与对平闸门的测试。
//
// 被测对象：
//
//	gate.ParseModuleManifest                 解析（非法 YAML / 结构不匹配 / 空文件 ⇒ 错误）
//	gate.CheckModuleManifestDeclared         声明层：格式 / 词表 / 幽灵槽位 / 幽灵槽 / 引用完整性
//	gate.CheckModuleManifestMatchesRegistry  清单 ↔ 运行时注册表 **双向** 对平
//	gate.ModuleKinds / NegotiationOnMissingDataSlot   词表与 docs / 清单文件**同源**
//
// ★ 反向自证（本仓纪律）：每条断言都配一个「**一个错误的实现能不能通过它**」的反问
// —— 参 G4 第八侧教训（断言的结果对 ≠ 断言测的东西对），以及 G4 第十一/十三侧的
// 「夹具本身可能是错的」。
package gate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ─────────────────────────── 夹具 ───────────────────────────

// g9ManifestValidYAML 是一份**最小合法**清单（除被测字段外全部合规）。
//
// 用 YAML 文本而非结构体做夹具，是为了让「注入一处破坏」成为一次字符串替换 ——
// 这样负向用例改的是**清单原文**（用户真正会写的东西），而不是 Go 结构体
// （那会绕过解析层，测不到 ParseModuleManifest 的实际行为）。
const g9ManifestValidYAML = `manifestVersion: "1.0"
layoutSlots:
  - id: main.content
    desc: 主内容区
  - id: main.side
    desc: 侧边区
modules:
  - id: module.report
    name: 经营报表
    version: 1.0.0
    kind: page
    lang: typescript
    slot: main.content
    permissions: [L2, L3]
    data_slots: [slot.revenue, slot.cogs]
    produces: [DataContract]
  - id: module.filter
    name: 筛选与时间
    version: 1.0.0
    kind: panel
    lang: typescript
    slot: main.side
    permissions: [L1, L2, L3, L4]
    data_slots: []
    produces: [QueryState]
negotiation:
  onMissingDataSlot: degrade
  onMissingPermission: hide
  onVersionMismatch: block
  moduleIsolation: true
  independentlyDisableable: true
`

// g9FixtureSlots 是夹具清单引用到的数据槽（与 slots/*.yaml 的真实 ID 一致）。
func g9FixtureSlots() map[string]bool {
	return map[string]bool{"slot.revenue": true, "slot.cogs": true}
}

// g9ParseValid 解析夹具清单；解析失败即 Fatal（夹具自身坏了要立刻暴露）。
func g9ParseValid(t *testing.T) gate.ManifestDoc {
	t.Helper()
	doc, err := gate.ParseModuleManifest([]byte(g9ManifestValidYAML))
	if err != nil {
		t.Fatalf("夹具清单应能解析，实际报错：%v", err)
	}
	return doc
}

// g9FixtureIsDiscriminating 夹具自证：**合法夹具必须通过闸门**。
//
// 若夹具本身就违规，后面所有负向用例都会「因为别的原因」报警 ——
// 断言看着在测「注入的那一处」，实际测的是夹具自带的毛病。
func g9FixtureIsDiscriminating(t *testing.T) {
	t.Helper()
	if bad := gate.CheckModuleManifestDeclared(g9ParseValid(t), g9FixtureSlots()); len(bad) > 0 {
		t.Fatalf("★ 夹具自证失败：合法夹具不该有违规，实际：%v", bad)
	}
}

// g9RepoRoot 从测试文件位置向上找含 slots 目录的仓库根。
func g9RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd() // backend/internal/gate
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "slots")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("找不到仓库根（含 slots 的目录）")
	return ""
}

// g9RealRegisteredSlots 从**真实** slots/*.yaml 提取数据槽 ID。
//
// ★ 为什么在 gate 包里自己解析而不是 import internal/slot：
//
//	slot 包 import gate，本测试包若 import slot 会构成**循环**。
//	这里只做「取每份 YAML 的首个顶层 `id:`」这一件事，且后面有非空断言兜底
//	（空集合会让 data_slots 全部判为未注册 ⇒ 用例会红，而不是静默恒过）。
func g9RealRegisteredSlots(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(g9RepoRoot(t), "slots", "*.yaml"))
	if err != nil {
		t.Fatalf("glob slots/*.yaml 失败：%v", err)
	}
	if len(paths) < 10 {
		t.Fatalf("★ 真实数据槽不足 10 个（实际 %d）—— 夹具/路径可疑，拒绝用弱集合做断言", len(paths))
	}
	out := map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", p, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "id:") {
				continue
			}
			v := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			if i := strings.Index(v, "#"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			if v != "" {
				out[v] = true
			}
			break
		}
	}
	return out
}

// ─────────────────────────── 真实文件 ───────────────────────────

// TestG9Manifest_RealFilePassesDeclaredGate 断言仓库里**真实的**清单通过声明层闸门。
//
// 这是本闸门的「正向基线」：若真实清单本身违规，说明清单或闸门有一处错了 ——
// 必须当场暴露，而不是等到 sparkd 启动时才 warn。
func TestG9Manifest_RealFilePassesDeclaredGate(t *testing.T) {
	raw := repoFile(t, "contracts/slot-manifest.yaml")
	doc, err := gate.ParseModuleManifest([]byte(raw))
	if err != nil {
		t.Fatalf("真实 SlotManifest 解析失败：%v", err)
	}
	slots := g9RealRegisteredSlots(t)
	if bad := gate.CheckModuleManifestDeclared(doc, slots); len(bad) > 0 {
		t.Fatalf("真实 SlotManifest 未通过声明层闸门：\n  %s", strings.Join(bad, "\n  "))
	}

	// 防「把清单删空也能过」：真实清单声明了 6 个模块、5 个布局槽位。
	if len(doc.Modules) != 6 {
		t.Errorf("真实清单应声明 6 个模块，实际 %d 个 —— 清单被改动请同步本断言", len(doc.Modules))
	}
	if len(doc.LayoutSlots) != 5 {
		t.Errorf("真实清单应声明 5 个布局槽位，实际 %d 个", len(doc.LayoutSlots))
	}
	want := map[string]bool{
		"module.filter": true, "module.report": true, "module.pnl": true,
		"module.strategy": true, "module.template": true, "module.admin": true,
	}
	for _, m := range doc.Modules {
		if !want[m.ID] {
			t.Errorf("真实清单出现了未预期的模块 %q", m.ID)
		}
		delete(want, m.ID)
	}
	if len(want) > 0 {
		t.Errorf("真实清单缺少模块：%v", want)
	}
}

// TestG9Manifest_VocabularyMatchesSources 断言两张词表与**它们的权威来源逐字同源**。
//
// ★ 为什么单列一条：词表若在 gate 与 docs / 清单文件里各写一份，迟早漂移
// （本仓已有 unit（THB vs CNY）、规则版本（2 vs 6）等同型事故）。
func TestG9Manifest_VocabularyMatchesSources(t *testing.T) {
	// ① 模块类型 ← docs/01 §8.1 的 `kind: page  # page | panel | widget | background-job`
	docs01 := repoFile(t, "docs/01-开发文档.md")
	wantKinds := "# " + strings.Join(gate.ModuleKinds(), " | ")
	if !strings.Contains(docs01, wantKinds) {
		t.Errorf("ModuleKinds 与 docs/01 §8.1 分叉：docs 里找不到 %q（实际词表 %v）",
			wantKinds, gate.ModuleKinds())
	}
	// ② 缺数据槽处理 ← contracts/slot-manifest.yaml 的 `# degrade | hide | error`
	manifest := repoFile(t, "contracts/slot-manifest.yaml")
	wantNeg := "# " + strings.Join(gate.NegotiationOnMissingDataSlot(), " | ")
	if !strings.Contains(manifest, wantNeg) {
		t.Errorf("NegotiationOnMissingDataSlot 与清单文件分叉：文件里找不到 %q（实际词表 %v）",
			wantNeg, gate.NegotiationOnMissingDataSlot())
	}
}

// ─────────────────────────── 解析层 ───────────────────────────

func TestG9Manifest_ParseRejectsUnparseable(t *testing.T) {
	cases := []struct{ name, yaml string }{
		{"空文件", ""},
		{"全空白", "   \n\n\t\n"},
		{"非法 YAML", "manifestVersion: \"1.0\"\n  modules: [oops"},
		{"modules 是标量而非列表", "manifestVersion: \"1.0\"\nmodules: 3\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := gate.ParseModuleManifest([]byte(c.yaml)); err == nil {
				t.Fatalf("应报错但通过了：%q", c.yaml)
			}
		})
	}
}

// TestG9Manifest_UnquotedScalarIsAcceptedAsText 把一条**否定结论**固化下来。
//
// ★ 为什么要有这条「测试一个非缺陷」的用例：
//
//	最初我（本 automation）以为 `manifestVersion: 1.0` 未加引号会被 yaml.v3
//	解成浮点数而**静默丢字段**，并在注释里这么写了。实测**并非如此**：
//	yaml.v3 把任意标量解进 `string` 字段时保留原文（`1.0` → `"1.0"`）。
//	若不把否定结论钉住，日后有人（或另一个我）会把它当成真问题去「修」——
//	而修一个不存在的问题，往往就是引入真问题的方式。
//
// 纪律同 G4 第八侧：**宁少一条真断言，不写一条假断言**。
func TestG9Manifest_UnquotedScalarIsAcceptedAsText(t *testing.T) {
	doc, err := gate.ParseModuleManifest([]byte("manifestVersion: 1.0\nmodules: []\n"))
	if err != nil {
		t.Fatalf("未加引号的标量不应解析失败：%v", err)
	}
	if doc.ManifestVersion != "1.0" {
		t.Fatalf("未加引号的标量应保留原文 \"1.0\"，实际 %q", doc.ManifestVersion)
	}
	if !doc.HasManifestVersion {
		t.Fatal("HasManifestVersion 应为 true（键确实存在）")
	}
}

// ─────────────────────────── 声明层负向 ───────────────────────────

func TestG9Manifest_DeclaredGateNegatives(t *testing.T) {
	cases := []struct {
		name    string
		old     string
		new     string
		wantSub string
	}{
		{"缺 manifestVersion", "manifestVersion: \"1.0\"\n", "", "manifestVersion"},
		{"manifestVersion 不可解析", `manifestVersion: "1.0"`, `manifestVersion: "1.x"`, "不可解析"},
		{"无 layoutSlots", "layoutSlots:\n  - id: main.content\n    desc: 主内容区\n  - id: main.side\n    desc: 侧边区\n", "", "layoutSlots"},
		{"layoutSlot id 重复", "  - id: main.side\n    desc: 侧边区\n", "  - id: main.content\n    desc: 重复\n", "重复声明"},
		{"幽灵槽位", "    slot: main.side\n", "    slot: main.ghost\n", "幽灵槽位"},
		{"幽灵数据槽", "data_slots: [slot.revenue, slot.cogs]", "data_slots: [slot.revenue, slot.ghost]", "幽灵引用"},
		{"module id 重复", "  - id: module.filter\n", "  - id: module.report\n", "重复声明"},
		{"kind 不在词表", "    kind: panel\n", "    kind: plugin\n", "不在词表"},
		{"version 非三段式", "    version: 1.0.0\n    kind: panel", "    version: 1.0\n    kind: panel", "不可解析"},
		{"未声明 permissions", "    permissions: [L1, L2, L3, L4]\n", "", "permissions"},
		{"未声明 produces", "    produces: [QueryState]\n", "", "produces"},
		{"缺 negotiation", "negotiation:\n", "negotiations:\n", "negotiation"},
		{"onMissingDataSlot 非法", "onMissingDataSlot: degrade", "onMissingDataSlot: explode", "不在词表"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g9FixtureIsDiscriminating(t)
			if !strings.Contains(g9ManifestValidYAML, c.old) {
				t.Fatalf("★ 夹具与用例脱节：夹具里找不到待替换串 %q", c.old)
			}
			mutated := strings.Replace(g9ManifestValidYAML, c.old, c.new, 1)
			doc, err := gate.ParseModuleManifest([]byte(mutated))
			if err != nil {
				// 解析层直接拦下也算拦下（例如结构被改坏），但要求原因可读。
				if strings.TrimSpace(err.Error()) == "" {
					t.Fatal("解析失败但错误信息为空")
				}
				return
			}
			bad := gate.CheckModuleManifestDeclared(doc, g9FixtureSlots())
			if len(bad) == 0 {
				t.Fatalf("★ 注入「%s」后闸门仍全绿 —— 该断言恒真", c.name)
			}
			joined := strings.Join(bad, "；")
			if !strings.Contains(joined, c.wantSub) {
				t.Fatalf("拦下了但原因不含 %q：%s", c.wantSub, joined)
			}
		})
	}
}

// ─────────────────────────── 对平层（双向） ───────────────────────────

func TestG9Manifest_MatchesRegistryBothDirections(t *testing.T) {
	doc := g9ParseValid(t)

	// ① 一致 ⇒ 无违规
	if bad := gate.CheckModuleManifestMatchesRegistry(doc, []string{"module.report", "module.filter"}); len(bad) > 0 {
		t.Fatalf("清单与注册表一致时不应有违规：%v", bad)
	}

	// ② 清单声明了、运行时没挂载（★ 本次抓到的真实缺口形态）
	bad := gate.CheckModuleManifestMatchesRegistry(doc, []string{"module.report"})
	if len(bad) == 0 {
		t.Fatal("★ 漏检：清单声明了 module.filter 却未挂载，应报警")
	}
	if !strings.Contains(strings.Join(bad, "；"), "module.filter") {
		t.Fatalf("报警未点名缺失模块：%v", bad)
	}

	// ③ 运行时挂了、清单没声明（清单失去约束力）
	bad = gate.CheckModuleManifestMatchesRegistry(doc, []string{"module.report", "module.filter", "module.rogue"})
	if len(bad) == 0 {
		t.Fatal("★ 漏检：运行时挂载了清单未声明的 module.rogue，应报警")
	}
	if !strings.Contains(strings.Join(bad, "；"), "module.rogue") {
		t.Fatalf("报警未点名越权模块：%v", bad)
	}

	// ④ 空注册表 + 非空清单 ⇒ 必须全量报警（防「空对空恒过」）
	if bad := gate.CheckModuleManifestMatchesRegistry(doc, nil); len(bad) != 2 {
		t.Fatalf("空注册表应对 2 个已声明模块各报一条，实际 %d 条：%v", len(bad), bad)
	}
}
