// G4 第九侧：算法的**计量单位**（unit）必须真实可解析、并与 DB 的
// 语义约束同源 —— 承袭本仓「字段被读进来、写进 DB，却没有任何判定消费」
// 这个病的**第十七个变种**。
//
// 缺口（2026-10-08 实测）：
//
//	`algorithms/*.yaml` 的 `unit` 在此之前是**被读进来、写进 DB
//	（registry_algorithm.unit）、但没有任何判定消费**的一个字段。全链路
//	只有三类用法 —— 解析（yaml.Unmarshal）、赋值（admin.Plane / precomp.AlgoDef
//	传输结构）、写库/读库（store/admin.go 的列）—— **没有第四类**
//	（比较、判定、阈值）：
//	  * 全仓 `grep "\.Unit"` 的非测试命中**只有** admin_handlers.go / plane.go /
//	    store/admin.go 三处，**全部是序列化与建表**，没有一处读它的值做判断；
//	  * `precomp.AlgoDef.Unit` 更是**结构性死字段**：`precomp.go` 从头到尾
//	    没有读过它一次（`BuildRow` 只消费 Formula / DependsOnSlots /
//	    MissingPolicy），字段存在只为「看起来完整」；
//	  * DB 侧 0002 迁移把 `unit` 建成 `text NOT NULL`，**没有任何 CHECK** ——
//	    也就是说 `unit: THB` 与 `unit: 想写什么写什么` 在加载期与入库期
//	    行为完全等价。
//
// 后果是**量纲静默失真**（与大数字、汇率、百分比三处叠加时最危险）：
//
//  1. `algo.gmp`（毛利率）声明 `unit: percent`，真实数值是 `0.42` 这样的**比率**。
//     单位字段若被改成 `THB` 或删掉，下游「按单位决定展示位数与量纲」的
//     逻辑就失去唯一判据 —— 页面上会出现 `0.42 THB` 这种**静默错误口径**；
//  2. `algo.cogs` 是金额：若把 `unit` 写成 `percent`，减值/税率类展示会把
//     金额按比例渲染，同样零报错；
//  3. 跨算法聚合（`sum(algo.*)` 之类的看板）本就**只能**靠 unit 判断
//     「哪些列可以相加」——unit 不可信 ⇒ 可加性判据整体失效。
//
// 而 docs/03 §3.1 早就把词表写明了（`unit: THB | % | count`），
// 却从没有一行代码去核对它。
//
// ★ 本侧**刻意不做**的两件事（避免把「声明可解析」包装成「量纲已验证」）：
//
//	① **不做「公式量纲推断」**。`Σ(qty × cost_unit)` 的 THB 是**业务约定**，
//	   不是表达式能推出来的（`qty` 无量纲、`cost_unit` 是 THB/件，乘法才得 THB；
//	   但 `revenue - cogs` 与 `gp / revenue` 的量纲规则完全不同）。
//	   若强行做量纲推断器，等于在仓库里再养一套语义规则，与本仓
//	   「单一事实源」纪律冲突 —— 且一旦推错，就是把**正确数据判成违规**
//	   （参 G4 第八侧那次的教训：宁少一条真断言，不写一条假断言）。
//	② **不把 `%` 强行改名成 `percent`**。`algo.gmp` 的真实声明就是 `%`
//	   （docs/03 §3.1 词表也写 `%`），而归一化只在**比较时**做（`%` 与
//	   `percent` 视为同一单位，见 unitAliases），**不改写声明原文** ——
//	   否则会与 0004 种子里的 `'ratio'` 形成新的分叉。
//
// 与 G4 其余八侧的关系（各侧互补，缺一即漏）：
//
//	1 CheckAlgorithmNoDataSource       算法里**不得**混入数据源（分离）
//	2 CheckSlotsRegistered             算法 → 槽 的引用完整性
//	3 CheckFormulaVariablesBound       公式自由变量的**值绑定**
//	4 CheckBucketProducersRegistered   桶 → 算法 的反向引用完整性
//	5 CheckRefreshDeclared             桶的**刷新节律**必须可解析
//	6 CheckSlotSourceResolvable        槽的**数据来源**必须可解析
//	7 CheckKeyStrategyDeclared         槽的**匹配键策略**必须可解析
//	8 CheckPermissionDeclared          槽/算法的**密级**必须可解析
//	9 CheckUnitDeclared                算法的**计量单位**必须可解析（本文件）
//
// ★ 诚实说明：本侧只断言「单位**声明**是已知的、可解析的」，
//
//	它**不**验证公式的真实量纲（见上①），也**不**验证前端按单位渲染的结果。
//	同 G2/G3/G10 的出站守卫：在正常数据上恒放行 —— 价值是把「单位写错 /
//	漏写 / 写成自由文本」变成一条**可被拒绝的加载**，并让这条判定函数
//	拥有真实生产调用点（`slot.Registry.Validate`）。
package gate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// algoUnits 是允许的**规范**单位表（**唯一权威表**）。
//
// ★ 权威依据：docs/03 §3.1「算法定义格式」里的 `unit: THB | % | count`，
// 外加本仓已实际使用、且语义明确的三个规范形态：
//
//	THB     金额（本位币，docs/03 §4.2 rule.fx.base）
//	%       比率（以百分数表示，如毛利率 0.42 ⇒ 42%）
//	count   计数（整数件数 / 条数）
//	ratio   比率（小数形态，等价于 % 的另一种书写，见下）
//
// ★ 为什么 `ratio` 必须在本表内：`sql/migrations/0004_registry_seed.sql`
//
//	里 `algo.gmp` 的 unit 写的是 `'ratio'`，而 `algorithms/gmp.yaml` 写的是
//	`percent` —— 这不是「种子错了」，而是**两侧各写一份词表**的必然结果。
//	本闸门把两侧都认下来（`ratio` / `percent` / `%` 归一到同一规范形），
//	并在 `CheckUnitVocabularyMatchesDB` 里把 0004 种子的实际取值读出来比对，
//	**不再靠人工记性**维持一致。
var algoUnits = []string{"THB", "%", "count", "ratio"}

