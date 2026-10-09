// G4 · 第十五侧：规则项的「生效期 + 契约外键」闭环。
//
// 动机（★ 真实缺口，同一个病的新形态 —— **第二十三个变种**）：
//
//	规则的**生效期**（`effective_from` / `effective_to`）在本仓被**三处**同时声明：
//
//	  * `contracts/compute.proto` —— `RuleItem.effective_to`（field 7）、
//	    `EvalRuleRequest.as_of`、`EvalRuleResponse.active_items` / `dropped_items`
//	    （即「按生效期选版本」的线格式契约）；
//	  * `docs/02-模块规格说明书.md` ——「时间生效：`effective_from` / `effective_to`，
//	    同一规则可多版本共存」；
//	  * `compute/src/gate.rs` —— `RuleItem.effective_to` + `is_effective()`
//	    真的实现了 `[effective_from, effective_to)` 的选取语义。
//
//	而 Go 侧（规则注册表 M-RULE 的**真实生产链路**）此前是：
//
//	  * `gate.RuleItem` / `rule.Item` **只有 `EffectiveFrom`，没有 `EffectiveTo`**
//	    ⇒ 契约里有的字段在实现侧**根本不存在**；
//	  * `EffectiveFrom` 被 YAML 解析进结构体后，**没有任何判定消费它** ——
//	    典型「被读被存却无判定」的死字段（全仓 `grep EffectiveFrom` 只有
//	    「字段声明」与「解析赋值」两处）；
//	  * `rules/*.yaml` 里 `effective_from` 已经是**真数据**
//	    （`platform_commission` 2026-07-01 / `commerce_growth_fee` 2026-05-01），
//	    却**零效果**：费率在任何日期都按同一套数字计提；
//	  * Rust 的 `is_effective` / `eval_rules` **只被 Rust 单测调用**，
//	    CLI `--eval` 不暴露它、Go 从不调用 ⇒ 该语义在生产路径上**不可达**。
//
//	更贵、也更隐蔽的是**静默丢弃**：`yaml.v3` 对结构体未声明的键**不报错**。
//	于是有人按 `docs/02` 的写法在 `rules/*.yaml` 里加 `effective_to: 2026-06-01`
//	（期望费率到期），该键会被**悄悄丢掉**、费率**永不到期**、全程零报错 ——
//	写的人以为生效了，实际零效果。`gate.RuleDoc.Raw` 的注释早已声称
//	「保留原始键（供**未知键/拼写错误**探测）」，但**该探测函数此前并不存在** ——
//	一句承诺了能力的注释，本身就是同一个病。
//
// 本文件把三件事变成可失败的断言（且都有**真实生产调用点**：
// `rule.LoadRuleRegistry → Registry.Validate`，由 `sparkd` 启动期调用）：
//
//  1. `RuleItemKnownKeys()` —— 规则项合法键集的**唯一**来源（白名单单点定义，
//     契约字段集只能从这里改，不允许各处散落字面量）；
//  2. `CheckRuleItemUnknownKeys()` —— 规则项里出现契约外的键 ⇒ **硬失败**。
//     这就是「把静默丢弃变成加载即报错」，也正是本仓的判别判据
//     「把真源文本改一处，测试会不会红？」的落地；
//  3. `CheckRuleItemWindowFields()` —— 生效期字段的**形态**（`YYYY-MM-DD` 或空）
//     与**次序**（`from < to`）校验，并用 `RuleItemEffective` **自证**语义是
//     `[from, to)`（含起不含止：`as_of == from` 生效、`as_of == to` 失效），
//     与 `compute/src/gate.rs` 同源。
//
// ★ 刻意不做（宁少一条真断言，不写一条假断言）：
//   - 不校验日期是否**真实存在**（`2026-02-31` 会通过）—— 那需要日历库，
//     而本侧要拦的是「键被静默丢弃」与「窗口写反」，不是历法；
//   - 不裁定「哪条费率在哪个日期该生效」的业务口径 —— 那是 `docs/06` F14/F15 的决策；
//   - 不把 `EvalRule` 的 `active_items`/`dropped_items` 落成生产 API ——
//     当前全仓**没有**任何真实调用点需要「按 as_of 取生效项」，
//     凭空造一个只被测试调用的 API 恰恰是本仓反复踩的坑。语义先以
//     `RuleItemEffective` 固化，等真实消费者出现再接（见 docs/06 F20）。
package gate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// isoDateRe 匹配 `YYYY-MM-DD`（零填充）。只做**形态**校验，不判历法合法性。
//
// 为什么零填充是硬要求：`RuleItemEffective` 与 Rust 的 `is_effective` 都按
// **字符串序**比较日期；`2026-7-1` 与 `2026-07-01` 的字典序不同，
// 混用会让「早于/不早于」的判定**静默错位**。
var isoDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// RuleItemKnownKeys 返回规则项（`rules/*.yaml` 的 `items[]`）**唯一**的合法键集合。
//
// ★ 这是「契约字段集」的单点定义：`CheckRuleItemUnknownKeys` 只认这里列出的键。
// 要新增一个字段，必须**同时**改这里与 `rule.Item` 的 yaml 标签，否则
// 写进 YAML 的该键会被 `yaml.v3` 静默丢弃 —— 这正是本侧要拦的东西。
//
// 键的来源：`contracts/compute.proto: RuleItem`（id/name/rate/flat_per_order/
// vat_included/effective_from/effective_to）+ `rules/*.yaml` 实际在用且
// `rule.Item` 已建模的 `kind`（`rules/cost.gate.yaml` 的 `kind: threshold`）。
func RuleItemKnownKeys() []string {
	return []string{
		"id",
		"name",
		"kind",
		"rate",
		"flat_per_order",
		"vat_included",
		"effective_from",
		"effective_to",
	}
}

