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
	// derivedResolver 由 ValidateWithBuckets 注入：把 derived 槽的 source_ref
	// 解析到**真实上游**（事实表简名 或 物化桶 ID）。nil ⇒ 尚无解析能力
	// （LoadRegistry 阶段），此时 derived 的 ref 不参与校验（见 sourceDocsForValidate）。
	derivedResolver func(ref string) bool
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

// KeyStrategyDocs 返回全部槽的匹配键策略声明，供 gate.CheckKeyStrategyDeclared /
// CheckKeyStrategyCohesion 判定。
//
// ★ 这是「槽的 key_strategy 必须可解析」这条断言第一次拿到**磁盘上的真值**。
// 同时带上 SourceKind / SourceRef，用于「键与来源是否相称」以及
// 「共用来源的槽键口径必须一致」两类判定 —— 二者都需要跨槽视野，
// 单个槽的 YAML 看不出来。
func (r *Registry) KeyStrategyDocs() []gate.KeyStrategyDoc {
	out := make([]gate.KeyStrategyDoc, 0, len(r.order))
	for _, id := range r.order {
		s := r.slots[id]
		out = append(out, gate.KeyStrategyDoc{
			SlotID:     s.ID,
			Raw:        s.KeyStrategy,
			SourceKind: s.SourceKind,
			SourceRef:  s.SourceRef,
		})
	}
	return out
}

// PermissionDocs 返回全部槽与算法的密级声明，供 gate.CheckPermissionDeclared 判定
// （G4 第八侧）。
//
// ★ 这是「密级必须可解析且已知」这条断言第一次拿到**磁盘上的真值**。
// 槽与算法两侧都收：二者都有 permission 列、都在此前「被读被存无判定消费」。
func (r *Registry) PermissionDocs() []gate.PermissionDoc {
	out := make([]gate.PermissionDoc, 0, len(r.order)+len(r.algos))
	for _, id := range r.order {
		out = append(out, gate.PermissionDoc{
			Kind: "槽", ID: r.slots[id].ID, Raw: r.slots[id].Permission,
		})
	}
	var algoIDs []string
	for id := range r.algos {
		algoIDs = append(algoIDs, id)
	}
	sort.Strings(algoIDs)
	for _, id := range algoIDs {
		out = append(out, gate.PermissionDoc{
			Kind: "算法", ID: r.algos[id].ID, Raw: r.algos[id].Permission,
		})
	}
	return out
}

// UnitDocs 返回全部算法的计量单位声明，供 gate.CheckUnitDeclared /
// CheckUnitMatchesFormulaKind 判定（G4 第九侧）。
//
// ★ 这是「算法的 unit 必须可解析且已知」这条断言第一次拿到**磁盘上的真值**。
// 同时带上 Formula：比率型判定（`A / B`）需要看公式形态 —— 单位与公式
// 是**一对**，单看 unit 字段看不出「金额公式声明成比率」这类矛盾。
func (r *Registry) UnitDocs() []gate.UnitDoc {
	var algoIDs []string
	for id := range r.algos {
		algoIDs = append(algoIDs, id)
	}
	sort.Strings(algoIDs)
	out := make([]gate.UnitDoc, 0, len(algoIDs))
	for _, id := range algoIDs {
		out = append(out, gate.UnitDoc{
			Kind: "算法", ID: r.algos[id].ID, Raw: r.algos[id].Unit,
		})
	}
	return out
}

// AlgoFormulas 返回「算法 ID → 公式」的映射，供 CheckUnitMatchesFormulaKind
// 做单位与公式形态的相称判定。
func (r *Registry) AlgoFormulas() map[string]string {
	out := make(map[string]string, len(r.algos))
	for id, a := range r.algos {
		out[id] = a.Formula
	}
	return out
}

// AlgorithmVersionDocs 返回全部算法的**版本声明**，供 gate.CheckAlgorithmVersionDeclared
// 判定（G4 第十侧 / docs/03 §3.1）。
//
// ★ 这是「算法的 version 必须可解析为正整数」这条断言第一次拿到**磁盘上的真值**。
//
// ★ 为什么 Raw 优先取 **YAML 原文**（`a.Raw["version"]`）而不是 `strconv.Itoa(a.Version)`：
//
//	版本是数值比较对象，报错必须点名**作者写下的那串字符**。若 Raw 一律由 int 反推，
//	「漏写 version」会被显示成 `0`（看着像作者真写了 0，其实他根本没写）——
//	报错信息误导人。取原文则能如实区分：缺键 ⇒ Raw="" ⇒ 报「版本号为空」。
//
// ★ 仅在**未经 YAML 解析**（Raw 缺键但结构体有值，如程序内构造）时退回十进制字段，
//
//	避免把「有值」误报成「为空」。
func (r *Registry) AlgorithmVersionDocs() []gate.VersionDoc {
	var algoIDs []string
	for id := range r.algos {
		algoIDs = append(algoIDs, id)
	}
	sort.Strings(algoIDs)
	out := make([]gate.VersionDoc, 0, len(algoIDs))
	for _, id := range algoIDs {
		a := r.algos[id]
		out = append(out, gate.VersionDoc{
			AlgoID: a.ID,
			Raw:    rawVersionText(a),
		})
	}
	return out
}

