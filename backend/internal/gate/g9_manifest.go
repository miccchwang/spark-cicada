// G9 · 插槽模块清单（SlotManifest）的**声明层**闸门。
//
// 缺口（2026-10-09 实测）：
//
//	`contracts/slot-manifest.yaml` 自称「新功能模块通过本清单自注册；主机**零改代码**
//	即可挂载」（docs/01 §8.1 模块清单 / docs/02 M-SLOT「契约 SlotManifest（YAML）」），
//	但 `grep "slot-manifest"` 全仓（.go/.ts/.js/.mjs/.yml）**零命中** ——
//	这份清单**从没被任何代码读过**。承袭本仓「定义文件全仓从没被任何代码读过」
//	这个病（第九/十/十二/十三变种的邻域），后果不是「少一条断言」而是：
//	  * 清单可被整体删除、可被写成任意内容，而 sparkd 照常启动、无人报错；
//	  * `gate.Registry`（运行时插槽主机）启动时**零模块挂载** ⇒
//	    `/api/admin/modules` 恒返回 `{"rendered":[],"modules":[]}`，
//	    而清单里明明声明了 6 个模块（filter/report/pnl/strategy/template/admin）；
//	  * 主机能力集是**手抄的 5 项**（`cap.core` / `slot.revenue` …），
//	    与真实数据槽注册表无关 ⇒ 清单声明的 `data_slots`（slot.qty 等）
//	    永远不在能力集里，能力协商**必然失败**。
//
// 本文件只做**纯判定**（不读文件、不连库），文件 IO 与挂载在 `internal/slothost`。
// 分工与本仓既有纪律一致：`gate` 判真假，`slot`/`rule`/`slothost` 提供真实生产调用点。
//
// 校验口径（与 CheckPermissionDeclared / CheckFreshnessDeclared 同构）：
//
//	声明层 —— 格式可解析、词表受控、**引用完整性**（幽灵槽 / 幽灵槽位一律拒）；
//	对平层 —— 清单声明的模块 ↔ 运行时注册表**双向**一致（缺一侧即「声明等于装饰」）。
//
// ★ 刻意**不做**的两件事（纪律：宁少一条真断言，不写一条假断言）：
//
//  1. **不**断言模块的 `permissions` ∈ L1–L4。清单里 `module.admin` 写的是
//     `permissions: [L0]`（附注「IT 只见元数据，不见业务数值」），而 docs/01 §13.3
//     的**密级**词表是 L1–L4（L0 在 docs 里指**阅览层级/总览**，是另一个维度）。
//     两者是「密级」还是「阅览层级」语义未定 ⇒ 强行断言只能靠改数据迁就未经验证的
//     规则（参 G4 第八侧「密级单调性」被证伪的教训）。已登记 docs/06 待拍板。
//  2. **不**断言 `upgrade.minHostVersion ≤ manifestVersion`。前者是「最低主机版本」，
//     后者是「清单格式版本」，二者是否同一坐标系在 docs 里没有定义 ⇒ 只断言
//     「可解析」，不断言大小关系。
package gate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// moduleKinds 是模块类型词表，**唯一权威**。
//
// 依据：docs/01 §8.1 的清单示例 `kind: page  # page | panel | widget | background-job`。
var moduleKinds = []string{"page", "panel", "widget", "background-job"}

// ModuleKinds 返回允许的模块类型（副本，防调用方改动权威表）。
func ModuleKinds() []string {
	out := make([]string, len(moduleKinds))
	copy(out, moduleKinds)
	return out
}

// negotiationOnMissingDataSlot 是「缺数据槽」的处理词表，**唯一权威**。
//
// 依据：contracts/slot-manifest.yaml 的 `onMissingDataSlot: degrade  # degrade | hide | error`。
var negotiationOnMissingDataSlot = []string{"degrade", "hide", "error"}

// NegotiationOnMissingDataSlot 返回「缺数据槽」的允许取值（副本）。
func NegotiationOnMissingDataSlot() []string {
	out := make([]string, len(negotiationOnMissingDataSlot))
	copy(out, negotiationOnMissingDataSlot)
	return out
}

var (
	// manifestVersionRe 匹配清单格式版本（`1.0` / `1` / `1.2.3`）。
	manifestVersionRe = regexp.MustCompile(`^\d+(\.\d+)*$`)
	// moduleVersionRe 匹配模块版本（docs/01 §8.1 示例为三段式 `2.1.0`）。
	moduleVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
)

// LayoutSlotDoc 是一个布局槽位声明（`layoutSlots[]`）。
type LayoutSlotDoc struct {
	ID   string `yaml:"id"`
	Desc string `yaml:"desc"`
}

