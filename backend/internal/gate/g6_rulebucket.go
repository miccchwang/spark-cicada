// G4 · 第十六侧：规则 ⇄ 桶 的**双向声明对平**。
//
// 动机（★ 真实缺口，同一个病的新形态 —— **第二十四个变种**）：
//
//	本仓已经反复踩过「**双向声明、零对平**」这一类病：
//
//	  * G1 契约镜像（第十四/十九个变种）：`contracts/*.ts` 是唯一真源，
//	    Go 侧有若干「与真源对齐」的镜像结构体 —— 两侧此前**没有任何东西逐字段对平**，
//	    真源加字段而镜像没跟（或反之）全程零报错；
//	  * G9 插槽模块清单（第二十一个变种）：`contracts/slot-manifest.yaml` 自称
//	    「模块通过本清单自注册」，而运行时注册表与它**互不校验** ——
//	    清单声明 6 个模块、运行时零挂载，两边各自为真。
//
//	「规则 ⇄ 桶」是同一类关系的**第三处**，而且是 G6（预计算一致性）的命门：
//
//	  规则 → 桶：`rules/*.yaml` 的 `applies_to_buckets`
//	              ——「这条规则改了要重算哪些桶」，由 `rule.Registry.AffectedBuckets`
//	              在生产侧消费；
//	  桶 → 规则：`buckets/*.yaml` 的 `rule_versions`
//	              ——「本桶的物化数据是按哪些规则的哪个版本算出来的」，由
//	              `gate.VersionDrift` / `precomp.Drift` 在生产侧消费。
//
//	两条声明**由两个不同目录、两套不同字段维护**，此前**没有任何断言把它们对起来**：
//
//	  * 规则侧 `applies_to_buckets` 只被校验「桶 ID 真实注册」
//	    （`gate.CheckRuleIDsRegistered`）；
//	  * 桶侧 `rule_versions` 只被校验「规则名真实注册 + 版本号为正」
//	    （`gate.CheckBucketRuleVersionsRegistered`）。
//
//	于是**两边可以各自成立而彼此矛盾**，后果是 G6 的两条「重算触发路径」分叉：
//
//	  ① 规则 R 声明 `applies_to_buckets: [B]`，而桶 B 的 `rule_versions` 里**没有 R**
//	     ⇒ 通过 `rule.AffectedBuckets` 能把 B 标成 stale，但
//	     `VersionDrift(B.rule_versions, current)` **永远看不见 R** ⇒
//	     「改了费率、B 重算过了没有」在漂移检测里**恒不体现**；
//	  ② 桶 B 的 `rule_versions` 里有 R，而规则 R 的 `applies_to_buckets` 里**没有 B**
//	     ⇒ `VersionDrift` 能把 B 报成漂移，但 `rule.AffectedBuckets("R")` 返回空
//	     ⇒「按规则变更重算」的流水线**漏掉 B**，报表口径静默失真。
//
//	两种方向都**零报错**：既有的每一条闸门都各自为真。
//
// 本侧把「双向一致」变成一条可失败的断言，且带**真实生产调用点**
// （`sparkd loadSpecs` → `rule.Registry.ValidateBucketLinks`）。
//
// ★ 刻意不做（宁少一条真断言，不写一条假断言）：
//
//	① **不比对「桶记录的版本值」与「规则当前版本」**。桶的 `rule_versions[R]`
//	   是**物化快照**（「本桶是按 R 的哪一版算出来的」），与规则当前版本**不等
//	   正是漂移本身** —— 那是 G6 要在**运行时**报出来的结论，不是静态配置错误。
//	   强行断言相等会把「合法的陈旧桶」判成违规（参 G4 第十侧对算法版本
//	   同型问题的处置：只对平**键集与语义**，不对平**快照值**）。
//	   版本值的对平由 `gate.VersionDrift` / `precomp.Drift` 在运行时完成。
//	② **不重复报告「引用不存在」**。桶不存在 / 规则不存在这两类已分别由
//	   `CheckRuleIDsRegistered` 与 `CheckBucketRuleVersionsRegistered` 关把；
//	   本侧只报告「两侧都真实存在、但声明彼此矛盾」的**配对**问题，
//	   避免同一处缺陷被两条闸门各报一次（那会让注入破坏的**分辨**变得含糊）。
//	③ **不做「applies_to_buckets 必须非空」**。空是合法的（该规则只影响槽、
//	   不影响任何桶），`rule.Registry.Validate` 已要求
//	   「`applies_to_slots` / `applies_to_buckets` 至少一侧非空」。
package gate

import (
	"fmt"
	"sort"
	"strings"
)

// RuleBucketLink 是一条规则的「受影响桶」声明（对应 rules/*.yaml 的 `applies_to_buckets`）。
//
// 为什么单列一个类型而不是复用 RuleDoc：RuleDoc 的 `Raw` 是 map，读它要重新做
// 键名匹配与类型断言（多一处可静默失败的地方）；本侧判定的对象就是**这一对关系**，
// 与 VersionDoc / UnitDoc / FreshnessDoc / PermissionDoc 同纪律：判定什么，就显式带什么。
type RuleBucketLink struct {
	// RuleID 规则 ID（报错时可定位）。
	RuleID string
	// AppliesToBuckets 该规则声明会影响到的桶 ID 列表（原文顺序，可含重复）。
	AppliesToBuckets []string
}

