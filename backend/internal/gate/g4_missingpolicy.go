// G4 第十三侧：算法的**缺失策略**（missing_policy, skip/null/error）必须真实可解析、
// 与 DB 的 CHECK 约束**同源**、并且在**运行时真的被执行** —— 承袭本仓
// 「字段被读进来、写进 DB，却没有任何判定消费」这个病的**第二十一个变种**。
//
// 缺口（2026-10-09 实测）：
//
//	`algorithms/*.yaml` 的 `missing_policy` 在此之前是**被读进来、写进 DB
//	（registry_algorithm.missing_policy）、但没有任何判定消费其声明值**的一个字段。
//	全链路只有三类用法 —— 解析（yaml.Unmarshal）、赋值（admin.Plane 传输结构）、
//	写库/读库（store/admin.go 的列）—— **没有第四类**（比较、判定、阈值）：
//	  * Go 侧**没有任何词表校验**：`missing_policy: erro`、`missing_policy: 中文`
//	    在 `LoadRegistry` 加载期一律放行（DB 侧 0002 的
//	    `CHECK (missing_policy IN ('skip','null','error'))` 只是**入库兜底**，
//	    不等于「Go 侧有判定」——本仓纪律正是「兜底 ≠ 闸门」）；
//	  * 更严重的是**运行时语义只有 skip 被实现**：`precomp.BuildRow` 里
//	    唯一的判定是 `val == nil && a.MissingPolicy == "skip"`，其余一切取值
//	    （`null`、`error`、以及任何拼错的值）**统统落到同一个默认分支** ——
//	    写 NULL、不报错。于是 `missing_policy: error`（docs 声明「缺失即报错」）
//	    与 `missing_policy: null`、与 `missing_policy: 随便写`
//	    **行为完全等价** ⇒ 声明等于装饰。
//
// ★★ 本侧还抓出一个**真实的静默语义反转**（YAML 保留字陷阱）：
//
//	docs/02 §M-ALGO 与 docs/03 §3.1 都明写三值 `skip | null | error`，
//	但作者按文档写 `missing_policy: null` 时，**YAML 会把裸 `null`（及 `~`、空值）
//	解析成 nil**，落到 Go 的 string 字段就是**空字符串**；而 `admin.RegisterAlgorithm`
//	见空即填 `"skip"` ⇒ 作者想要的「缺失写 NULL」被**静默改成「跳过（不写）」**，
//	全程零报错。实测（yaml.v3）：
//
//	    missing_policy: null      → struct=""（作者意图丢失）
//	    missing_policy: ~         → struct=""
//	    missing_policy:           → struct=""
//	    missing_policy: "null"    → struct="null"（唯一能表达 null 策略的写法）
//
//	故本闸门对「键存在、但值是 YAML 空值」单列一条违规并给出**可行动提示**，
//	而不是把它与「漏写」混为一谈（两者的修法不同）。
//
// 与 G4 其余十二侧的关系（各侧互补，缺一即漏）：
//
//	 1 CheckAlgorithmNoDataSource       算法里**不得**混入数据源（分离）
//	 2 CheckSlotsRegistered             算法 → 槽 的引用完整性
//	 3 CheckFormulaVariablesBound       公式自由变量的**值绑定**
//	 4 CheckBucketProducersRegistered   桶 → 算法 的反向引用完整性
//	 5 CheckRefreshDeclared             桶的**刷新节律**必须可解析
//	 6 CheckSlotSourceResolvable        槽的**数据来源**必须可解析
//	 7 CheckKeyStrategyDeclared         槽的**匹配键策略**必须可解析
//	 8 CheckPermissionDeclared          槽/算法的**密级**必须可解析
//	 9 CheckUnitDeclared                算法的**计量单位**必须可解析
//	10 CheckAlgorithmVersionDeclared    算法的**版本号**必须可解析
//	11 CheckBucketIndexesDeclared       桶的**索引**声明必须与 DDL 同源
//	12 CheckBucketGrainDeclared         桶的**粒度**声明必须与物理唯一键同源
//	13 CheckMissingPolicyDeclared       算法的**缺失策略**必须可解析（本文件）
//
// ★ 诚实说明：本侧只断言「缺失策略**声明**是已知的、可解析的、与 DB CHECK 同源的」，
//
//	它**不**替 precomp 断言「三种策略在真实数据上都被正确执行」—— 那部分由
//	`precomp/precomp.go` 的三值分支 + `precomp` 的行为级用例承担
//	（两者配合才闭环：声明层防「写错值」，运行时防「写了不实现」）。
package gate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// missingPolicyInClauseRe 匹配 `missing_policy IN ('skip','null','error')` 形态。
//
// 有意用 `missing_policy` 前缀锚定，避免误匹配同一迁移里 `permission IN (...)`。
var missingPolicyInClauseRe = regexp.MustCompile(`(?is)\bmissing_policy\s+IN\s*\(\s*((?:'[A-Za-z_]+'\s*,?\s*)+)\)`)