// LayoutDoc 是模块的挂载位置（docs/01 §8.1 用 `layout: { slot: ... }` 嵌套形态）。
//
// ★ 为什么两种形态都认：docs/01 §8.1 的示例是 `layout.slot`，而仓库里真实的
// `contracts/slot-manifest.yaml` 写的是**扁平** `slot:` —— 这是文档与文件之间的
// 一处真实漂移（此前无人读该文件，故从未暴露）。本闸门**不替它做决定**
// （不臆断谁对谁错），两种写法都接受；漂移本身如实登记 docs/06。
type LayoutDoc struct {
	Slot string `yaml:"slot"`
}

// NavDoc 是导航声明（`nav: { group, order, icon }`）。
type NavDoc struct {
	Group string `yaml:"group"`
	Order int    `yaml:"order"`
	Icon  string `yaml:"icon"`
}

// UpgradeDoc 是升级声明（docs/01 §8.1）。
type UpgradeDoc struct {
	MinHostVersion string   `yaml:"minHostVersion"`
	Migrations     []string `yaml:"migrations"`
}

// ModuleDoc 是一条模块声明（`modules[]`）。
type ModuleDoc struct {
	ID          string      `yaml:"id"`
	Name        string      `yaml:"name"`
	Version     string      `yaml:"version"`
	Kind        string      `yaml:"kind"`
	Lang        string      `yaml:"lang"`
	Slot        string      `yaml:"slot"`
	Layout      *LayoutDoc  `yaml:"layout"`
	Permissions []string    `yaml:"permissions"`
	DataSlots   []string    `yaml:"data_slots"`
	Produces    []string    `yaml:"produces"`
	Nav         *NavDoc     `yaml:"nav"`
	Upgrade     *UpgradeDoc `yaml:"upgrade"`
}

// EffectiveSlot 返回模块声明的挂载槽（扁平 `slot:` 优先，回落 `layout.slot`）。
func (m ModuleDoc) EffectiveSlot() string {
	if s := strings.TrimSpace(m.Slot); s != "" {
		return s
	}
	if m.Layout != nil {
		return strings.TrimSpace(m.Layout.Slot)
	}
	return ""
}

// NegotiationDoc 是能力协商规则（`negotiation`）。
type NegotiationDoc struct {
	OnMissingDataSlot    string `yaml:"onMissingDataSlot"`
	OnMissingPermission  string `yaml:"onMissingPermission"`
	OnVersionMismatch    string `yaml:"onVersionMismatch"`
	ModuleIsolation      bool   `yaml:"moduleIsolation"`
	IndependentlyDisable bool   `yaml:"independentlyDisableable"`
}

// ManifestDoc 是一份已解析的 SlotManifest。
type ManifestDoc struct {
	ManifestVersion string
	LayoutSlots     []LayoutSlotDoc
	Modules         []ModuleDoc
	Negotiation     NegotiationDoc

	// Has* 表示 YAML 里**真的写了**对应顶层键（用于区分「漏写」与「写了但为空」）。
	HasManifestVersion bool
	HasLayoutSlots     bool
	HasModules         bool
	HasNegotiation     bool

	// Raw 保留原始顶层键（供「未知/拼错的顶层键」探测 —— 拼错的键 YAML 不报错，
	// 只会静默丢失，正是本仓反复出现的静默失效形态）。
	Raw map[string]any
}

// ParseModuleManifest 把 SlotManifest 原文解析为 ManifestDoc。
//
// 解析失败（非法 YAML / 空文件 / 结构不匹配）⇒ 返回 error，由调用方 fail-closed。
// 注意：**结构不匹配也是错误**（例如 `modules: 3` —— 标量塞进列表字段）。
//
// ★ 实测澄清（勿重复排查）：yaml.v3 把**任意标量**解进 `string` 字段时保留原文
// （`manifestVersion: 1.0` 未加引号也得 `"1.0"`，不报错、不丢字段），
// 故这里**没有**「不加引号就丢字段」的陷阱 —— 由
// TestG9Manifest_UnquotedScalarIsAcceptedAsText 把该结论固化，防止日后
// 有人把不存在的陷阱当成真问题去「修」。
func ParseModuleManifest(data []byte) (ManifestDoc, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return ManifestDoc{}, fmt.Errorf("SlotManifest 不是合法 YAML：%w", err)
	}
	if raw == nil {
		return ManifestDoc{}, fmt.Errorf("SlotManifest 为空文件（解析出 nil）")
	}
	var typed struct {
		ManifestVersion string          `yaml:"manifestVersion"`
		LayoutSlots     []LayoutSlotDoc `yaml:"layoutSlots"`
		Modules         []ModuleDoc     `yaml:"modules"`
		Negotiation     NegotiationDoc  `yaml:"negotiation"`
	}
	if err := yaml.Unmarshal(data, &typed); err != nil {
		return ManifestDoc{}, fmt.Errorf("SlotManifest 结构不匹配：%w", err)
	}
	doc := ManifestDoc{
		ManifestVersion: typed.ManifestVersion,
		LayoutSlots:     typed.LayoutSlots,
		Modules:         typed.Modules,
		Negotiation:     typed.Negotiation,
		Raw:             raw,
	}
	_, doc.HasManifestVersion = raw["manifestVersion"]
	_, doc.HasLayoutSlots = raw["layoutSlots"]
	_, doc.HasModules = raw["modules"]
	_, doc.HasNegotiation = raw["negotiation"]
	return doc, nil
}

