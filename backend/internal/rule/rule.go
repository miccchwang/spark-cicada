// Package rule —— 规则集注册表（M-RULE，G4 引用完整性**第三侧**的真实生产链路）。
//
// 为什么要有这个包（承袭本仓「定义文件全仓从没被任何代码读过」这个病）：
//
//	`rules/*.yaml` 是 docs/01 §0.3 与 docs/03 §4 明写的「费率/口径的**唯一事实源**」，
//	改费率只改这里、全域生效。但在本包出现之前：
//	  * `grep "rules/"` 全仓唯一命中是 README 的一句话 —— **没有任何代码读过它**；
//	  * 于是「唯一事实源」在实现侧不成立：该目录可以被整体删除、可以被写成任意内容，
//	    而 `slot.LoadRegistry` / `slot.LoadBucketRegistry` 依然全绿；
//	  * 更贵的是 **G6 的规则漂移检测静默失效**：桶的 `rule_versions` 声明
//	    `{"rule.tk.fee": 2}`，但「规则改了 ⇒ 哪些桶要重算」的映射
//	    （`applies_to_buckets`）此前**没有来源**，因为没人读这份定义。
//
//	这与 G4 的另外两向是同一条不变量（引用完整性），只是第三侧：
//
//	    算法 → 槽     CheckSlotsRegistered              （slot.LoadRegistry）
//	    桶   → 算法   CheckBucketProducersRegistered    （slot.LoadBucketRegistry）
//	    规则 → 槽/桶  CheckRuleIDsRegistered            （**本包**）
//
//	本包把 `rules/*.yaml` 读**真文件**、过**真闸门**：
//
//	LoadRuleRegistry(dir)
//	  → 解析 YAML 为 gate.RuleDoc（Raw 保留全部原始键，供引用完整性校验）
//	  → 逐条调 gate.CheckRuleIDMatchesFilename / CheckRuleIDsRegistered / CheckPlatformFeeRule
//	  → 任一违规 ⇒ 返回 error（fail-closed：规则半残比没有更危险）
//
// 与「种子」的关系：`sql/migrations/0004_registry_seed.sql` 把规则写进
// `registry_rule_set` 表。**两处都是「规则的事实源」** ⇒ 必然漂移（本仓已有
// 多个同型事故）。因此本包提供 `CompareSeed`，并且**只对 YAML 里真实存在的
// 规则**比对 —— 这是有意的：种子比 YAML 多出的条目属「文档/种子 vs 实现的漂移」，
// 由 docs 与 CI 如实标注收口，本闸门不替它做决定（那会变成「用种子覆盖实现」）。
package rule

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// Rule 是一条规则的定义（对应 rules/*.yaml，docs/03 §4.1）。
type Rule struct {
	ID      string         `yaml:"id"`
	Version int            `yaml:"version"`
	Scope   map[string]any `yaml:"scope"`
	Items   []Item         `yaml:"items"`

	// AppliesToSlots / AppliesToBuckets = 「改了这条规则要动哪些对象」的机器可读来源。
	AppliesToSlots   []string `yaml:"applies_to_slots"`
	AppliesToBuckets []string `yaml:"applies_to_buckets"`

	// Raw 保留原始键（供「未知键/拼写错误」探测 —— 拼错的键 YAML 不报错，
	// 只会静默丢失，这正是本仓反复出现的静默失效形态）。
	Raw map[string]any `yaml:"-"`

	// File 记录来源文件名（用于 id ↔ 文件名一致性校验与排障）。
	File string `yaml:"-"`
}

// Item 是一条费率/阈值明细（docs/03 §4.1）。
type Item struct {
	ID            string  `yaml:"id"`
	Name          string  `yaml:"name"`
	Kind          string  `yaml:"kind"`
	Rate          float64 `yaml:"rate"`
	FlatPerOrder  float64 `yaml:"flat_per_order"`
	VATIncluded   bool    `yaml:"vat_included"`
	EffectiveFrom string  `yaml:"effective_from"`
}

// Registry 规则注册表（加载即校验，fail-closed）。
type Registry struct {
	rules map[string]Rule
	order []string // 稳定顺序（确定性输出/审计）

	// seedDrift 记录「YAML 有、种子没有」与「YAML 与种子不一致」的条目，
	// 供调用方上报（不在加载期直接失败：种子由迁移维护，可能滞后于仓库）。
	seedDrift []string
}

// Rules 返回全部规则（按 ID 升序）；内部顺序即加载顺序（升序）。
func (r *Registry) Rules() []Rule {
	out := make([]Rule, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.rules[id])
	}
	return out
}

// Rule 按 ID 取规则。
func (r *Registry) Rule(id string) (Rule, bool) {
	x, ok := r.rules[id]
	return x, ok
}