// CheckRuleBucketBidirectional 断言「规则 → 桶」与「桶 → 规则」两侧**双向一致**。
//
//	入参 rules  —— 来自真读的 `rules/*.yaml`（`rule.Registry.RuleBucketLinks()`）；
//	入参 buckets —— 来自真读的 `buckets/*.yaml`（`slot.BucketRegistry.BucketDocs()`）。
//
// 语义（两个方向各一条，互不替代）：
//
//	正向：规则 R 声明 `applies_to_buckets` 含桶 B ⇒ 桶 B 的 `rule_versions` 必须含 R；
//	反向：桶 B 的 `rule_versions` 含规则 R   ⇒ 规则 R 的 `applies_to_buckets` 必须含 B。
//
// ★ 空输入**不得**视为通过：规则集合或桶集合为空时，本判定「因为没东西可查」
// 会恒为真 —— 这正是本仓反复出现的「假闸门」形态（参 `rule.Registry.Validate`
// 对 nil 注册表的处置）。故空集合直接报违规，而不是静默放行。
//
// 返回人类可读原因（已排序、去重）；空 = 双向一致。
func CheckRuleBucketBidirectional(rules []RuleBucketLink, buckets []BucketDoc) []string {
	var violations []string

	if len(rules) == 0 {
		violations = append(violations,
			"规则集合为空 —— 规则⇄桶 双向对平会『因为没东西可查』恒真（疑似加载失败被当成通过）")
	}
	if len(buckets) == 0 {
		violations = append(violations,
			"桶集合为空 —— 规则⇄桶 双向对平会『因为没东西可查』恒真（疑似加载失败被当成通过）")
	}

	// 索引：规则 ID → 声明的桶集合（去重后升序，便于稳定报错）。
	ruleToBuckets := map[string]map[string]bool{}
	for _, r := range rules {
		id := strings.TrimSpace(r.RuleID)
		if id == "" {
			continue // 空规则 ID 由 rule.Registry.Validate 关把，本侧不重复报
		}
		set, ok := ruleToBuckets[id]
		if !ok {
			set = map[string]bool{}
			ruleToBuckets[id] = set
		}
		for _, b := range r.AppliesToBuckets {
			bid := strings.TrimSpace(b)
			if bid == "" {
				violations = append(violations, fmt.Sprintf(
					"规则 %s 的 applies_to_buckets 含空桶名", id))
				continue
			}
			set[bid] = true
		}
	}

	// 索引：桶 ID → 记录的规则集合。
	bucketToRules := map[string]map[string]bool{}
	for _, d := range buckets {
		bid := strings.TrimSpace(d.ID)
		if bid == "" {
			continue
		}
		set, ok := bucketToRules[bid]
		if !ok {
			set = map[string]bool{}
			bucketToRules[bid] = set
		}
		for r := range d.RuleVersions {
			if strings.TrimSpace(r) == "" {
				continue // 空规则名由 CheckBucketRuleVersionsRegistered 关把
			}
			set[strings.TrimSpace(r)] = true
		}
	}

	// ① 正向：规则 → 桶 声明的桶，必须在桶侧 `rule_versions` 里有回指。
	for ruleID, bs := range ruleToBuckets {
		for _, b := range sortedKeys(bs) {
			if _, exists := bucketToRules[b]; !exists {
				continue // 桶不存在 ⇒ 由 CheckRuleIDsRegistered 报，本侧不重复
			}
			if !bucketToRules[b][ruleID] {
				violations = append(violations, fmt.Sprintf(
					"规则 %s 声明 applies_to_buckets 含桶 %s，但桶 %s 的 rule_versions 未声明该规则"+
						" —— G6：规则改了该桶不会被版本漂移检测看见（两条重算触发路径分叉）",
					ruleID, b, b))
			}
		}
	}

	// ② 反向：桶 `rule_versions` 里的规则，必须在规则侧 `applies_to_buckets` 里有回指。
	for bid, rs := range bucketToRules {
		for _, ruleID := range sortedKeys(rs) {
			declared, exists := ruleToBuckets[ruleID]
			if !exists {
				continue // 规则不存在 ⇒ 由 CheckBucketRuleVersionsRegistered 报，本侧不重复
			}
			if !declared[bid] {
				violations = append(violations, fmt.Sprintf(
					"桶 %s 的 rule_versions 声明规则 %s，但规则 %s 的 applies_to_buckets 未含该桶"+
						" —— G6：规则改了该桶不会被 rule.AffectedBuckets 标出（两条重算触发路径分叉）",
					bid, ruleID, ruleID))
			}
		}
	}

	sort.Strings(violations)
	return dedupeStrings(violations)
}

// sortedKeys 返回 map 键的升序切片（稳定报错顺序）。
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dedupeStrings 去掉相邻重复项（输入需已排序）。
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