// CheckModuleManifestDeclared 断言 SlotManifest 的**声明层**全部可解析、可核对。
//
// registeredSlots = 真实数据槽注册表（`slot.Registry.RegisteredSlotIDs()`，
// 即本仓统一的「注册集合」形态 `map[string]bool`），
// 用于校验 `data_slots` 的**引用完整性** —— 幽灵引用会让能力协商永远失败，
// 而「协商失败」在旧实现里表现为「模块静默不挂载」，不报错。
//
// 返回人类可读原因（fail-closed：无法确认 ⇒ 不视为合规）。
func CheckModuleManifestDeclared(doc ManifestDoc, registeredSlots map[string]bool) []string {
	var violations []string

	// ① 清单格式版本
	switch {
	case !doc.HasManifestVersion || strings.TrimSpace(doc.ManifestVersion) == "":
		violations = append(violations,
			"SlotManifest 未声明 manifestVersion —— 清单格式版本不可判定（无法做版本兼容检查）")
	case !manifestVersionRe.MatchString(strings.TrimSpace(doc.ManifestVersion)):
		violations = append(violations, fmt.Sprintf(
			"SlotManifest 的 manifestVersion=%q 不可解析（应形如 \"1.0\"）", doc.ManifestVersion))
	}

	// ② 布局槽位
	if !doc.HasLayoutSlots || len(doc.LayoutSlots) == 0 {
		violations = append(violations,
			"SlotManifest 未声明任何 layoutSlots —— 模块无从声明挂载位置（挂载槽校验失去依据）")
	}
	layoutSlotSet := map[string]bool{}
	for _, ls := range doc.LayoutSlots {
		id := strings.TrimSpace(ls.ID)
		if id == "" {
			violations = append(violations, "layoutSlots 存在 id 为空的条目")
			continue
		}
		if layoutSlotSet[id] {
			violations = append(violations, fmt.Sprintf("layoutSlots 的 id %q 重复声明", id))
			continue
		}
		layoutSlotSet[id] = true
	}

	// ③ 模块声明
	if !doc.HasModules || len(doc.Modules) == 0 {
		violations = append(violations,
			"SlotManifest 未声明任何 modules —— 插槽主机将零模块挂载（/api/admin/modules 恒为空列表）")
	}
	regSlotSet := map[string]bool{}
	for s, ok := range registeredSlots {
		if ok && strings.TrimSpace(s) != "" {
			regSlotSet[strings.TrimSpace(s)] = true
		}
	}
	kinds := map[string]bool{}
	for _, k := range moduleKinds {
		kinds[k] = true
	}
	seen := map[string]bool{}
	for i, m := range doc.Modules {
		id := strings.TrimSpace(m.ID)
		who := id
		if who == "" {
			who = fmt.Sprintf("#%d", i+1)
		}
		switch {
		case id == "":
			violations = append(violations, fmt.Sprintf("模块 %s 的 id 为空", who))
		case seen[id]:
			violations = append(violations, fmt.Sprintf("模块 id %q 重复声明", id))
		default:
			seen[id] = true
		}
		if strings.TrimSpace(m.Name) == "" {
			violations = append(violations, fmt.Sprintf("模块 %s 未声明 name", who))
		}
		if !moduleVersionRe.MatchString(strings.TrimSpace(m.Version)) {
			violations = append(violations, fmt.Sprintf(
				"模块 %s 的 version=%q 不可解析（应形如 \"1.0.0\"）—— 独立升级检查失去依据",
				who, m.Version))
		}
		if k := strings.TrimSpace(m.Kind); !kinds[k] {
			violations = append(violations, fmt.Sprintf(
				"模块 %s 的 kind=%q 不在词表 %v 内（docs/01 §8.1）", who, m.Kind, moduleKinds))
		}
		if strings.TrimSpace(m.Lang) == "" {
			violations = append(violations, fmt.Sprintf("模块 %s 未声明 lang", who))
		}
		switch slot := m.EffectiveSlot(); {
		case slot == "":
			violations = append(violations, fmt.Sprintf(
				"模块 %s 未声明挂载槽（`slot:` 或 `layout.slot:` 二者皆空）", who))
		case !layoutSlotSet[slot]:
			violations = append(violations, fmt.Sprintf(
				"模块 %s 声明的挂载槽 %q 不在 layoutSlots 中（幽灵槽位 ⇒ 主机无处挂载）", who, slot))
		}
		if len(m.Permissions) == 0 {
			violations = append(violations, fmt.Sprintf(
				"模块 %s 未声明 permissions —— 能力协商「权限不满足则隐藏」失去依据", who))
		}
		for _, ds := range m.DataSlots {
			d := strings.TrimSpace(ds)
			if d == "" {
				violations = append(violations, fmt.Sprintf("模块 %s 的 data_slots 含空项", who))
				continue
			}
			if !regSlotSet[d] {
				violations = append(violations, fmt.Sprintf(
					"模块 %s 的 data_slots 引用未注册的数据槽 %q（幽灵引用 ⇒ 能力协商永远失败、"+
						"模块静默不挂载）", who, d))
			}
		}
		if len(m.Produces) == 0 {
			violations = append(violations, fmt.Sprintf(
				"模块 %s 未声明 produces —— 主机无法知道它产出什么契约", who))
		}
		if m.Upgrade != nil {
			if mv := strings.TrimSpace(m.Upgrade.MinHostVersion); mv != "" &&
				!manifestVersionRe.MatchString(mv) {
				violations = append(violations, fmt.Sprintf(
					"模块 %s 的 upgrade.minHostVersion=%q 不可解析（应形如 \"1.0.0\"）", who, mv))
			}
		}
	}

	// ④ 能力协商规则
	if !doc.HasNegotiation {
		violations = append(violations,
			"SlotManifest 未声明 negotiation —— 能力不满足时的行为不可判定（不得默认放行）")
	} else {
		v := strings.TrimSpace(doc.Negotiation.OnMissingDataSlot)
		switch {
		case v == "":
			violations = append(violations, "negotiation.onMissingDataSlot 未声明")
		case !manifestHasValue(negotiationOnMissingDataSlot, v):
			violations = append(violations, fmt.Sprintf(
				"negotiation.onMissingDataSlot=%q 不在词表 %v 内", doc.Negotiation.OnMissingDataSlot,
				negotiationOnMissingDataSlot))
		}
		if strings.TrimSpace(doc.Negotiation.OnMissingPermission) == "" {
			violations = append(violations, "negotiation.onMissingPermission 未声明")
		}
		if strings.TrimSpace(doc.Negotiation.OnVersionMismatch) == "" {
			violations = append(violations, "negotiation.onVersionMismatch 未声明")
		}
	}

	sort.Strings(violations)
	return violations
}

