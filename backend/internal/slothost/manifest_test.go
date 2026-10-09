// manifest_test.go —— M-SLOT 宿主侧（装载 + 挂载）的**行为级**测试。
//
// ★ 为什么必须是行为级（从真实入口进）而不是只做静态扫描：
//
//	本仓实测过（R-20261008-03）：静态扫描只能证明「调用点还在」，
//	**证明不了「入参还是对的」**。摘掉一处映射（不是调用点）后静态扫描照过、
//	真实数据也照过 —— 因为被传进去的集合恒为空，闸门恒放行（假绿）。
//	故每个判定函数除了「有生产调用点」，还要有「从真实路径进、以真实结果出」的用例。
package slothost_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
	"github.com/miccchwang/spark-cicada/backend/internal/slothost"
)

// realSlots 装载**真实**数据槽注册表（事实源），供装载/挂载用例使用。
func realSlots(t *testing.T) *slot.Registry {
	t.Helper()
	reg, err := slot.LoadRegistry(
		filepath.Join("..", "..", "..", "slots"),
		filepath.Join("..", "..", "..", "algorithms"))
	if err != nil {
		t.Fatalf("装载真实数据槽注册表失败：%v", err)
	}
	return reg
}

// realManifestPath 是仓库里真实的 SlotManifest 路径。
func realManifestPath() string {
	return filepath.Join("..", "..", "..", "contracts", "slot-manifest.yaml")
}

// TestG9Manifest_RealFileLoadsAndMounts 是 M-SLOT 的**端到端正向基线**：
// 真实清单 + 真实数据槽注册表 + 真实模块注册表 ⇒ 6 个模块全部挂载成功。
func TestG9Manifest_RealFileLoadsAndMounts(t *testing.T) {
	slots := realSlots(t)
	ids := slots.RegisteredSlotIDs()

	man, err := slothost.LoadManifest(realManifestPath(), ids)
	if err != nil {
		t.Fatalf("真实 SlotManifest 装载失败：%v", err)
	}
	if len(man.Modules) != 6 {
		t.Fatalf("真实清单应装载 6 个模块，实际 %d 个", len(man.Modules))
	}

	reg := gate.NewRegistry(nil)
	reg.SetAvailable(ids) // ★ 能力集来自**真实槽注册表**（唯一事实源）
	if err := man.MountAll(reg); err != nil {
		t.Fatalf("真实清单挂载失败：%v", err)
	}

	// 接口层看到的东西：/api/admin/modules 的 rendered/modules 必须非空。
	rendered := reg.RenderModules()
	if len(rendered) != 6 {
		t.Fatalf("挂载后应有 6 个模块可渲染（/api/admin/modules 非空），实际 %d 个：%v",
			len(rendered), rendered)
	}
	mounted := map[string]bool{}
	for _, id := range reg.MountedIDs() {
		mounted[id] = true
	}
	for _, want := range []string{
		"module.filter", "module.report", "module.pnl",
		"module.strategy", "module.template", "module.admin",
	} {
		if !mounted[want] {
			t.Errorf("清单声明了 %s，但挂载后不在注册表里", want)
		}
	}
}

// TestG9Manifest_CapabilitySetMustComeFromRealSlots 是上一条的**判别性对照**。
//
// ★ 这条用例回答的问题是：「能力集来自真实槽注册表」到底是不是必需的？
//
//	若能力集仍是旧的手抄 5 项（或空集），清单里声明的 `data_slots`
//	（slot.qty / slot.stock / slot.ad_spend 等）就不在能力集里 ⇒
//	能力协商**必然失败**。本用例把该结论钉死：空能力集下挂载**必须**失败。
//	（若哪天有人把 Mount 的能力校验去掉，本用例会红。）
func TestG9Manifest_CapabilitySetMustComeFromRealSlots(t *testing.T) {
	ids := realSlots(t).RegisteredSlotIDs()
	man, err := slothost.LoadManifest(realManifestPath(), ids)
	if err != nil {
		t.Fatalf("真实 SlotManifest 装载失败：%v", err)
	}

	// ① 空能力集（旧实现的近似）⇒ 必须失败，且原因点名缺哪个能力。
	empty := gate.NewRegistry(nil)
	err = man.MountAll(empty)
	if err == nil {
		t.Fatal("★ 能力集为空时挂载竟然成功 —— 说明能力协商形同虚设")
	}
	if !strings.Contains(err.Error(), "必需能力缺失") {
		t.Fatalf("失败原因应点名缺失的能力，实际：%v", err)
	}
	if len(empty.MountedIDs()) != 0 {
		t.Fatalf("挂载失败时不应留下半挂载状态，实际 %v", empty.MountedIDs())
	}

	// ② 只给手抄的旧 5 项 ⇒ 仍必须失败（清单声明了 slot.qty 等真实槽）。
	legacy := gate.NewRegistry(nil)
	legacy.SetAvailable(map[string]bool{
		"cap.core": true, "slot.revenue": true, "slot.cogs": true,
		"slot.platform_fee": true, "slot.inventory_snap": true,
	})
	if err := man.MountAll(legacy); err == nil {
		t.Fatal("★ 旧的手抄能力集竟然能让清单挂载成功 —— 说明清单的 data_slots 未被真正核对")
	}

	// ③ 给真实槽注册表 ⇒ 成功（对照）。
	ok := gate.NewRegistry(nil)
	ok.SetAvailable(ids)
	if err := man.MountAll(ok); err != nil {
		t.Fatalf("真实能力集下应挂载成功，实际：%v", err)
	}
}