// rawVersionText 取算法版本号的**原始文本**（保留作者写法）；缺键且结构体为空 ⇒ ""。
//
// 只做「原样呈现」，不做任何语义归一 —— 判定交给 gate.ParseAlgorithmVersion。
func rawVersionText(a Algorithm) string {
	if v, ok := a.Raw["version"]; ok && v != nil {
		return fmt.Sprintf("%v", v)
	}
	if a.Version != 0 {
		return fmt.Sprintf("%d", a.Version)
	}
	return ""
}

// MissingPolicyDocs 返回全部算法的**缺失策略**声明，供 gate.CheckMissingPolicyDeclared
// 判定（G4 第十三侧 / docs/03 §3.1）。
//
// ★ 这是「缺失策略必须可解析且已知」这条断言第一次拿到**磁盘上的真值**。
//
// ★ 为什么必须回报「键是否存在」而不是只看结构体字符串：
//
//	YAML 会把裸 `missing_policy: null`（及 `~`、空值）解析成 nil，落到 string
//	字段就是 ""，与「漏写该键」在结构体上**完全一样**，但两者修法不同
//	（漏写 ⇒ 补策略；裸 null ⇒ 加引号写 "null"）。故这里回看 Raw 原始键。
func (r *Registry) MissingPolicyDocs() []gate.MissingPolicyDoc {
	var algoIDs []string
	for id := range r.algos {
		algoIDs = append(algoIDs, id)
	}
	sort.Strings(algoIDs)
	out := make([]gate.MissingPolicyDoc, 0, len(algoIDs))
	for _, id := range algoIDs {
		a := r.algos[id]
		raw, declared := rawMissingPolicyText(a)
		out = append(out, gate.MissingPolicyDoc{AlgoID: a.ID, Raw: raw, Declared: declared})
	}
	return out
}

// rawMissingPolicyText 取缺失策略的**原始文本**，并回报该键是否真的存在。
//
// 区分三种情形（这正是裸 null 陷阱的判据）：
//   - 键不存在            ⇒ ("", false) 漏写；
//   - 键存在、值是 YAML 空值 ⇒ ("", true)  裸 null / ~ / 空（作者意图被 YAML 吞掉）；
//   - 键存在、值非空       ⇒ (原文, true) 正常；
//   - 未经 YAML（Raw 缺键但结构体有值，如程序内构造）⇒ (结构体值, true)，避免把「有值」误报成「为空」。
func rawMissingPolicyText(a Algorithm) (string, bool) {
	if v, ok := a.Raw["missing_policy"]; ok {
		if v == nil {
			return "", true
		}
		return fmt.Sprintf("%v", v), true
	}
	if s := strings.TrimSpace(a.MissingPolicy); s != "" {
		return s, true
	}
	return "", false
}