// CheckModuleManifestMatchesRegistry 断言「清单声明的模块」与「运行时注册表已挂载的模块」
// **双向一致**（G9 / docs/02 M-SLOT 验收「新模块零改主机代码即可挂载」）。
//
// ★ 为什么必须双向：
//
//	单向（只查「清单里的都在运行时」）会漏掉「运行时挂了个清单没有的模块」——
//	那是**声明失去约束力**（清单不再是唯一事实源）；
//	单向（只查「运行时的都在清单里」）会漏掉本次抓到的真实缺口 ——
//	**清单声明了 6 个模块、运行时一个都没挂载**，而接口只是安静地返回空列表。
//
// ★ 这是判定函数在**生产路径**上的调用点（由 slothost.Manifest.MountAll 调用），
// 而不是又一条只被自己测试调用的恒真断言。
func CheckModuleManifestMatchesRegistry(doc ManifestDoc, mountedIDs []string) []string {
	var violations []string
	mounted := map[string]bool{}
	for _, id := range mountedIDs {
		if v := strings.TrimSpace(id); v != "" {
			mounted[v] = true
		}
	}
	declared := map[string]bool{}
	for _, m := range doc.Modules {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		declared[id] = true
		if !mounted[id] {
			violations = append(violations, fmt.Sprintf(
				"模块 %q 在 SlotManifest 里已声明，但运行时注册表里**没有挂载** —— "+
					"声明等于装饰（/api/admin/modules 会把它漏掉）", id))
		}
	}
	for _, id := range mountedIDs {
		d := strings.TrimSpace(id)
		if d == "" {
			continue
		}
		if !declared[d] {
			violations = append(violations, fmt.Sprintf(
				"运行时注册表挂载了模块 %q，但 SlotManifest 未声明它 —— 清单失去约束力", d))
		}
	}
	sort.Strings(violations)
	return violations
}

// manifestHasValue 判断 xs 是否含 v（本文件私有，避免与既有 helper 重名）。
func manifestHasValue(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
