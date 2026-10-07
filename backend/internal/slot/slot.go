// Package slot —— 数据槽注册表（M-SLOT，G4/G5 的**真实生产链路**）。
//
// 为什么要有这个包（承袭本仓「判定函数没有生产调用点」这个病）：
//
//	docs/05 的 G2/G3/G4/G5 四条闸门，其判定函数 `gate.CheckDefaultCollapsed` /
//	`CheckNoZeroImputation` / `CheckAlgorithmNoDataSource` / `CheckSlotsRegistered` /
//	`DecideCoverage` 在此之前**全仓没有任何非测试调用点** —— 唯一「调用」它们的
//	就是它们自己的 `gate_test.go`。也就是说：
//	  * 「算法 YAML 无数据源字段」从不真的扫过 YAML；
//	  * 「每个 depends_on_slots 已注册」从不真的对过注册表；
//	  * 「覆盖率 < 门限 ⇒ 跳过」从不真的看过一个槽的状态。
//	文档却标着「✅ 已实现」。这是同一个病的**第八个变种**
//	（前七见 docs/11 与本表 G7/G8/G10/G12 的收口记录）。
//
// 本包把 `slots/*.yaml` 与 `algorithms/*.yaml` 读**真文件**、过**真闸门**：
//
//	LoadRegistry(slotsDir, algosDir)
//	  → 解析 YAML 为 gate.AlgoDoc / Slot（Raw 保留全部原始键，供 G4 探测数据源字段）
//	  → 逐条调 gate.CheckAlgorithmNoDataSource / CheckSlotsRegistered 判定
//	  → 任一违规 ⇒ 返回 error（fail-closed：注册表半残比没有更危险）
//
// 于是 G4 的两条断言第一次有了真实生产调用点：**YAML 真的被扫过了**。
//
// 与「覆盖率门控」的分工：
//   - 本包 = **槽的静态身份与引用完整性**（注册表、状态、门限）；
//   - `gate.DecideCoverage` 的判活由 *覆盖率观测* 驱动，见 Coverage.Cases() ——
//     它把本注册表的槽状态与实测覆盖率**合成**成 `gate.CoverageCase` 交给闸门，
//     使「MISSING ⇒ 硬跳过」也第一次有真实调用点。
package slot

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// 状态取值（docs/03 §2.1）。PARTIAL 与 MISSING 对算法同等对待：均触发 skip。
const (
	StatusAvailable = "AVAILABLE"
	StatusPartial   = "PARTIAL"
	StatusMissing   = "MISSING"
)

// Slot 是一个数据槽的静态声明（对应 slots/*.yaml，docs/03 §2.3）。
//
// 槽只回答「数据从哪来」；算法只回答「怎么算」—— 故本结构**不含 formula**。
type Slot struct {
	ID           string  `yaml:"id"`
	Name         string  `yaml:"name"`
	SourceKind   string  `yaml:"source_kind"`
	SourceRef    string  `yaml:"source_ref"`
	KeyStrategy  string  `yaml:"key_strategy"`
	CoverageGate float64 `yaml:"coverage_gate"`
	Freshness    string  `yaml:"freshness"`
	Permission   string  `yaml:"permission"`
	Status       string  `yaml:"status"`
	Notes        string  `yaml:"notes"`

	// Raw 保留原始键（供 G4 探测是否混入数据源字段；也用于「未知键」自证）。
	Raw map[string]any `yaml:"-"`
}

// Algorithm 是一个算法声明（对应 algorithms/*.yaml，docs/03 §3.1）。
type Algorithm struct {
	ID             string   `yaml:"id"`
	Name           string   `yaml:"name"`
	Version        int      `yaml:"version"`
	Formula        string   `yaml:"formula"`
	Unit           string   `yaml:"unit"`
	Permission     string   `yaml:"permission"`
	DependsOnSlots []string `yaml:"depends_on_slots"`
	// DependsOnAlgos 声明「算法依赖算法」（docs/03 §3.2 尾注的待决策项）。
	//
	// ★ 为什么必须有这个字段（2026-10-07 实测缺陷）：
	//   `algo.net_contrib` 的公式是 `cm2 - overhead_alloc`，其中 `cm2` 是
	//   **上游算法**的字段。但此前 AlgoDef **没有任何地方能声明该依赖** ——
	//   绑定只能靠桶 `produced_by` 的书写顺序偶然成立。桶清单少一个算法、
	//   或顺序写反，公式就在**每条数据**上静默取 Missing（整列空、零报错）。
	DependsOnAlgos []string `yaml:"depends_on_algos"`
	// Status 实现状态：缺省 ACTIVE；口径未定/实现待补写 PENDING（见 gate.StatusPending）。
	Status        string `yaml:"status"`
	WritesBucket  string `yaml:"writes_bucket"`
	MissingPolicy string `yaml:"missing_policy"`
	Trace         bool   `yaml:"trace"`

	// Raw 保留原始键（G4 用它探测 source/table/sql 等数据源字段）。
	Raw map[string]any `yaml:"-"`
}

