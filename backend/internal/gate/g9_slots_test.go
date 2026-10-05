// g9_slots_test.go —— G9 插槽闸门测试。
package gate_test

import (
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// 挂载一个示例模块，**不改主机代码** → 断言通过（G9-1）。
func TestG9_ZeroHostChangeMount(t *testing.T) {
	reg := gate.NewRegistry([]string{"slot.revenue", "slot.cogs"})
	m := gate.ModuleManifest{
		ID:       "m.example",
		Name:     "示例模块",
		Requires: []string{"slot.revenue"},
	}
	if err := reg.Mount(m); err != nil {
		t.Fatalf("G9 零改主机挂载失败: %v", err)
	}
	if got := reg.RenderModules(); len(got) != 1 || got[0] != "m.example" {
		t.Fatalf("G9 挂载后渲染异常: %v", got)
	}
}

// 必需能力缺失 ⇒ 硬失败（fail-closed），不静默降级。
func TestG9_MissingRequiredCapabilityFails(t *testing.T) {
	reg := gate.NewRegistry([]string{"slot.revenue"})
	m := gate.ModuleManifest{
		ID:       "m.needs_affiliate",
		Requires: []string{"slot.affiliate"},
	}
	if err := reg.Mount(m); err == nil {
		t.Fatal("G9 必需能力缺失时应拒绝挂载")
	}
}

// 可选能力缺失 ⇒ 降级但**不崩**（G9-2）。
func TestG9_MissingOptionalDegradesNotCrash(t *testing.T) {
	reg := gate.NewRegistry([]string{"slot.revenue"})
	m := gate.ModuleManifest{
		ID:       "m.optional",
		Requires: []string{"slot.revenue"},
		Optional: []string{"slot.affiliate"},
	}
	if err := reg.Mount(m); err != nil {
		t.Fatalf("G9 可选缺失不应导致挂载失败: %v", err)
	}
	mods := reg.ActiveModules()
	if len(mods) != 1 || !mods[0].Degraded {
		t.Fatalf("G9 应标记为 degraded: %+v", mods)
	}
	// 关键：降级模块仍能渲染（不崩）
	rendered := reg.RenderModules()
	if len(rendered) != 1 {
		t.Fatalf("G9 降级模块应仍可渲染: %v", rendered)
	}
}

// 模块可独立停用，其他模块不受影响（G9-3）。
func TestG9_IndependentDisable(t *testing.T) {
	reg := gate.NewRegistry([]string{"slot.revenue", "slot.cogs", "slot.affiliate"})
	_ = reg.Mount(gate.ModuleManifest{ID: "m.a", Requires: []string{"slot.revenue"}})
	_ = reg.Mount(gate.ModuleManifest{ID: "m.b", Requires: []string{"slot.cogs"}})
	_ = reg.Mount(gate.ModuleManifest{ID: "m.c", Requires: []string{"slot.affiliate"}})

	reg.Disable("m.b")
	got := reg.RenderModules()
	if len(got) != 2 {
		t.Fatalf("G9 停用后应剩 2 个模块，实际 %v", got)
	}
	for _, id := range got {
		if id == "m.b" {
			t.Fatal("G9 m.b 已停用却仍被渲染")
		}
	}
}