// 缺失策略的三个取值（**唯一权威表**）。
//
// ★ 权威依据：docs/02 §M-ALGO 与 docs/03 §3.1 的 `missing_policy: skip | null | error`，
//
//	并与 sql/migrations/0002_slots_and_rules.sql 的
//	`CHECK (missing_policy IN ('skip','null','error'))` **同源**。
//
// ★ 语义（本仓定义，docs/03 §3.1）：
//
//	skip  —— 依赖值缺失 ⇒ **跳过该字段**（不写、记入 skipped_fields，绝不写 0 冒充）；
//	null  —— 依赖值缺失 ⇒ **显式写 NULL**（该列该行为空，但不计入 skipped_fields）；
//	error —— 依赖值缺失 ⇒ **报错**（fail-closed，让构建失败而非静默产出空值）。
const (
	MissingPolicySkip  = "skip"
	MissingPolicyNull  = "null"
	MissingPolicyError = "error"
)

// missingPolicies 是允许的缺失策略（**唯一权威表**）。
var missingPolicies = []string{MissingPolicySkip, MissingPolicyNull, MissingPolicyError}

// MissingPolicies 返回允许的缺失策略（供接线闸门比对，防「文档改了、实现没改」漂移）。
func MissingPolicies() []string {
	out := make([]string, len(missingPolicies))
	copy(out, missingPolicies)
	return out
}

// ParseMissingPolicy 解析缺失策略声明，返回**规范化**取值（小写去空白）。
//
// 第二个返回值为 false 表示「不是已知策略」。本函数**不做**大小写之外的任何
// 猜测或模糊匹配 —— `ignore`、`raise`、`skip_if_missing`、`报错` 一律 false
// （fail-closed）。
//
// 允许大小写差异（`SKIP` → `skip`）：YAML 里手写大写是常见笔误且语义无歧义；
// 但**不允许**别名，那正是「写了等于没写」的入口。
//
// ★ 注意：`null` 必须由调用方保证是**字符串** `"null"`（带引号）——
//
//	裸写 `missing_policy: null` 会被 YAML 解析成空值，根本到不了本函数
//	（见 CheckMissingPolicyDeclared 的 Declared/Raw 区分）。
func ParseMissingPolicy(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", false
	}
	for _, p := range missingPolicies {
		if s == p {
			return p, true
		}
	}
	return "", false
}

// MissingPolicyDoc 是待判定的一条缺失策略声明。
type MissingPolicyDoc struct {
	// AlgoID 算法 ID（仅用于报错定位）。
	AlgoID string
	// Raw 声明原文。
	//
	// ★ 对 YAML 路径：`missing_policy: null`（裸）会得到 Raw=""、Declared=true，
	// 与「漏写」（Declared=false）**必须区分** —— 两者修法不同。
	Raw string
	// Declared 表示 YAML 里是否**真的写了** `missing_policy` 这个键。
	Declared bool
}