// RuleItemEffective 判定规则项在 `asOf`（`YYYY-MM-DD`）是否生效。
//
// 区间语义 **`[effective_from, effective_to)`**（含起不含止），与
// `compute/src/gate.rs::is_effective` **同源**（对应 proto 的
// `EvalRuleRequest.as_of` → `EvalRuleResponse.active_items`/`dropped_items`）：
//
//   - `effective_from` 为空 ⇒ 无下界（自始生效）；
//   - `effective_to` 为空 ⇒ 无上界（永不失效）；
//   - `asOf == effective_from` ⇒ **生效**（含起）；
//   - `asOf == effective_to` ⇒ **失效**（不含止）。
//
// 返回 `(是否生效, 原因)`；原因供审计复盘「这条费率当时为什么没生效」。
//
// ★ 空基准日 ⇒ fail-closed 判**不生效**：判定基准不明时按「不可用」处理，
// 而不是按「全窗口都算生效」放行（后者会把「没传 as_of」静默变成「费率永远有效」）。
func RuleItemEffective(item RuleItem, asOf string) (bool, string) {
	asOf = strings.TrimSpace(asOf)
	if asOf == "" {
		return false, "求值基准日为空（无法判定生效期，fail-closed）"
	}
	from := strings.TrimSpace(item.EffectiveFrom)
	to := strings.TrimSpace(item.EffectiveTo)
	if from != "" && asOf < from {
		return false, fmt.Sprintf("as_of=%s 早于 effective_from=%s（未生效）", asOf, from)
	}
	if to != "" && asOf >= to {
		return false, fmt.Sprintf("as_of=%s 不早于 effective_to=%s（已失效；上界不含）", asOf, to)
	}
	return true, ""
}

// CheckRuleItemWindowFields 校验规则项生效期字段的**形态**与**次序**。
//
// 三类违规（任一命中即返回）：
//
//	① `effective_from` / `effective_to` 非空却不是 `YYYY-MM-DD`；
//	② 两值都合法但 `from >= to` ⇒ **空窗**：该费率在**任何**日期都不生效
//	   （写的人以为填了个区间，实际把这条项写死了）；
//	③ 语义自证：`RuleItemEffective` 必须给出「含起不含止」的结论
//	   （`as_of == from` 生效、`as_of == to` 失效）—— 把「语义漂移」也变成红灯，
//	   而不只是「字段写没写」。
func CheckRuleItemWindowFields(doc RuleDoc) []string {
	var out []string
	for _, it := range doc.Items {
		label := fmt.Sprintf("规则 %s 项 %s", doc.ID, it.ID)
		from := strings.TrimSpace(it.EffectiveFrom)
		to := strings.TrimSpace(it.EffectiveTo)

		fromOK := from == "" || isoDateRe.MatchString(from)
		toOK := to == "" || isoDateRe.MatchString(to)
		if !fromOK {
			out = append(out, fmt.Sprintf(
				"%s 的 effective_from=%q 不是 YYYY-MM-DD（日期按**字符串序**比较，非零填充会静默错位）",
				label, it.EffectiveFrom))
		}
		if !toOK {
			out = append(out, fmt.Sprintf(
				"%s 的 effective_to=%q 不是 YYYY-MM-DD（日期按**字符串序**比较，非零填充会静默错位）",
				label, it.EffectiveTo))
		}
		if !fromOK || !toOK || from == "" || to == "" {
			continue
		}
		if !(from < to) {
			out = append(out, fmt.Sprintf(
				"%s 的生效期为空窗：effective_from=%s 不早于 effective_to=%s ⇒ "+
					"该费率在**任何**日期都不生效（区间是 [from, to)，含起不含止）",
				label, from, to))
			continue
		}
		if ok, why := RuleItemEffective(it, from); !ok {
			out = append(out, fmt.Sprintf(
				"%s 在 effective_from=%s 当日应生效（含起），实际判为不生效：%s", label, from, why))
		}
		if ok, _ := RuleItemEffective(it, to); ok {
			out = append(out, fmt.Sprintf(
				"%s 在 effective_to=%s 当日应失效（不含止），实际判为生效", label, to))
		}
	}
	return out
}

