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

	// ② 闸门之外的结构性约束（版本号为正、grain/refresh 必备）。
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
		if strings.TrimSpace(b.Refresh) == "" {
			bad = append(bad, fmt.Sprintf("桶 %s 未声明 refresh 策略", id))
		}
	}

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