// Validate 用 **gate 的多条 G4 判定函数**校验注册表本身。//
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
	// 槽的**数据来源**必须可解析（G4 第六侧 / docs/03 §2.3）。
	//
	// ★ 此前 `source_kind`/`source_ref` 是**被读进来、写进 DB、然后被彻底遗忘**的一对
	// 字段：全仓唯一校验是 `validateSlot` 的 `SourceKind == ""`，于是
	// `source_kind: 随便写` 与 `api` 等价、`source_ref` 删掉也照过。
	// 这条断言把「数据从哪来」从装饰字段变成**必须成立的前提**。
	//
	// derived 槽的 ref 交给 `ValidateWithBuckets` 在**桶到位后**收口
	// （LoadRegistry 阶段桶还没加载，此处按「未声明 ref」处理，不伪造解析结果）。
	violations = append(violations, gate.CheckSlotSourceResolvable(r.sourceDocsForValidate(false))...)
	// 槽的**匹配键策略**必须可解析（G4 第七侧 / docs/03 §2.2、§2.3）。
	//
	// ★ 此前 `key_strategy` 是**被读进来、写进 DB（registry_slot.key_strategy）、
	// 但没有任何判定消费**的一个字段：全链路只有解析/赋值/写库三类用法，
	// 没有第四类（比较、判定、阈值）。于是 16 个槽全声明了它，
	// 而 `store_sku` 与 `随便写` 在行为上完全等价。
	//
	// 后果不是「少一条断言」，而是**跨源对齐口径失去判据**：key_strategy 决定
	// 多条来源按什么键合并成一行；写错它，桶照样算出来，但合并后的每一行都可能
	// 张冠李戴（A 店的成本配到 B 店的收入上），且全程零报错。
	violations = append(violations, gate.CheckKeyStrategyDeclared(r.KeyStrategyDocs())...)
	// 共用同一来源的槽，其键口径必须一致（逐条合法、合起来是错的 —— 单元断言看不见）。
	violations = append(violations, gate.CheckKeyStrategyCohesion(r.KeyStrategyDocs())...)
	// 槽的**密级**必须可解析，且不得低于依赖它的算法的密级（G4 第八侧 / docs/01 §13.3）。
	//
	// ★ 此前 `permission` 是**被读进来、写进 DB（registry_slot.permission）、
	// 但没有任何判定消费**的一个字段：全链路只有解析/赋值/写库三类用法，
	// 没有第四类（比较、判定、阈值）。Go 侧唯一的校验是 `validateSlot` 的
	// `s.Permission == ""`，于是 16 个槽全声明了它，而 `L4` 与 `想写什么写什么`
	// 在 Go 侧行为完全等价（DB 侧 0002 的 CHECK 只是**入库兜底**，不等于 Go 侧有闸门）。
	//
	// 后果是**越权**：成本/利润类槽（L3/L4）一旦被写成 L1，就会与产品 ID 同权限暴露，
	// 而脱敏与区间展示（docs/01 §13.3）也随之失去依据 —— 且全程零报错。
	violations = append(violations, gate.CheckPermissionDeclared(r.PermissionDocs())...)
	// 算法的**计量单位**必须可解析且已知，且与公式形态相称（G4 第九侧 / docs/03 §3.1）。
	//
	// ★ 此前 `unit` 是**被读进来、写进 DB（registry_algorithm.unit）、
	// 但没有任何判定消费**的一个字段：全链路只有解析/赋值/写库三类用法，
	// 没有第四类（比较、判定、阈值）。全仓 `grep "\.Unit"` 的非测试命中只有
	// admin_handlers.go / plane.go / store/admin.go 三处，**全是序列化与建表**；
	// `precomp.AlgoDef.Unit` 更是结构性死字段（BuildRow 从不读它）。
	// 于是 `unit: THB` 与 `unit: 想写什么写什么` 在加载期行为完全等价。
	//
	// 后果是**量纲静默失真**：`algo.gmp` 是比率（0.42）却可被写成 THB，
	// 页面就会出现 `0.42 THB` 这种静默错误口径；跨算法聚合（哪些列可相加）
	// 也以 unit 为唯一判据 —— unit 不可信则可加性整体失效。
	violations = append(violations, gate.CheckUnitDeclared(r.UnitDocs())...)
	violations = append(violations, gate.CheckUnitMatchesFormulaKind(r.UnitDocs(), r.AlgoFormulas())...)
	// 算法的**版本号**必须可解析为正整数（G4 第十侧 / docs/03 §3.1）。
	//
	// ★ 此前 `version` 看似「处处都在用」（registry_algorithm.version 列、
	// VersionDrift、AlgoVersions、AffectedBuckets …），但把它的**值**当作
	// 被校验对象来看，全链路只有解析/赋值/逐层搬运三类用法，**没有第四类**
	// （比较、判定、阈值）：全仓非测试代码里没有一处把 `Algorithm.Version`
	// 与算法身份比较，DB 侧 0002 把 version 建成 `integer NOT NULL` 且**无 CHECK**。
	// 于是 `version: 想写什么写什么` 在加载期与入库期行为完全等价。
	//
	// 后果是**版本漂移检测静默失真**（G6 的全部结论都建在它上面）：
	// 版本号是「改了公式要重算哪些桶」的唯一判据，若它不可信，
	// AffectedBuckets 可能返回空 ⇒ 改了公式却不重算，报表长期显示错误口径。
	violations = append(violations, gate.CheckAlgorithmVersionDeclared(r.AlgorithmVersionDocs())...)

	// 算法的**缺失策略**必须可解析且已知（G4 第十三侧 / docs/03 §3.1）。
	//
	// ★ 此前 `missing_policy` 是**被读进来、写进 DB（registry_algorithm.missing_policy）、
	// 但没有任何判定消费其声明值**的一个字段：Go 侧没有词表校验，而运行时
	// （precomp.BuildRow）只判 `== "skip"`，其余一切取值（null/error/拼错）统统落到
	// 同一个默认分支写 NULL ⇒ `missing_policy: error` 与 `null` 与乱写**行为完全等价**
	// （DB 侧 0002 的 CHECK 只是入库兜底，不等于 Go 侧有闸门）。
	//
	// 后果是**静默的错误口径**：本该报错的算法静默产出空值；本该置空的算法被当成
	// 跳过（记入 skipped_fields，影响下游覆盖率判定）。
	//
	// ★ 还兜住一个 YAML 保留字陷阱：`missing_policy: null`（裸写）会被 YAML 解析成
	// 空值 ⇒ 被缺省填充静默改成 "skip"，语义被反转。本断言对此单列报错。
	violations = append(violations, gate.CheckMissingPolicyDeclared(r.MissingPolicyDocs())...)
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