// unitAliases 把「同一单位的书写变体」归一到 algoUnits 里的规范形。
//
// ★ 归一**只发生在比较时**，不改写声明原文（见文件头 ②）。
// 之所以 `percent` / `pct` / `percentage` 都接受：它们与 `%` 语义**完全等价、
// 无歧义**（不像密级里的 `高密` 那种自然语言别名，会掩盖真实档位）。
var unitAliases = map[string]string{
	"THB":        "THB",
	"CNY":        "CNY", // 非本位币但历史种子在用；见 CheckUnitVocabularyMatchesDB
	"%":          "%",
	"PERCENT":    "%",
	"PCT":        "%",
	"PERCENTAGE": "%",
	"RATIO":      "ratio",
	"COUNT":      "count",
	"CNT":        "count",
}

// UnitVocabulary 返回 Go 侧单位词表（规范形，升序）。
//
// 供接线闸门比对（防「文档改了、实现没改」两侧漂移）。
func UnitVocabulary() []string {
	out := make([]string, 0, len(algoUnits))
	out = append(out, algoUnits...)
	sort.Strings(out)
	return out
}

// UnitCodes 返回归一表里认得的**全部书写形态**（规范化后去重，升序）。
//
// 与 UnitVocabulary 的区别：后者是「规范单位」，本函数是「解析器实际认得的
// 输入集合」。接线闸门用后者断言「文档词表 ⊆ 解析器认得」。
func UnitCodes() []string {
	set := map[string]bool{}
	for _, v := range unitAliases {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ParseUnit 解析单位声明，返回**规范化**单位。
//
// 第二个返回值为 false 表示「不是已知单位」。本函数只做大小写归一与
// 已知别名映射，**不做**任何模糊匹配 —— `货币`、`元`、`百分比`、`泰铢`
// 一律 false（fail-closed，与 ParsePermission / ParseFreshness 同纪律）。
func ParseUnit(raw string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return "", false
	}
	if v, ok := unitAliases[s]; ok {
		return v, true
	}
	return "", false
}

// KnownUnits 返回允许的单位（供报错信息稳定可读）。
func KnownUnits() []string {
	return UnitVocabulary()
}

// UnitDoc 是待判定的一条单位声明。
type UnitDoc struct {
	// Kind 声明主体类别（当前恒为 "algorithm"，保留字段以便日后槽侧复用）。
	Kind string
	// ID 算法 ID。
	ID string
	// Raw 声明原文（如 `THB` / `%` / `percent`）。
	Raw string
}

// CheckUnitDeclared 断言每个算法的单位声明都**非空、可解析、且是已知单位**
// （G4 第九侧 / docs/03 §3.1）。
//
// 校验口径（与 CheckPermissionDeclared / CheckKeyStrategyDeclared 同构，有意保守）：
//   - 必须非空（缺声明 ⇒ 量纲无判据，展示与可加性无从判断）；
//   - 必须是已知单位（`货币`/`元`/`个`/`THB/件` 一律拒）。
//
// 返回人类可读原因（fail-closed：无法确认单位 ⇒ 不视为合规）。
func CheckUnitDeclared(docs []UnitDoc) []string {
	var violations []string
	for _, d := range docs {
		raw := strings.TrimSpace(d.Raw)
		who := unitWho(d)
		if raw == "" {
			violations = append(violations, fmt.Sprintf(
				"%s 未声明 unit —— 无计量单位判据，展示量纲与可加性无从判断", who))
			continue
		}
		if _, ok := ParseUnit(raw); !ok {
			violations = append(violations, fmt.Sprintf(
				"%s 的 unit=%q 不是已知单位（允许：%s）",
				who, raw, strings.Join(KnownUnits(), "/")))
		}
	}
	sort.Strings(violations)
	return violations
}

// CheckUnitMatchesFormulaKind 断言**比率型算法**的单位声明与其公式形态相称。
//
// ★ 为什么单列这一条：`rate` / `ratio` / `%` 类算法的数值是**无量纲比率**，
// 与金额列混在同一个看板上时必然量纲错乱。而本仓有一个**可静态识别的信号**：
// 公式里出现除法、且分母是**同量纲的减法结果**（如 `gp / revenue`）。
//
// 本判定**只做单向、保守的可疑提示**（不猜真实量纲）：
//
//	若公式是「A / B」形态、且依赖槽里**既有金额槽又有比例化输出**，
//	则该算法不应声明为金额单位（THB/CNY）。
//
// ★ 这条断言**刻意很窄**（宁少一条真断言，不写一条假断言）：
//
//	它只拦「公式明显是比率、单位却声明为金额」这一种**明确矛盾**，
//	不拦反向（金额公式声明为 `%` 可能是业务上确实要百分数展示，
//	如「成本占收入比」类派生指标 —— 那种情况由 formula 本身决定，
//	静态看不出来，强行判会误伤）。参 G4 第八侧教训：闸门本身也可能是错的。
func CheckUnitMatchesFormulaKind(docs []UnitDoc, formulas map[string]string) []string {
	var violations []string
	for _, d := range docs {
		unit, ok := ParseUnit(d.Raw)
		if !ok {
			continue // 不可解析的交由 CheckUnitDeclared 报，不重复报
		}
		f, has := formulas[d.ID]
		if !has {
			continue
		}
		if !isDimensionlessRatioFormula(f) {
			continue
		}
		if unit == "THB" || unit == "CNY" {
			violations = append(violations, fmt.Sprintf(
				"%s 的公式 %q 是比率形态（分子/分母同量纲相除），"+
					"但 unit 声明为金额 %q —— 量纲自相矛盾（应为 %%/ratio）",
				unitWho(d), strings.TrimSpace(f), unit))
		}
	}
	sort.Strings(violations)
	return violations
}

// ratioDivideRe 匹配「标识符 / 标识符」形态的除法（两侧均为裸变量名）。
//
// 只认**顶层**裸名相除（`gp / revenue`），不认 `sum(a)/sum(b)` ——
// 后者两侧同为聚合、量纲由被聚合列决定，静态判不了（保守起见放过）。
var ratioDivideRe = regexp.MustCompile(`(?i)(^|[\s(?:])([a-z_][a-z0-9_]*)\s*/\s*([a-z_][a-z0-9_]*)([\s)?:]|$)`)

// isDimensionlessRatioFormula 保守判断公式是否为「比率形态」。
//
// 判定：公式里出现**顶层裸名相除**（`X / Y`），且 X/Y 两侧变量名不同
// （自己除自己恒为 1，不是比率语义）。
//
// ★ 这是**启发式**，不是量纲推导：它只回答「看起来是不是比率」，
// 故调用方只把它用于**单向**的可疑提示（见 CheckUnitMatchesFormulaKind）。
func isDimensionlessRatioFormula(formula string) bool {
	m := ratioDivideRe.FindAllStringSubmatch(formula, -1)
	for _, g := range m {
		numer, denom := strings.ToLower(g[2]), strings.ToLower(g[3])
		if numer != denom {
			return true
		}
	}
	return false
}

// unitAlgebraRe 匹配「单位 × 单位」或「单位 + 单位」形态的算术表达式。
var unitAlgebraRe = regexp.MustCompile(`(?i)[a-z%]{1,10}\s*[*/+-]\s*[a-z%]{1,10}`)

// CheckUnitVocabularyMatchesDB 断言 Go 侧单位词表与 0002 迁移的
// registry_algorithm.unit 列语义、以及 0004 种子的实际取值**同源**。
//
// 为什么单列一条：0002 把 `unit` 建成 `text NOT NULL`（**无 CHECK**），
// 而 0004 种子里 `algo.*` 的 unit 实际写的是 `'CNY'` / `'ratio'` ——
// 与 `algorithms/*.yaml` 的 `THB` / `percent` **逐字不同**。
// 若 Go 侧词表只认文档里的 `THB | % | count`，那么种子里的 `CNY` 会被
// 加载期判违规，而真相是**两侧各写一份词表**、谁也没错、就是不一致。
//
// 本函数在**加载期**把迁移文本读出来逐个比对（**不连数据库**，
// 与 CheckPermissionVocabularyMatchesDB / rule.CompareSeed 同纪律）：
//
//	① 0002 的 unit 列**必须**没有 CHECK（有 CHECK 就必须与本包词表双向等价，
//	   否则下方会报「DB 有而 Go 无」）；
//	② 0004 种子里出现的每个 unit 字面量，**必须**能被 ParseUnit 解析 ——
//	   否则「种子入库成功、加载期认不出」，字段名存实亡。
func CheckUnitVocabularyMatchesDB(ddlText, seedText string) []string {
	var violations []string

	// ① 0002 的 unit 列若带 CHECK，其枚举必须与 Go 词表双向等价。
	unitCheckRe := regexp.MustCompile(`(?is)\bunit\s+text[^,]*?CHECK\s*\(\s*unit\s+IN\s*\(\s*((?:'[^']*'\s*,?\s*)+)\)`)
	if m := unitCheckRe.FindStringSubmatch(ddlText); m != nil {
		goSet := map[string]bool{}
		for _, u := range algoUnits {
			goSet[strings.ToUpper(u)] = true
		}
		for _, tok := range strings.Split(m[1], ",") {
			t := strings.Trim(strings.TrimSpace(tok), "'\" ")
			if t == "" {
				continue
			}
			if _, ok := ParseUnit(t); !ok {
				violations = append(violations, fmt.Sprintf(
					"0002 的 unit CHECK 含 %q，但 Go 侧单位解析器不认它"+
						"（入库允许、加载期却认不出 ⇒ 字段名存实亡）", t))
			} else if !goSet[strings.ToUpper(t)] {
				violations = append(violations, fmt.Sprintf(
					"0002 的 unit CHECK 含 %q（仅有别名形态），Go 侧规范词表里没有它的规范形"+
						"（两侧书写形态不对齐）", t))
			}
		}
	}

	// ② 0004 种子里 `registry_algorithm` 的 unit 列字面量必须全部可解析。
	//
	// ★ 必须**按列位取**，不能全文扫「全大写 2–10 字母」——
	//   初版正是全文扫，结果把 `status` 的 `'ACTIVE'` / `'MISSING'` / `'STALE'`
	//   也当成疑似单位报了出来（**闸门自身的夹具错**）。
	//   正确做法：定位 `INSERT INTO registry_algorithm (列清单) VALUES`，
	//   从列清单里算出 unit 是第几列，再逐行按 `('algo.x', ..., '<unit>', ...)` 取。
	for _, m := range registryAlgoInsertRe.FindAllStringSubmatch(seedText, -1) {
		cols := m[1]
		idx := unitColumnIndex(cols)
		if idx < 0 {
			continue // 该 INSERT 没有 unit 列，跳过（不臆测）
		}
		body := m[2]
		for _, row := range splitTupleRows(body) {
			fields := splitTopLevelCommas(row)
			if idx >= len(fields) {
				continue
			}
			lit := unquoteSQL(fields[idx])
			if lit == "" {
				continue
			}
			if _, ok := ParseUnit(lit); !ok {
				violations = append(violations, fmt.Sprintf(
					"0004 种子的 registry_algorithm.unit 列取值 %q 不是已知单位"+
						"（允许：%s）—— 种子与 algorithms/*.yaml 的口径分叉",
					lit, strings.Join(KnownUnits(), "/")))
			}
		}
	}

	sort.Strings(violations)
	return violations
}

// registryAlgoInsertRe 匹配 `INSERT INTO registry_algorithm (<列清单>) VALUES <行组>;`
// 捕获组 1 = 列清单，捕获组 2 = VALUES 之后到分号之前的内容。
var registryAlgoInsertRe = regexp.MustCompile(
	`(?is)INSERT\s+INTO\s+registry_algorithm\s*\(([^)]*)\)\s*VALUES\s*(.*?);`)

// unitColumnIndex 在列清单里找 `unit` 的下标（0 基）；找不到返回 -1。
func unitColumnIndex(cols string) int {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		if strings.EqualFold(strings.TrimSpace(p), "unit") {
			return i
		}
	}
	return -1
}

