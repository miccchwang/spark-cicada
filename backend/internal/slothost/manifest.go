// Package slothost —— M-SLOT 插槽注册中心的**宿主侧**实现。
//
// 缺口（2026-10-09 实测）：
//
//	`contracts/slot-manifest.yaml` 自称「新功能模块通过本清单自注册；主机**零改代码**
//	即可挂载」（docs/01 §8.1 / docs/02 M-SLOT「契约 SlotManifest（YAML）」），
//	但 `grep "slot-manifest"` 全仓（.go/.ts/.js/.mjs/.yml）**零命中** ——
//	这份清单**从没被任何代码读过**（承袭「定义文件全仓从没被任何代码读过」这个病）。
//	于是：
//	  * 删掉整个文件 / 写成任意内容 ⇒ sparkd 照常启动，无人报错；
//	  * `gate.Registry`（运行时插槽主机）启动时**一个模块都没挂载** ⇒
//	    `/api/admin/modules` 恒返回 `{"rendered":[],"modules":[]}`，
//	    而清单里明明声明了 6 个模块（filter/report/pnl/strategy/template/admin）；
//	  * 主机能力集是**手抄的 5 项**，与真实数据槽注册表无关 ⇒
//	    清单声明的 `data_slots` 永远无法满足，能力协商必然失败。
//
// 本包把这份清单读**真文件**、过**真闸门**、真挂载：
//
//	LoadManifest(path, registeredSlots)
//	  → gate.ParseModuleManifest          解析（保留原始键，供存在性判定）
//	  → gate.CheckModuleManifestDeclared  声明层：格式/词表/幽灵槽位/幽灵槽/引用完整性
//	  → 返回可挂载的 []gate.ModuleManifest（Requires = 清单声明的 data_slots）
//	Manifest.MountAll(reg)
//	  → 逐个 reg.Mount                    能力协商：缺能力即拒（fail-closed）
//	  → gate.CheckModuleManifestMatchesRegistry
//	                                      清单 ↔ 运行时注册表 **双向** 对平
//
// 分工纪律（与本仓既有包一致）：`gate` 只判真假（纯函数，不读文件、不连库）；
// 真实文件 IO 与「生产调用点」由本包承担。
package slothost

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// Manifest 是一份已装载、已过闸门的 SlotManifest。
type Manifest struct {
	// Version 是清单格式版本（`manifestVersion`）。
	Version string
	// Modules 是可直接交给 gate.Registry 挂载的模块清单。
	Modules []gate.ModuleManifest
	// Doc 是解析后的原始文档（供双向对平与排障）。
	Doc gate.ManifestDoc
}

// LoadManifest 读取并校验一份 SlotManifest。
//
// registeredSlots = 真实数据槽注册表（`slot.Registry.RegisteredSlotIDs()`，
// 本仓统一的「注册集合」形态），用于 `data_slots` 的引用完整性校验。
//
// fail-closed：
//   - 文件不存在 / 读失败 ⇒ error；
//   - 空文件（或全空白）⇒ error —— **空清单必须报错**，绝不静默「零模块」；
//   - 非法 YAML / 结构不匹配 ⇒ error；
//   - 声明层闸门有违规 ⇒ error（附全部人类可读原因）。
//
// ★ 为什么空清单必须报错而不是「无害地返回空」：
//
//	本包存在的全部理由，就是「清单声明了模块却零挂载」这件事此前**静默无感**。
//	若这里把空文件当成「没有模块要挂」放行，等于把同一个病换个位置复发。
func LoadManifest(path string, registeredSlots map[string]bool) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 SlotManifest 失败（%s）：%w", path, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("SlotManifest 为空文件（%s）—— 空清单必须报错，绝不静默零模块", path)
	}
	doc, err := gate.ParseModuleManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("%s：%w", path, err)
	}
	if bad := gate.CheckModuleManifestDeclared(doc, registeredSlots); len(bad) > 0 {
		return nil, fmt.Errorf("SlotManifest 声明层闸门未通过（%s）：%s", path, strings.Join(bad, "；"))
	}
	mods := make([]gate.ModuleManifest, 0, len(doc.Modules))
	for _, m := range doc.Modules {
		requires := make([]string, 0, len(m.DataSlots))
		for _, ds := range m.DataSlots {
			if v := strings.TrimSpace(ds); v != "" {
				requires = append(requires, v)
			}
		}
		mods = append(mods, gate.ModuleManifest{
			ID:       strings.TrimSpace(m.ID),
			Name:     strings.TrimSpace(m.Name),
			Requires: requires,
		})
	}
	return &Manifest{
		Version: strings.TrimSpace(doc.ManifestVersion),
		Modules: mods,
		Doc:     doc,
	}, nil
}

// MountAll 把清单里的模块逐个挂载到 reg，并做「清单 ↔ 运行时注册表」双向对平。
//
// ★ 这是 M-SLOT 三条验收（docs/02）在实现侧的唯一落点：
//
//	「新模块零改主机代码即可挂载」—— 挂载只依赖清单声明，主机不写模块专属代码；
//	「能力不满足时降级而非崩溃」—— 必需能力缺失由 reg.Mount 拒绝（fail-closed），
//	                              可选能力缺失由 reg.Mount 标 Degraded；
//	「IT 可停用任一模块」          —— 挂载后由 reg.Disable / admin.Plane 负责。
//
// ★ **原子性**（本包自己的行为级用例逼出来的，非事后补写）：
//
//	先在**一次性注册表**上试挂全部模块，全部成功才真挂 —— 否则返回 error
//	且**一个都不挂**。原因是「半挂载」比「全失败」更难排查：
//	接口看起来"有几个模块"（`/api/admin/modules` 非空），实则残缺，
//	而 healthz 只报一条"挂载失败"，人很容易以为只是少了一个。
//	试挂**复用 reg.Mount 这同一条规则**（不另写一份能力判定 ——
//	两处各写一份判定，迟早分叉，本仓已反复踩过这个坑）。
//
// 任一步失败 ⇒ 返回 error（由调用方进 healthz，不静默）。
func (m *Manifest) MountAll(reg *gate.Registry) error {
	if m == nil {
		return errors.New("slothost: 清单为 nil（未装载就调用挂载）")
	}
	if reg == nil {
		return errors.New("slothost: 模块注册表为 nil（主机不可用，不得假装挂载成功）")
	}
	// ① 试挂（同一份能力集，一次性注册表）
	probe := gate.NewRegistry(nil)
	probe.SetAvailable(reg.Available())
	for _, mm := range m.Modules {
		if err := probe.Mount(mm); err != nil {
			return fmt.Errorf("挂载模块 %s 失败（已整体拒绝，不留半挂载状态）：%w", mm.ID, err)
		}
	}
	// ② 真挂
	for _, mm := range m.Modules {
		if err := reg.Mount(mm); err != nil {
			return fmt.Errorf("挂载模块 %s 失败：%w", mm.ID, err)
		}
	}
	if bad := gate.CheckModuleManifestMatchesRegistry(m.Doc, reg.MountedIDs()); len(bad) > 0 {
		return fmt.Errorf("清单与运行时注册表不一致：%s", strings.Join(bad, "；"))
	}
	return nil
}