// ResolveDerived 报告 derived 槽的 source_ref 是否解析到**真实上游**。
//
// 合法上游有两种形态（docs/01 §12.1 + buckets/*.yaml）：
//   - 事实表简名/全名（`channel_sales` / `fact_channel_sales`）；
//   - 物化桶 ID（`pnl_sku_month`，取「预计算桶的物化列」，docs/03 §2.2）。
//
// derivedResolver 在 `ValidateWithBuckets` 阶段由桶注册表注入；此前为 nil
// ⇒ fail-closed 返回 false（绝不假装解析得到）。
func (r *Registry) ResolveDerived(ref string) bool {
	if r.derivedResolver == nil {
		return false
	}
	return r.derivedResolver(ref)
}

// sourceDocsForValidate 构造 G4 第六侧的判定输入。
//
// withBuckets=false（LoadRegistry 阶段）：derived 槽的 ref 暂时置空 ——
// 上游还没加载、无解析结果可用；derived 的 ref 本就可空，故按「未声明 ref」处理，
// **不伪造解析结果、也不假装核对过**（那正是本仓反复踩的坑）。
//
// withBuckets=true （ValidateWithBuckets 阶段）：带真实上游解析结果。
func (r *Registry) sourceDocsForValidate(withBuckets bool) []gate.SlotSourceDoc {
	out := make([]gate.SlotSourceDoc, 0, len(r.order))
	for _, id := range r.order {
		s := r.slots[id]
		doc := gate.SlotSourceDoc{SlotID: s.ID, Kind: s.SourceKind, Ref: s.SourceRef}
		isDerived := strings.EqualFold(strings.TrimSpace(s.SourceKind), gate.SourceKindDerived)
		if isDerived && !withBuckets {
			// 上游还没加载 ⇒ 无解析结果可用。derived 的 ref 本就可空，
			// 故暂按「未声明 ref」处理（不伪造、不假装核对过）。
			doc.Ref = ""
		} else if isDerived {
			doc.DerivedChecked = true
			doc.DerivedResolved = r.ResolveDerived(strings.TrimSpace(s.SourceRef))
		}
		out = append(out, doc)
	}
	return out
}

// ValidateWithBuckets 在**桶注册表到位后**校验 derived 槽的 source_ref。
//
// ★ 这是「加载顺序」造成静默窗口的收口：`LoadRegistry` 只读 slots/algorithms，
//   而 derived 槽的 ref 指向**上游**（事实表或物化桶）。若不在上游到位后校验，
//   `source_ref: pnl_sku_month_typo` 这类幽灵引用将永远无人发现。
func (r *Registry) ValidateWithBuckets(br *BucketRegistry) error {
	if br == nil {
		return fmt.Errorf("ValidateWithBuckets: 桶注册表为 nil（fail-closed：无法核对 derived 来源）")
	}
	// 上游解析 = 事实表简名/全名（docs/01 §12.1） ∪ 物化桶 ID（buckets/*.yaml）。
	r.derivedResolver = func(ref string) bool {
		return gate.IsKnownFactTable(ref) || br.Resolves(ref)
	}
	var violations []string
	violations = append(violations, gate.CheckSlotSourceResolvable(r.sourceDocsForValidate(true))...)
	if len(violations) > 0 {
		return fmt.Errorf("槽的数据来源校验失败（G4 第六侧）：\n  - %s", strings.Join(violations, "\n  - "))
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