// CheckMissingPolicyDeclared 断言每条缺失策略声明都**可解析且是已知策略**
// （G4 第十三侧 / docs/03 §3.1）。
//
// 校验口径（与 CheckPermissionDeclared / CheckUnitDeclared / CheckRefreshDeclared 同构）：
//   - 未声明（Declared=false）⇒ 违规：无缺失行为判据；
//   - 声明了但值是 **YAML 空值**（Declared=true 且 Raw 为空）⇒ 违规，并给出
//     「请写带引号的 \"null\"」的可行动提示（这正是 `missing_policy: null` 被
//     YAML 吞掉的陷阱）；
//   - 值不可解析（`ignore`/`raise`/自由文本）⇒ 违规。
//
// 返回人类可读原因（fail-closed：无法确认策略 ⇒ 不视为合规）。
func CheckMissingPolicyDeclared(docs []MissingPolicyDoc) []string {
	var violations []string
	for _, d := range docs {
		who := missingPolicyWho(d)
		raw := strings.TrimSpace(d.Raw)
		switch {
		case raw == "" && !d.Declared:
			violations = append(violations, fmt.Sprintf(
				"%s 未声明 missing_policy —— 依赖值缺失时无从判断是跳过、置空还是报错", who))
		case raw == "" && d.Declared:
			violations = append(violations, fmt.Sprintf(
				"%s 的 missing_policy 值是 **YAML 空值**（裸 null / ~ / 空）—— "+
					"YAML 会把裸 null 解析成空值并丢失作者意图；若要表达 null 策略请写带引号的 %s",
				who, `"null"`))
		default:
			if _, ok := ParseMissingPolicy(raw); !ok {
				violations = append(violations, fmt.Sprintf(
					"%s 的 missing_policy=%q 不是已知策略（允许：%s）",
					who, raw, strings.Join(MissingPolicies(), "/")))
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// CheckMissingPolicyVocabularyMatchesDB 断言 Go 侧缺失策略词表与 DB 的 CHECK 约束**同源**。
//
// 为什么单列一条：`0002_slots_and_rules.sql` 的
// `CHECK (missing_policy IN ('skip','null','error'))` 是**入库兜底**，而本包的
// missingPolicies 是**加载期闸门**。二者若分叉（如 DB 加了 `zero` 而 Go 侧没加），
// 会出现「Go 加载放行、入库被拒」或反之的静默不一致 —— 两侧各写一份词表
// 迟早漂移，故在**加载期**就把 DB 的那份读出来逐个比对。
//
// sqlText 为 0002 迁移的全文；本函数只做**文本级**核对（不连数据库），
// 与 `slot.ParseSeedItems/CompareSeed` 的静态比对纪律一致。
func CheckMissingPolicyVocabularyMatchesDB(sqlText string) []string {
	var violations []string
	dbSet := map[string]bool{}
	for _, m := range missingPolicyInClauseRe.FindAllStringSubmatch(sqlText, -1) {
		for _, tok := range strings.Split(m[1], ",") {
			t := strings.ToLower(strings.Trim(strings.TrimSpace(tok), "'\" "))
			if t != "" {
				dbSet[t] = true
			}
		}
	}
	goSet := map[string]bool{}
	for _, p := range missingPolicies {
		goSet[p] = true
	}
	for _, p := range missingPolicies {
		if !dbSet[p] {
			violations = append(violations, fmt.Sprintf(
				"Go 侧缺失策略词表含 %s，但 0002 迁移的 missing_policy CHECK 里没有它"+
					"（Go 放行、入库被拒 ⇒ 加载期闸门与入库兜底分叉）", p))
		}
	}
	for p := range dbSet {
		if !goSet[p] {
			violations = append(violations, fmt.Sprintf(
				"0002 迁移的 missing_policy CHECK 含 %s，但 Go 侧词表没有它"+
					"（入库允许、加载期却认不出 ⇒ 字段名存实亡）", p))
		}
	}
	sort.Strings(violations)
	return violations
}

// missingPolicyWho 生成报错定位串。
func missingPolicyWho(d MissingPolicyDoc) string {
	return "算法 " + d.AlgoID
}
