// bucket.go —— 预计算桶注册表（M-PRECOMP 的静态身份与引用完整性）。
//
// 承袭本仓「判定函数没有生产调用点」这个病（第九个变种）：
//
//	`buckets/*.yaml` 里 `produced_by` 列了哪些算法产出该桶 —— 这是
//	docs/03 §5 预计算桶定义的核心字段。但在此之前：
//	  * 全仓**没有任何代码读 `buckets/*.yaml`**（唯一的命中的是
//	    `store/admin.go` 的一条 INSERT —— 那只是把列名写进 SQL，不是读定义）；
//	  * 于是 `pnl_month.yaml` 的 `produced_by` 里列了 7 个算法，其中
//	    `algo.rev` / `algo.net_rev` / `algo.platform_fee` / `algo.net_contrib`
//	    在 `algorithms/` 下**根本不存在**，而**没有任何东西会报出来**。
//
//	这与 G4「每个 depends_on_slots 已注册」是同一条不变量（引用完整性），
//	只是方向相反：算法 → 槽 已由 slot.Validate 把关；
//	**桶 → 算法**这一侧是漏的，且没有任何生产调用点。
//
//	本文件的 `LoadBucketRegistry` 把 `buckets/*.yaml` 读**真文件**，
//	对 `produced_by` 做引用完整性校验 —— 于是这条不变量第一次有了真实生产调用点。
//
// 与 G6 的关系：`gate.AffectedBuckets(bucketAlgos, …)` 依赖「桶 → 算法」映射
// 才成立（算法升级后要知道该重算哪些桶）。该映射若来自一份**残缺或含幽灵引用**
// 的桶定义，G6 的「仅重算受影响桶」就会漏算 —— 所以注册表必须先自证完整。
package slot

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// Bucket 是一个预计算桶的静态声明（对应 buckets/*.yaml，docs/03 §5）。
type Bucket struct {
	ID      string   `yaml:"id"`
	Grain   []string `yaml:"grain"`
	Refresh string   `yaml:"refresh"`

	// ProducedBy 产出该桶的算法 ID 列表。
	ProducedBy []string `yaml:"produced_by"`

	// AlgoVersions 桶记录的算法版本（对应 G6 的「桶 algo_version == 注册表版本」）。
	AlgoVersions map[string]int `yaml:"algo_versions"`

	// RuleVersions 桶记录的**规则**版本（对应 G6 的规则漂移检测）。
	//
	// ★ 这条此前是漏的：`gate.CheckBucketProducersRegistered` 只查了 `produced_by`
	// 与 `algo_versions` 里的**算法**；`rule_versions` 里的**规则名**无人校验。
	// 实测往这里塞一个幽灵规则名不会让任何断言变红 ⇒ 「费率改了桶不重算」
	// 完全静默。现由 `gate.CheckBucketRuleVersionsRegistered` 关把。
	RuleVersions map[string]int `yaml:"rule_versions"`

	// Indexes 桶上的索引声明（[[month], [channel_code, month], …]）。
	Indexes [][]string `yaml:"indexes"`

	// Raw 保留原始键（供「未知键/拼写错误」探测 —— 拼错的键 YAML 不报错，
	// 只会静默丢失，这正是本仓反复出现的静默失效形态）。
	Raw map[string]any `yaml:"-"`
}

// BucketRegistry 桶注册表（加载即校验，fail-closed）。
type BucketRegistry struct {
	buckets map[string]Bucket
	order   []string
}

// Buckets 返回全部桶（按 ID 升序）；返回副本。
func (r *BucketRegistry) Buckets() []Bucket {
	out := make([]Bucket, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.buckets[id])
	}
	return out
}

// Bucket 按 ID 取桶。
func (r *BucketRegistry) Bucket(id string) (Bucket, bool) {
	b, ok := r.buckets[id]
	return b, ok
}

// Resolves 报告某桶 ID 是否在本注册表内存在（供槽的 derived source_ref 解析）。
//
// ★ 为什么由桶注册表来答：`slots/*.yaml` 里 derived 槽的 `source_ref` 指的是
// **上游物化来源**，而本仓的物化单位是**桶**（`buckets/*.yaml` 的 ID 即物化列名，
// 见 docs/03 §3.2「取自预计算桶的 gp 物化列」）。故「这个 ref 是否真实存在」
// 只有桶注册表能回答 —— 与 `pnl_month.yaml` 引幽灵算法是同一类引用完整性问题。
func (r *BucketRegistry) Resolves(ref string) bool {
	_, ok := r.buckets[strings.TrimSpace(ref)]
	return ok
}