// IDs 返回全部规则 ID（升序）。
func (r *Registry) IDs() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// RegisteredRuleIDs 返回「已注册规则」集合，供桶注册表做
// **桶 → 规则**这一侧的引用完整性（G4 第三方向：`rule_versions`）。
//
// 与 RegisteredSlotIDs / RegisteredAlgorithmIDs / RegisteredBucketIDs 同构：
// 「一份可判真假的注册集合」是引用完整性成立的前提，空 map 会让校验永远通过。
func (r *Registry) RegisteredRuleIDs() map[string]bool {
	out := make(map[string]bool, len(r.rules))
	for id := range r.rules {
		out[id] = true
	}
	return out
}

// Doc 把规则翻成 gate.RuleDoc（Raw 原样带上 ⇒ 闸门能真扫引用字段）。
func (r *Registry) Doc(id string) (gate.RuleDoc, bool) {
	x, ok := r.rules[id]
	if !ok {
		return gate.RuleDoc{}, false
	}
	return x.doc(), true
}

func (x Rule) doc() gate.RuleDoc {
	items := make([]gate.RuleItem, 0, len(x.Items))
	for _, it := range x.Items {
		items = append(items, gate.RuleItem{
			ID:            it.ID,
			Name:          it.Name,
			Rate:          it.Rate,
			FlatPerOrder:  it.FlatPerOrder,
			VATIncluded:   it.VATIncluded,
			EffectiveFrom: it.EffectiveFrom,
		})
	}
	return gate.RuleDoc{ID: x.ID, Version: x.Version, Scope: x.Scope, Items: items, Raw: x.Raw}
}

// ApplyToBuckets 返回「规则 id → 受影响的桶」，供 G6 规则漂移检测使用。
//
// ★ 这是规则注册表存在的**生产用途**：规则升 version 后要据此找出需重算的桶。
// 映射基于真读的 `rules/*.yaml`，而不是任何硬编码表。
func (r *Registry) ApplyToBuckets() map[string][]string {
	out := map[string][]string{}
	for _, id := range r.order {
		buckets := append([]string(nil), r.rules[id].AppliesToBuckets...)
		sort.Strings(buckets)
		out[id] = buckets
	}
	return out
}

