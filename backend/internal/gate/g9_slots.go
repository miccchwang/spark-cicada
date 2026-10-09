// G9 · 插槽闸门 —— 新模块零改主机即可挂载；能力不满足时降级不崩；可独立停用。
//
// docs/05 G9 三条断言：
//   1. 新模块零改主机即可挂载（集成测试：挂个示例模块）
//   2. 能力不满足时降级不崩（注入缺失槽，断言降级渲染）
//   3. 模块可独立停用（停用后其他模块正常）
//
// 本文件实现一个**最小可运行的模块注册表**，让上述断言可被真实执行，
// 而不是纸面承诺。设计对应 docs/02 的 SlotManifest 声明式挂载。
package gate

import (
	"fmt"
	"sort"
	"sync"
)

// ModuleManifest 模块清单（声明式；对应 slots/*.yaml 的模块段）。
//
// 关键：模块只**声明**自己需要哪些能力（Requires），
// 主机据此做能力协商（Capability Negotiation），无需为每个新模块改代码。
type ModuleManifest struct {
	ID       string
	Name     string
	Requires []string // 依赖的能力/槽 ID
	Optional []string // 可选依赖：缺失时降级而非失败
}

// Module 已挂载的模块运行态。
type Module struct {
	Manifest ModuleManifest
	Enabled  bool
	// Degraded 由能力协商置位：可选依赖缺失时为 true。
	Degraded bool
	// Missing 记录缺失的能力。
	Missing []string
}

// Registry 模块注册表（插槽主机）。
//
// 并发安全：挂载/停用可能来自管理面，与查询并发。
type Registry struct {
	mu        sync.RWMutex
	modules   map[string]*Module
	order     []string // 保持挂载顺序（渲染顺序稳定）
	available map[string]bool
}

// NewRegistry 构造；available 为当前已就绪的能力集合。
func NewRegistry(available []string) *Registry {
	set := map[string]bool{}
	for _, a := range available {
		set[a] = true
	}
	return &Registry{
		modules:   map[string]*Module{},
		available: set,
	}
}

// Mount 挂载一个模块。零改主机：只要 manifest 声明清楚了，挂载不需要改主机代码。
//
// 返回错误仅当**必需能力**缺失（硬失败）；可选能力缺失会将模块标记为 Degraded。
func (r *Registry) Mount(m ModuleManifest) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var missing, missingOptional []string
	for _, req := range m.Requires {
		if !r.available[req] {
			missing = append(missing, req)
		}
	}
	for _, opt := range m.Optional {
		if !r.available[opt] {
			missingOptional = append(missingOptional, opt)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("module %s: 必需能力缺失 %v（拒绝挂载，fail-closed）", m.ID, missing)
	}

	mod := &Module{
		Manifest: m,
		Enabled:  true,
		Degraded: len(missingOptional) > 0,
		Missing:  missingOptional,
	}
	if _, exists := r.modules[m.ID]; !exists {
		r.order = append(r.order, m.ID)
	}
	r.modules[m.ID] = mod
	return nil
}

// Disable 停用模块（独立停用；不影响其他模块）。
func (r *Registry) Disable(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.modules[id]; ok {
		m.Enabled = false
	}
}

// Enable 重新启用。
func (r *Registry) Enable(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.modules[id]; ok {
		m.Enabled = true
	}
}

// ActiveModules 返回当前启用（含降级）的模块，按挂载顺序。
func (r *Registry) ActiveModules() []Module {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Module
	for _, id := range r.order {
		m := r.modules[id]
		if m != nil && m.Enabled {
			out = append(out, *m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return indexOf(r.order, out[i].Manifest.ID) < indexOf(r.order, out[j].Manifest.ID)
	})
	return out
}

// RenderModules 模拟渲染：返回每个活跃模块的渲染描述（降级模块带标记）。
//
// 断言用途：降级模块**必须**仍能渲染（不崩），只是标注能力缺失。
func (r *Registry) RenderModules() []string {
	var out []string
	for _, m := range r.ActiveModules() {
		if m.Degraded {
			out = append(out, fmt.Sprintf("%s(degraded:missing=%v)", m.Manifest.ID, m.Missing))
		} else {
			out = append(out, m.Manifest.ID)
		}
	}
	return out
}

func indexOf(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}

// SetAvailable 重设「已就绪能力集」（能力协商的依据）。
//
// ★ 为什么需要它（2026-10-09 实测的真实缺口）：
//
//	能力集原先只在 NewRegistry 时**手抄 5 项**（`cap.core` / `slot.revenue` /
//	`slot.cogs` / `slot.platform_fee` / `slot.inventory_snap`），与真实数据槽注册表
//	（`slots/*.yaml`）**毫无关系**。而模块的 `Requires` 是**数据槽 ID** ⇒
//	清单里声明的 `data_slots`（slot.qty 等）永远不在能力集里，
//	能力协商**必然失败**、模块一个都挂不上 —— 且失败是静默的（只体现为
//	/api/admin/modules 返回空列表）。
//
//	现在由 sparkd 用 `slot.Registry.RegisteredSlotIDs()` 重设它，
//	让「主机能力」与「数据槽事实源」同源。
//
// 入参用「注册集合」（`map[string]bool`）而非切片：与
// RegisteredSlotIDs / RegisteredBucketIDs / RegisteredRuleIDs 同形态。
func (r *Registry) SetAvailable(caps map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := make(map[string]bool, len(caps))
	for c, ok := range caps {
		if ok && c != "" {
			set[c] = true
		}
	}
	r.available = set
}

// Available 返回当前「已就绪能力集」的副本。
//
// 用途：`slothost.Manifest.MountAll` 要在**一次性注册表**上先试挂一遍
// （见其注释：避免半挂载状态），试挂必须用与真挂**同一份能力集**，
// 故需要一个读取口。返回副本，防调用方改动内部状态。
func (r *Registry) Available() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]bool, len(r.available))
	for k, v := range r.available {
		out[k] = v
	}
	return out
}

// MountedIDs 返回已挂载模块的 ID（按挂载顺序）。
//
// 用途：`gate.CheckModuleManifestMatchesRegistry` 需要它做
// 「清单声明 ↔ 运行时注册表」的双向对平 —— 缺了它，
// 「清单声明了 6 个模块、运行时一个都没挂载」这种缺口无法被检出。
func (r *Registry) MountedIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, id)
	}
	return out
}