// IDs 返回全部桶 ID（升序）。
func (r *BucketRegistry) IDs() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// RegisteredBucketIDs 返回「已注册桶」集合，供规则注册表做
// **规则 → 桶**这一侧的引用完整性（G4 第三侧：`applies_to_buckets`）。
//
// 与 RegisteredSlotIDs / RegisteredAlgorithmIDs 同构：三侧引用完整性都需要
// 「一份可判真假的注册集合」，而不是一个空 map（空 map 会让校验永远通过）。
func (r *BucketRegistry) RegisteredBucketIDs() map[string]bool {
	out := make(map[string]bool, len(r.buckets))
	for id := range r.buckets {
		out[id] = true
	}
	return out
}

// RefreshIntervalMinutes 返回该桶声明的刷新上限分钟数（0 = 无固定节律）。
//
// 这是 `Bucket.Refresh` 的**生产消费点**：把裸字符串变成可比较的时长，
// 供 IsDue 判定。此前该字段除了「非空校验」外没有任何人读它。
func (b Bucket) RefreshIntervalMinutes() int {
	mins, ok := gate.ParseRefresh(b.Refresh)
	if !ok {
		return 0
	}
	return mins
}

// IsDue 判定该桶在 now 时刻是否**到期需重算**（docs/04 §3.2）。
//
// lastRefreshed 为 nil 表示从未成功刷新 ⇒ 一律判到期（fail-closed）。
// 声明为 manual/event（无固定节律）时不参与判定 —— 返回 (false, 原因)。
//
// ★ 这是 gate.CheckRefreshDue 的**真实生产调用点**：把「桶该刷了吗」
//
//	从「无人能答」变成一条可被运维流水线消费的判定。
func (b Bucket) IsDue(lastRefreshed *time.Time, now time.Time) (bool, string) {
	return gate.CheckRefreshDue(b.ID, b.Refresh, lastRefreshed, now)
}