// Registry 槽与算法的注册表（加载即校验，fail-closed）。
type Registry struct {
	slots map[string]Slot
	algos map[string]Algorithm
	order []string // 槽的稳定顺序（便于确定性输出/审计）
}

// Slots 返回全部槽（按 ID 升序）；返回副本，调用方改不动内部状态。
func (r *Registry) Slots() []Slot {
	out := make([]Slot, 0, len(r.slots))
	for _, id := range r.order {
		out = append(out, r.slots[id])
	}
	return out
}

// Slot 按 ID 取槽。
func (r *Registry) Slot(id string) (Slot, bool) {
	s, ok := r.slots[id]
	return s, ok
}

// Algorithms 返回全部算法（按 ID 升序）。
func (r *Registry) Algorithms() []Algorithm {
	out := make([]Algorithm, 0, len(r.algos))
	for _, a := range r.algos {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Algorithm 按 ID 取算法。
func (r *Registry) Algorithm(id string) (Algorithm, bool) {
	a, ok := r.algos[id]
	return a, ok
}

// RegisteredSlotIDs 返回「已注册槽」集合，供 gate.CheckSlotsRegistered 做引用完整性。
func (r *Registry) RegisteredSlotIDs() map[string]bool {
	out := make(map[string]bool, len(r.slots))
	for id := range r.slots {
		out[id] = true
	}
	return out
}

// RegisteredAlgorithmIDs 返回「已注册算法」集合，供 gate.CheckBucketProducersRegistered
// 做**桶 → 算法**这一侧的引用完整性（G4 反向）。
func (r *Registry) RegisteredAlgorithmIDs() map[string]bool {
	out := make(map[string]bool, len(r.algos))
	for id := range r.algos {
		out[id] = true
	}
	return out
}

// AlgoDocs 把算法翻成 gate.AlgoDoc（Raw 原样带上 ⇒ G4 能真扫数据源字段）。
func (r *Registry) AlgoDocs() []gate.AlgoDoc {
	algos := r.Algorithms()
	out := make([]gate.AlgoDoc, 0, len(algos))
	for _, a := range algos {
		out = append(out, gate.AlgoDoc{
			ID:             a.ID,
			Formula:        a.Formula,
			DependsOnSlots: a.DependsOnSlots,
			DependsOnAlgos: a.DependsOnAlgos,
			Status:         a.Status,
			Raw:            a.Raw,
		})
	}
	return out
}

// FreshnessDoc 返回全部槽的采集时效声明，供 gate.CheckFreshnessDeclared 判定。
//
// ★ 这是「槽的 freshness 必须可解析」这条断言第一次拿到**磁盘上的真值**。
func (r *Registry) FreshnessDocs() []gate.FreshnessDoc {
	out := make([]gate.FreshnessDoc, 0, len(r.order))
	for _, id := range r.order {
		s := r.slots[id]
		out = append(out, gate.FreshnessDoc{SlotID: s.ID, Raw: s.Freshness})
	}
	return out
}

// FreshnessMinutes 把槽的 freshness 声明解析为分钟数（第二返回值 false = 不可解析）。
//
// 解析实现**委托** gate.ParseFreshness —— 单位集合只有一份权威表（gate.freshUnits），
// 防止「校验器认 1d、判活器不认 1d」这类分叉静默发生。
func (s Slot) FreshnessMinutes() (int, bool) {
	return gate.ParseFreshness(s.Freshness)
}

// Validate 用 **gate 的多条 G4 判定函数**校验注册表本身。
//
// ★ 这是 G4 闸门第一次被生产代码调用：判定的对象是**磁盘上的真 YAML**，
// 而不是测试里手搓的假数据。违规即返回 error（fail-closed）。
func (r *Registry) Validate() error {
	docs := r.AlgoDocs()
	var violations []string
	violations = append(violations, gate.CheckAlgorithmNoDataSource(docs)...)
	violations = append(violations, gate.CheckSlotsRegistered(docs, r.RegisteredSlotIDs())...)
	// 槽的采集时效必须显式且可解析（G4 / docs/03 §2.3）。
	//
	// ★ 此前 `freshness` 是**被读进来、但没有任何判定消费**的一个字符串：
	// 解析进 Slot.Freshness → 写进 DB → 此后全仓无人再读。于是
	// `freshness: 7d` 与 `freshness: 随便写` 在行为上完全等价 ——
	// 都是「不影响任何事」。这条断言把「时效」从装饰字段变成**必须成立的前提**，
	// 并由 coverage.go 的真正判活（超期 ⇒ DEGRADED ⇒ skip）接手。
	violations = append(violations, gate.CheckFreshnessDeclared(r.FreshnessDocs())...)
	// 算法 → 算法 的引用完整性（depends_on_algos 必须指向已注册算法）。
	// 与 CheckSlotsRegistered 对称；缺了它，`depends_on_algos: [algo.ghost]`
	// 会被当作「已声明依赖」从而让绑定校验放行一个根本不存在的来源。
	for _, a := range r.Algorithms() {
		for _, up := range a.DependsOnAlgos {
			if _, ok := r.algos[up]; !ok {
				violations = append(violations, fmt.Sprintf(
					"algo %s 的 depends_on_algos 引用未注册算法 %q", a.ID, up))
			}
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("槽注册表校验失败（G4）：\n  - %s", strings.Join(violations, "\n  - "))
	}
	return nil
}

// LoadRegistry 从目录读全部 slots/*.yaml 与 algorithms/*.yaml，加载并**立即校验**。
//
// 目录为空 ⇒ 报错：静默返回空注册表会让后续所有校验「因为没东西可查」而永远通过 ——
// 这正是本仓反复出现的「假闸门」形态。
func LoadRegistry(slotsDir, algosDir string) (*Registry, error) {
	slots, err := loadSlots(slotsDir)
	if err != nil {
		return nil, err
	}
	algos, err := loadAlgorithms(algosDir)
	if err != nil {
		return nil, err
	}
	if len(slots) == 0 {
		return nil, fmt.Errorf("%s 下没有槽定义（*.yaml）", slotsDir)
	}
	if len(algos) == 0 {
		return nil, fmt.Errorf("%s 下没有算法定义（*.yaml）", algosDir)
	}

	r := &Registry{slots: map[string]Slot{}, algos: map[string]Algorithm{}}
	for _, s := range slots {
		if _, dup := r.slots[s.ID]; dup {
			return nil, fmt.Errorf("槽 ID 重复：%s", s.ID)
		}
		r.slots[s.ID] = s
		r.order = append(r.order, s.ID)
	}
	sort.Strings(r.order)
	for _, a := range algos {
		if _, dup := r.algos[a.ID]; dup {
			return nil, fmt.Errorf("算法 ID 重复：%s", a.ID)
		}
		r.algos[a.ID] = a
	}

	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// loadSlots 读槽定义。
func loadSlots(dir string) ([]Slot, error) {
	files, err := yamlFiles(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Slot, 0, len(files))
	for _, f := range files {
		var s Slot
		if err := unmarshalYAML(f, &s); err != nil {
			return nil, err
		}
		if s.ID == "" {
			return nil, fmt.Errorf("%s 缺少 id 字段", f)
		}
		if err := validateSlot(f, s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// loadAlgorithms 读算法定义。
func loadAlgorithms(dir string) ([]Algorithm, error) {
	files, err := yamlFiles(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Algorithm, 0, len(files))
	for _, f := range files {
		var a Algorithm
		if err := unmarshalYAML(f, &a); err != nil {
			return nil, err
		}
		if a.ID == "" {
			return nil, fmt.Errorf("%s 缺少 id 字段", f)
		}
		if len(a.DependsOnSlots) == 0 {
			return nil, fmt.Errorf("%s（%s）未声明 depends_on_slots", f, a.ID)
		}
		out = append(out, a)
	}
	return out, nil
}

// validateSlot 校验槽的必备字段（docs/03 §2.3 + G4「每个槽有 coverage_gate」）。
func validateSlot(file string, s Slot) error {
	if s.CoverageGate <= 0 {
		return fmt.Errorf("%s（%s）缺少正数 coverage_gate（G4：每个槽必须有门限）", file, s.ID)
	}
	switch s.Status {
	case StatusAvailable, StatusPartial, StatusMissing:
		// 合法；ACTIVE/DISABLED 属 gate.CoverageCase 的运行时状态，槽定义里不允许
	case "":
		// 未标状态 ⇒ 视为 MISSING（保守，fail-closed）
	default:
		return fmt.Errorf("%s（%s）status=%q 非法（应为 %s/%s/%s）",
			file, s.ID, s.Status, StatusAvailable, StatusPartial, StatusMissing)
	}
	if s.SourceKind == "" {
		return fmt.Errorf("%s（%s）缺少 source_kind（槽必须回答『数据从哪来』）", file, s.ID)
	}
	if s.Permission == "" {
		return fmt.Errorf("%s（%s）缺少 permission", file, s.ID)
	}
	return nil
}

// yamlFiles 列出目录下的 .yaml/.yml（升序，确定性）。
func yamlFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取目录 %s 失败: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// unmarshalYAML 解析 YAML 同时保留 Raw（原始键值），供 G4 探测数据源字段。
func unmarshalYAML(path string, dst any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	var generic map[string]any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return fmt.Errorf("解析 %s 的原始键失败: %w", path, err)
	}
	switch v := dst.(type) {
	case *Slot:
		v.Raw = generic
	case *Algorithm:
		v.Raw = generic
	}
	return nil
}