// g9WriteTemp 把清单内容写到临时文件并返回路径。
func g9WriteTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "slot-manifest.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// g9ValidManifest 是一份最小合法清单（与 gate 侧夹具同构，但独立一份：
// 本包测试的是**装载器**，不该依赖 gate 包的测试夹具）。
const g9ValidManifest = `manifestVersion: "1.0"
layoutSlots:
  - id: main.content
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
negotiation:
  onMissingDataSlot: degrade
  onMissingPermission: hide
  onVersionMismatch: block
`

// TestG9Manifest_LoadFailClosed 断言装载器在**每一种残破输入**下都 fail-closed。
func TestG9Manifest_LoadFailClosed(t *testing.T) {
	ids := realSlots(t).RegisteredSlotIDs()
	base := realManifestPath()

	cases := []struct {
		name    string
		path    string
		wantSub string
	}{
		{"文件不存在", filepath.Join("..", "..", "..", "contracts", "no-such-manifest.yaml"), "读 SlotManifest 失败"},
		{"空文件", g9WriteTemp(t, ""), "为空文件"},
		{"全空白", g9WriteTemp(t, "\n   \n\t\n"), "为空文件"},
		{"幽灵数据槽", g9WriteTemp(t, strings.Replace(g9ValidManifest,
			"data_slots: [slot.revenue, slot.cogs]", "data_slots: [slot.ghost]", 1)), "幽灵引用"},
		{"幽灵槽位", g9WriteTemp(t, strings.Replace(g9ValidManifest,
			"slot: main.content", "slot: main.ghost", 1)), "幽灵槽位"},
		{"kind 非法", g9WriteTemp(t, strings.Replace(g9ValidManifest,
			"kind: page", "kind: plugin", 1)), "不在词表"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 夹具自证：同一份内容修好之后必须能装载（证明上面拦的确实是那一处）。
			if _, err := slothost.LoadManifest(base, ids); err != nil {
				t.Fatalf("★ 真实清单本该可装载，先坏了：%v", err)
			}
			man, err := slothost.LoadManifest(c.path, ids)
			if err == nil {
				t.Fatalf("应报错但装载成功（%d 个模块）", len(man.Modules))
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("拦下了但原因不含 %q：%v", c.wantSub, err)
			}
		})
	}

	// ★ 夹具自证（把「修好即通过」这一半也测掉，防止负向用例「因为别的原因」报警）
	fixed := g9WriteTemp(t, g9ValidManifest)
	if _, err := slothost.LoadManifest(fixed, ids); err != nil {
		t.Fatalf("★ 夹具自证失败：合法清单应能装载，实际：%v", err)
	}
}

// TestG9Manifest_MountAllCatchesRogueModule 断言对平闸门真的接在挂载路径上：
// 运行时注册表里多挂了一个**清单未声明**的模块 ⇒ MountAll 必须报警。
func TestG9Manifest_MountAllCatchesRogueModule(t *testing.T) {
	ids := realSlots(t).RegisteredSlotIDs()
	man, err := slothost.LoadManifest(realManifestPath(), ids)
	if err != nil {
		t.Fatalf("真实 SlotManifest 装载失败：%v", err)
	}
	reg := gate.NewRegistry(nil)
	reg.SetAvailable(ids)
	if err := reg.Mount(gate.ModuleManifest{ID: "module.rogue", Name: "未声明模块"}); err != nil {
		t.Fatalf("预置越权模块失败：%v", err)
	}
	err = man.MountAll(reg)
	if err == nil {
		t.Fatal("★ 漏检：运行时挂了清单未声明的 module.rogue，MountAll 应报警")
	}
	if !strings.Contains(err.Error(), "module.rogue") {
		t.Fatalf("报警未点名越权模块：%v", err)
	}
}

// TestG9Manifest_MountAllRejectsNilRegistry 断言「主机不可用」不得假装挂载成功。
func TestG9Manifest_MountAllRejectsNilRegistry(t *testing.T) {
	ids := realSlots(t).RegisteredSlotIDs()
	man, err := slothost.LoadManifest(realManifestPath(), ids)
	if err != nil {
		t.Fatalf("真实 SlotManifest 装载失败：%v", err)
	}
	if err := man.MountAll(nil); err == nil {
		t.Fatal("注册表为 nil 时应报错（不得假装挂载成功）")
	}
	var nilMan *slothost.Manifest
	if err := nilMan.MountAll(gate.NewRegistry(nil)); err == nil {
		t.Fatal("清单为 nil 时应报错")
	}
}