// AffectedBuckets 返回「这些规则被改动后需要重算的桶」（去重、升序）。
//
// 与 gate.AffectedBuckets（算法侧）成对：G6 的「仅重算受影响桶」两个输入
// 至此都有真实来源，而不是空映射（空映射会让 G6-3 恒真）。
func (r *Registry) AffectedBuckets(changedRuleIDs ...string) []string {
	seen := map[string]bool{}
	for _, id := range changedRuleIDs {
		x, ok := r.rules[id]
		if !ok {
			continue
		}
		for _, b := range x.AppliesToBuckets {
			seen[b] = true
		}
	}
	out := make([]string, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// Validate 用 **gate 的三条规则判定函数**校验注册表本身。
//
// ★ 这是这些判定函数第一次被生产代码调用：判定的对象是**磁盘上的真 YAML**，
// 而不是测试里手搓的假数据。违规即返回 error（fail-closed）。
//
// registeredSlots / registeredBuckets 为 nil 时，引用完整性会「因为没东西可查」
// 而永远通过 —— 所以 nil 直接视为违规（这正是本仓反复出现的「假闸门」形态）。
func (r *Registry) Validate(registeredSlots, registeredBuckets map[string]bool) error {
	if registeredSlots == nil || registeredBuckets == nil {
		return fmt.Errorf(
			"规则注册表校验需要槽与桶的注册表（nil 会让引用完整性恒真）: slots=%v buckets=%v",
			registeredSlots != nil, registeredBuckets != nil)
	}

	var violations []string
	docs := make([]gate.RuleDoc, 0, len(r.order))
	for _, id := range r.order {
		x := r.rules[id]
		docs = append(docs, x.doc())
		// ① 文件名 ↔ id 一致性（人类按文件名找、程序按 id 找，两侧必须对得上）
		violations = append(violations, gate.CheckRuleIDMatchesFilename(x.doc(), x.File)...)
		// ② 逐项计提的费率自洽（非法费率不会报错，只会把整张损益表算错一个系数）
		violations = append(violations, gate.CheckPlatformFeeRule(x.doc())...)
		// ③ 结构性：scope 非空、applies_to_* 至少一侧非空（否则规则改完没人知道要重算什么）
		if len(x.Scope) == 0 {
			violations = append(violations, fmt.Sprintf("规则 %s 未声明 scope", x.ID))
		}
		if len(x.AppliesToSlots) == 0 && len(x.AppliesToBuckets) == 0 {
			violations = append(violations,
				fmt.Sprintf("规则 %s 既未声明 applies_to_slots 也未声明 applies_to_buckets"+
					"（规则变更后无法定位受影响对象）", x.ID))
		}
	}
	// ④ 引用完整性（规则 → 槽 / 桶）
	violations = append(violations, gate.CheckRuleIDsRegistered(docs, registeredSlots, registeredBuckets)...)

	if len(violations) > 0 {
		return fmt.Errorf("规则注册表校验失败（G4 第三侧）：\n  - %s", strings.Join(violations, "\n  - "))
	}
	return nil
}

// SeedDrift 返回「YAML 与迁移种子不一致」的描述（空 = 一致）。
func (r *Registry) SeedDrift() []string {
	out := make([]string, len(r.seedDrift))
	copy(out, r.seedDrift)
	return out
}

// LoadRuleRegistry 从目录读全部 rules/*.yaml，加载并按槽/桶注册表校验。
//
// 目录为空 ⇒ 报错：静默返回空注册表会让引用完整性「因为没东西可查」而永远通过。
func LoadRuleRegistry(dir string, registeredSlots, registeredBuckets map[string]bool) (*Registry, error) {
	files, err := yamlFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s 下没有规则定义（*.yaml）—— "+
			"规则集是费率/口径的唯一事实源，空目录会让它静默失效", dir)
	}

	r := &Registry{rules: map[string]Rule{}}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %w", f, err)
		}
		var x Rule
		if err := yaml.Unmarshal(raw, &x); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", f, err)
		}
		// Raw 必须在结构体解码**之外**单独解一遍：结构体字段是白名单，
		// 而引用完整性要看的正是「原始键里有没有 applies_to_*」。
		var generic map[string]any
		if err := yaml.Unmarshal(raw, &generic); err != nil {
			return nil, fmt.Errorf("解析 %s 的原始键失败: %w", f, err)
		}
		x.Raw = generic
		x.File = filepath.Base(f)

		if strings.TrimSpace(x.ID) == "" {
			return nil, fmt.Errorf("%s 缺少 id 字段", filepath.Base(f))
		}
		if _, dup := r.rules[x.ID]; dup {
			return nil, fmt.Errorf("规则 ID 重复：%s", x.ID)
		}
		r.rules[x.ID] = x
		r.order = append(r.order, x.ID)
	}
	sort.Strings(r.order)

	if err := r.Validate(registeredSlots, registeredBuckets); err != nil {
		return nil, err
	}
	return r, nil
}

// ───────────────────────── 与迁移种子的一致性 ─────────────────────────

// SeedRule 是迁移种子里的一条规则（`registry_rule_set` 的一行）。
type SeedRule struct {
	ID      string
	Version int
	Items   []Item
}

// CompareSeed 把仓库 YAML 与迁移种子逐条比对，产出漂移描述并记入注册表。
//
// ★ 只对**YAML 里真实存在的规则**比对：
// 种子比 YAML 多出的条目属于「种子/文档 vs 实现」的漂移，由 docs 与 CI 如实标注，
// 本函数不替它做决定 —— 否则就成了「用种子覆盖实现」，正是本仓反复避免的做法。
func (r *Registry) CompareSeed(seeds []SeedRule) []string {
	byID := map[string]SeedRule{}
	for _, s := range seeds {
		byID[s.ID] = s
	}
	var drift []string
	for _, id := range r.order {
		x := r.rules[id]
		seed, ok := byID[id]
		if !ok {
			drift = append(drift, fmt.Sprintf(
				"规则 %s（%s）在 YAML 中存在，但迁移种子 registry_rule_set 里没有对应行"+
					"（部署后该费率不会被平台读到）", id, x.File))
			continue
		}
		// 版本必须一致：版本是「改了规则要重算哪些桶」的判定依据（G6）。
		// YAML version=6 而库里 version=2，则桶永远认为「规则没变」。
		if seed.Version != x.Version {
			drift = append(drift, fmt.Sprintf(
				"规则 %s 版本漂移：YAML=%d 种子=%d（G6 会误判该规则未变更 ⇒ 桶不重算）",
				id, x.Version, seed.Version))
		}
		// 种子的 items 只有一条汇总项是**已知且如实标注**的形态（0004 尚未拆项），
		// 故只在「种子有多项」时逐项比对费率，避免把「尚未拆项」误报为漂移。
		if len(seed.Items) > 1 {
			got := map[string]float64{}
			for _, it := range seed.Items {
				got[it.ID] = it.Rate
			}
			for _, it := range x.Items {
				if v, ok := got[it.ID]; ok && v != it.Rate {
					drift = append(drift, fmt.Sprintf(
						"规则 %s 项 %s 费率漂移：YAML=%.6f 种子=%.6f", id, it.ID, it.Rate, v))
				}
			}
		}
	}
	r.seedDrift = drift
	return drift
}