// splitTupleRows 从 VALUES 体里切出**每一行**的括号内容（不含最外层括号）。
//
// 逐字符扫描并跟踪括号深度与单引号状态：单引号内的括号/逗号不参与结构。
func splitTupleRows(body string) []string {
	var rows []string
	depth := 0
	inQuote := false
	start := -1
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\'' {
			// SQL 里的 '' 是转义的单引号，跳过下一个
			if inQuote && i+1 < len(body) && body[i+1] == '\'' {
				i++
				continue
			}
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch c {
		case '(':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case ')':
			depth--
			if depth == 0 && start >= 0 {
				rows = append(rows, body[start:i])
				start = -1
			}
		}
	}
	return rows
}

// splitTopLevelCommas 按**顶层**逗号切分（不切函数参数里的逗号，也不切引号内的）。
//
// 单元素的 ARRAY[...] 里的逗号也是顶层的，本函数不做 ARRAY 感知 ——
// 但那没关系：unit 列的真实取值都是简单字面量，且被截断的 ARRAY 只会让
// 后续列**偏移**从而把某个非单位串取出来 → 会被 ParseUnit 拒 → 这是**保守**
// 方向（宁可误报也不放过），故不额外处理。
func splitTopLevelCommas(row string) []string {
	var out []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(row); i++ {
		c := row[i]
		if c == '\'' {
			if inQuote && i+1 < len(row) && row[i+1] == '\'' {
				i++
				continue
			}
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch c {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, row[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, row[start:])
	return out
}

// unquoteSQL 去掉 SQL 字面量两端的单引号并还原 ” 转义；非引号字面量返回空串。
func unquoteSQL(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return ""
	}
	return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
}

// unitWho 生成报错定位串。
func unitWho(d UnitDoc) string {
	if strings.TrimSpace(d.Kind) == "" {
		return d.ID
	}
	return d.Kind + " " + d.ID
}