// CheckRuleItemUnknownKeys 断言规则项里**没有契约外**的键。
//
// ★ 这是本侧的核心：`yaml.v3` 对结构体未声明的键**不报错**，只静默丢弃。
// 把 `effective_to`（`docs/02` 认可、proto 有、Rust 有）或任意拼写错误
// （`effective_form`）写进 YAML，此前**没有任何东西会变红**。
//
// fail-closed：`doc.Raw` 为空或 `items` 形态异常 ⇒ 直接报违规
// （「读不到原始键」不能当成「没有契约外的键」—— 那正是「空集合让校验恒真」）。
func CheckRuleItemUnknownKeys(doc RuleDoc) []string {
	known := map[string]bool{}
	for _, k := range RuleItemKnownKeys() {
		known[k] = true
	}

	rawItems, ok := rawRuleItems(doc)
	if !ok {
		return []string{fmt.Sprintf(
			"规则 %s 的原始键不可读（Raw 为空或 items 形态异常）—— "+
				"无法核对规则项是否有契约外的键；yaml.v3 对未声明的键**不报错**，"+
				"故此处必须 fail-closed（Raw 必须由 LoadRuleRegistry 填充）", doc.ID)}
	}

	var out []string
	for i, m := range rawItems {
		id := ""
		if v, has := m["id"]; has {
			id = strings.TrimSpace(fmt.Sprintf("%v", v))
		}
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		var unknown []string
		for k := range m {
			if !known[k] {
				unknown = append(unknown, k)
			}
		}
		sort.Strings(unknown)
		for _, k := range unknown {
			out = append(out, fmt.Sprintf(
				"规则 %s 项 %s 含契约外的键 %q —— 该键会被 yaml 解析**静默丢弃**"+
					"（写的人以为生效了，实际零效果）；合法键：%s",
				doc.ID, id, k, strings.Join(RuleItemKnownKeys(), "/")))
		}
	}
	sort.Strings(out)
	return out
}

// CheckRuleItems 是规则项校验的**聚合入口**（生产调用点：rule.Registry.Validate）。
//
// 单列一个聚合函数，是为了让「生产接线」只有一处、且可被
// `TestWiring_*` 精确钉住（散落的调用点会让「漏接一条」再次静默）。
func CheckRuleItems(doc RuleDoc) []string {
	var out []string
	out = append(out, CheckRuleItemUnknownKeys(doc)...)
	out = append(out, CheckRuleItemWindowFields(doc)...)
	return out
}

// rawRuleItems 取出规则 YAML 原始键里的 `items[]`（每项为键值映射）。
//
// 返回 ok=false 表示**读不到**（Raw 为空 / items 缺失 / 形态不是映射列表）——
// 调用方必须 fail-closed，绝不把「读不到」当作「没问题」。
//
// 兼容 `map[string]any`（yaml.v3 对字符串键的默认产物）与 `map[any]any`
// （某些解码路径会给出）两种形态。
func rawRuleItems(doc RuleDoc) ([]map[string]any, bool) {
	if doc.Raw == nil {
		return nil, false
	}
	raw, has := doc.Raw["items"]
	if !has {
		// 结构体里也没有 items ⇒ 该规则确实没有明细项（合法：纯门限规则）；
		// 但若结构体有 items 而原始键里没有，说明两侧对不上 ⇒ fail-closed。
		if len(doc.Items) > 0 {
			return nil, false
		}
		return nil, true
	}
	list, isList := raw.([]any)
	if !isList {
		return nil, false
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		switch m := e.(type) {
		case map[string]any:
			out = append(out, m)
		case map[any]any:
			conv := make(map[string]any, len(m))
			for k, v := range m {
				conv[fmt.Sprintf("%v", k)] = v
			}
			out = append(out, conv)
		default:
			return nil, false
		}
	}
	return out, true
}