// ParseSeedItems 从迁移 SQL 的 `registry_rule_set` 行里抽取 (id, version, items JSON)。
//
// ★ 为什么不连库读：本机与 CI 的静态作业**没有数据库**（真库作业另行承担，见 docs/05 §3.1）。
// 「种子 ↔ 仓库定义」这种纯文本漂移，正是**不需要数据库就能拦**的那一类，
// 且它恰是本仓有过多次真实事故的类别。真库侧另有 `store/integration_test.go` 兜底。
//
// 只认 `('rule.xxx', N, …)` 这一显式形态，且**续行衔接**：
// SQL 里每个元组跨多行（`('rule.tk.fee', 1,` / `'{…}'::jsonb,` / `'[…]'::jsonb),`），
// 因此先把 `VALUES` 之后的内容按**顶层逗号**重新聚拢成元组再解析 ——
// 逐行扫描会漏掉 items（曾实测：一条都解不出来 ⇒ 闸门永远绿灯）。
//
// 解析不出任何一条 ⇒ 返回空切片，由调用方判为失败（绝不静默「通过」）。
func ParseSeedItems(sqlText string) []SeedRule {
	// ① 找到 `registry_rule_set` 且含 VALUES 的语句，截取其 VALUES 之后的正文。
	idx := strings.Index(sqlText, "registry_rule_set")
	if idx < 0 {
		return nil
	}
	body := sqlText[idx:]
	if v := strings.Index(body, "VALUES"); v >= 0 {
		body = body[v+len("VALUES"):]
	} else {
		return nil
	}
	// 语句终止：`ON CONFLICT`（若存在）
	if e := strings.Index(body, "ON CONFLICT"); e >= 0 {
		body = body[:e]
	}

	// ② 按括号深度聚拢成元组。
	var tuples []string
	depth := 0
	inQuote := false
	start := -1
	for i, r := range body {
		switch r {
		case '\'':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				if depth == 0 {
					start = i
				}
				depth++
			}
		case ')':
			if !inQuote {
				depth--
				if depth == 0 && start >= 0 {
					tuples = append(tuples, body[start:i+1])
					start = -1
				}
			}
		}
	}

	// ③ 每个元组按顶层逗号切字段。
	var out []SeedRule
	for _, t := range tuples {
		fields := splitTupleFields(t)
		if len(fields) < 2 {
			continue
		}
		id := strings.Trim(strings.TrimSpace(fields[0]), "'")
		if !strings.HasPrefix(id, "rule.") {
			continue
		}
		ver := 0
		if _, err := fmt.Sscanf(strings.TrimSpace(fields[1]), "%d", &ver); err != nil {
			continue
		}
		var items []Item
		for i := len(fields) - 1; i >= 2; i-- {
			f := strings.TrimSpace(fields[i])
			f = strings.TrimSuffix(f, "::jsonb")
			f = strings.TrimSpace(f)
			f = strings.Trim(f, "'")
			if !strings.HasPrefix(f, "[") {
				continue
			}
			// items 是 **RuleItem 的 JSON**；用 yaml 解析器可兼容 JSON（JSON 是 YAML 子集）。
			if err := yaml.Unmarshal([]byte(f), &items); err != nil {
				items = nil
			}
			break
		}
		out = append(out, SeedRule{ID: id, Version: ver, Items: items})
	}
	return out
}

// splitTupleFields 把一个元组文本 `('a', 1, '{...}'::jsonb, '[...]'::jsonb)` 按
// **顶层逗号**切成字段，返回时**去掉元组最外层括号与每个字段的前导/尾随空白**。
//
// ★ 元组最外层的 `(` `)` 必须先剥掉再统计深度：否则切分时深度基线是 1，
// 「顶层逗号」永远不会命中（曾实测：一条都切不出来 ⇒ 闸门永远绿灯）。
// 引号内的括号与逗号一律不参与（JSON 的 `{}` `[]` 与中文文本都可能含它们）。
func splitTupleFields(t string) []string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "(")
	t = strings.TrimSuffix(t, ")")

	var out []string
	depth := 0
	inQuote := false
	start := 0
	for i, r := range t {
		switch r {
		case '\'':
			inQuote = !inQuote
		case '(', '[', '{':
			if !inQuote {
				depth++
			}
		case ')', ']', '}':
			if !inQuote {
				depth--
			}
		case ',':
			if !inQuote && depth == 0 {
				out = append(out, strings.TrimSpace(t[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(t[start:]))
	return out
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