// AlgoToBuckets 返回「算法 ID → 产出它的桶」映射，供 gate.AffectedBuckets 使用（G6）。
//
// 这是桶注册表存在的**生产用途**：算法升级后要据此找出需重算的桶。
// 映射基于真读的 `buckets/*.yaml`，而不是任何硬编码表。
func (r *BucketRegistry) AlgoToBuckets() map[string][]string {
	out := map[string][]string{}
	for _, id := range r.order {
		for _, algo := range r.buckets[id].ProducedBy {
			out[algo] = append(out[algo], id)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// Validate 校验桶注册表：引用完整性 + 必备字段。
//
// reg 为算法注册表（用于判断 produced_by 里的算法是否真实存在）。
// registeredRules 为规则注册表（用于判断 rule_versions 里的规则是否真实存在）；
// 传 nil 表示「尚未装载规则注册表」—— 此时**只跳过规则一侧**并如实记录，
// 绝不假装通过（调用方可据 ValidateWithRules 的返回决定是否放行启动）。
//
// ★ 它把 `gate.CheckBucketProducersRegistered`（G4 反向）+ 新增的
// `gate.CheckBucketRuleVersionsRegistered`（桶 → 规则）一起从
// 「只在测试里被调用」变成**真实生产调用点** —— 判定的对象是磁盘上的真 YAML。
func (r *BucketRegistry) Validate(reg *Registry) error {
	return r.validate(reg, nil)
}

// ValidateWithRules 同上，但额外用**规则注册表**校验 rule_versions。
//
// 这是 sparkd 启动时的调用形态：先装载槽/算法 + 桶，装载规则后回填校验。
func (r *BucketRegistry) ValidateWithRules(reg *Registry, registeredRules map[string]bool) error {
	return r.validate(reg, registeredRules)
}

func (r *BucketRegistry) validate(reg *Registry, registeredRules map[string]bool) error {
	if reg == nil {
		return fmt.Errorf("桶注册表校验需要一个算法注册表（nil 会导致引用完整性恒真）")
	}

	// ① 先过 gate 的判定函数（G4 反向引用完整性）—— 这是闸门的真实调用点。
	docs := make([]gate.BucketDoc, 0, len(r.order))
	for _, id := range r.order {
		b := r.buckets[id]
		docs = append(docs, gate.BucketDoc{
			ID:           b.ID,
			ProducedBy:   b.ProducedBy,
			AlgoVersions: b.AlgoVersions,
			RuleVersions: b.RuleVersions,
			Grain:        b.Grain,
			Refresh:      b.Refresh,
		})
	}
	var bad []string
	bad = append(bad, gate.CheckBucketProducersRegistered(docs, reg.RegisteredAlgorithmIDs())...)
	if registeredRules != nil {
		bad = append(bad, gate.CheckBucketRuleVersionsRegistered(docs, registeredRules)...)
	}

	// ①b 公式自由变量绑定完整性（G4 第三侧：值绑定）。
	//
	// ★ 这是本仓「假闸门」形态在 G4 上的**第三个变种**：
	//   前两个是「桶 → 算法」（上面 ①）与「桶 → 规则」；
	//   而算法真正的执行载荷 —— `formula` —— 里的自由变量**从没被任何断言看过**。
	//   实例：`algo.gp` 公式 `rev - cogs` 依赖槽 `slot.revenue`，槽绑出的变量名是
	//   `revenue`，公式要的却是 `rev` ⇒ 内核按 Missing 处理 ⇒ 配合
	//   `missing_policy: skip`，该列在**每条数据**上都被静默跳过（全空、零报错）。
	//
	//   ★ 语义：**逐桶**校验，且只校验该桶 `produced_by` 里真的会被执行的算法。
	//   理由：BuildRow 是「一个桶一行、按该桶 produced_by 串行求值」。某个算法
	//   若没被任何桶引用，它在运行时**根本不会被执行**，要求其变量可绑定是过严的
	//   （且会让「尚未入桶的新算法」无法先落地）。反之，只要它入了桶，其公式的
	//   每个自由变量就必须能在该桶的执行顺序里绑定到，否则就是一条恒空的静默列。
	for _, bid := range r.order {
		b := r.buckets[bid]
		// 该桶的执行序：只有列在 produced_by 里、且真实注册的算法会被执行。
		var chain []gate.AlgoDoc
		upstream := map[string]map[string]bool{}
		seen := map[string]bool{}
		byID := map[string]gate.AlgoDoc{}
		for _, d := range reg.AlgoDocs() {
			byID[d.ID] = d
		}
		for _, algoID := range b.ProducedBy {
			d, ok := byID[algoID]
			if !ok {
				continue // 未注册 ⇒ 已由 ① 报出
			}
			up := map[string]bool{}
			for prev := range seen {
				up[prev] = true
			}
			upstream[algoID] = mergeMaps(upstream[algoID], up)
			seen[algoID] = true
			chain = append(chain, d)
		}
		bad = append(bad, gate.CheckFormulaVariablesBound(chain, upstream)...)
	}

	// ①c PENDING 算法不得被桶引用（否则运行时会产出恒空的静默列）。
	bad = append(bad, gate.CheckPendingAlgosNotProducing(reg.AlgoDocs(), docs)...)

	// ② 闸门之外的结构性约束（版本号为正、grain 必备）。
	for _, id := range r.order {
		b := r.buckets[id]

		for algo, v := range b.AlgoVersions {
			if v <= 0 {
				bad = append(bad, fmt.Sprintf("桶 %s 的 algo_versions[%s]=%d 非正数", id, algo, v))
			}
		}
		if len(b.Grain) == 0 {
			bad = append(bad, fmt.Sprintf("桶 %s 未声明 grain（预计算粒度是桶的定义性属性）", id))
		}
	}

	// ①d 刷新节律声明（G4 第五侧）。
	//
	// ★ 这是本仓「假闸门」形态的另一实例：`Bucket.Refresh` 全链路
	//   （YAML → Bucket → BucketDoc → registry_bucket.refresh 列）被带过，
	//   但此前对它唯一的校验是「非空字符串」⇒ `monthly_incremental` 与
	//   乱写**完全等价**，且没有任何到期判定 ⇒ 桶可能无限期陈旧而无人知。
	//   现把 gate.CheckRefreshDeclared 接成真实生产调用点。
	refreshDocs := make([]gate.RefreshDoc, 0, len(r.order))
	for _, id := range r.order {
		refreshDocs = append(refreshDocs, gate.RefreshDoc{
			BucketID: id,
			Raw:      r.buckets[id].Refresh,
		})
	}
	bad = append(bad, gate.CheckRefreshDeclared(refreshDocs)...)

	// ①e 算法的 writes_bucket 必须指向**真实桶**（G4 第六侧附）。
	//
	// ★ 这是同一个病的又一处实例（本闸门上线首刻抓到 3 处真缺陷）：
	//   `writes_bucket` **被运行时真正消费**（sparkd `currentAlgoVersions` 按它
	//   汇总桶的算法版本，G6 据此判「桶版本落后 ⇒ 需重算」），但它的值此前
	//   **从未被校验** —— `algorithms/{cogs,gmp,gp}.yaml` 都写
	//   `writes_bucket: pnl_sku_month`，而真桶 ID 是 `pnl_month`。
	//   后果：`currentAlgoVersions("pnl_month")` 恒返回空 map ⇒ G6 对该桶恒不成立。
	algoBucketDocs := make([]gate.AlgoBucketDoc, 0)
	for _, a := range reg.Algorithms() {
		algoBucketDocs = append(algoBucketDocs, gate.AlgoBucketDoc{
			AlgoID: a.ID, WritesBucket: a.WritesBucket, Status: a.Status,
		})
	}
	bad = append(bad, gate.CheckAlgoWritesBucketRegistered(algoBucketDocs, r.RegisteredBucketIDs())...)

	if len(bad) > 0 {
		return fmt.Errorf("桶注册表校验失败：\n  - %s", strings.Join(bad, "\n  - "))
	}
	return nil
}

// LoadBucketRegistry 从目录读全部 buckets/*.yaml，加载并按**算法注册表**校验。
//
// 目录为空 ⇒ 报错：静默返回空注册表会让引用完整性「因为没东西可查」而永远通过。
func LoadBucketRegistry(bucketsDir string, reg *Registry) (*BucketRegistry, error) {
	files, err := yamlFiles(bucketsDir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s 下没有桶定义（*.yaml）", bucketsDir)
	}

	r := &BucketRegistry{buckets: map[string]Bucket{}}
	for _, f := range files {
		var b Bucket
		if err := unmarshalYAML(f, &b); err != nil {
			return nil, err
		}
		if b.ID == "" {
			return nil, fmt.Errorf("%s 缺少 id 字段", f)
		}
		if _, dup := r.buckets[b.ID]; dup {
			return nil, fmt.Errorf("桶 ID 重复：%s", b.ID)
		}
		r.buckets[b.ID] = b
		r.order = append(r.order, b.ID)
	}
	sort.Strings(r.order)

	if err := r.Validate(reg); err != nil {
		return nil, err
	}
	return r, nil
}

// mergeMaps 把 b 的键并入 a（返回 a，nil 时新建）。
func mergeMaps(a, b map[string]bool) map[string]bool {
	if a == nil {
		a = map[string]bool{}
	}
	for k := range b {
		a[k] = true
	}
	return a
}

// upstreamByAlgo 计算「算法 ID → 在本算法**之前**已算出的算法 ID 集合」。
//
// 语义与 precomp.Builder.BuildRow 的运行时串行循环一致：按 produced_by 顺序
// 依次求值，每算出一个就放进变量表供下游使用。同一算法在多个桶里可能有不同的
// 上游集合，故取**并集**。
//
// 供测试与外部核对使用（生产校验路径已按桶逐条进行，见 validate 的 ①b）。
func (r *BucketRegistry) upstreamByAlgo(reg *Registry) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if reg != nil {
		for _, a := range reg.Algorithms() {
			if out[a.ID] == nil {
				out[a.ID] = map[string]bool{}
			}
		}
	}
	for _, bid := range r.order {
		seen := map[string]bool{}
		for _, algoID := range r.buckets[bid].ProducedBy {
			for prev := range seen {
				out[algoID] = mergeMaps(out[algoID], map[string]bool{prev: true})
			}
			seen[algoID] = true
		}
	}
	return out
}

// bucketFileNames 返回 buckets/ 下的文件名（供接线闸门核对目录约定）。
func bucketFileNames(dir string) ([]string, error) {
	files, err := yamlFiles(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, filepath.Base(f))
	}
	return out, nil
}

var _ = os.ReadDir // 保留 os 导入的显式用途（真实读盘是本包的存在理由）
