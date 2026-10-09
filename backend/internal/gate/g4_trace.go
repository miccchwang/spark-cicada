// G4 第十四侧：算法的**可回溯声明**（trace）必须真实可解析、与 DB 默认值**同源**，
// 并且在**出站路径上真的被执行**（AlgoTrace 必须覆盖每一条派生列且带 dataSlots）——
// 承袭本仓「字段被读进来、写进 DB，却没有任何判定消费」这个病的**第二十二个变种**。
//
// 缺口（2026-10-09 实测）：
//
//	`algorithms/*.yaml` 的 `trace: true` 在此之前是**被读进来、写进 DB
//	（registry_algorithm.trace）、但没有任何判定消费其声明值**的一个字段。
//	全链路只有三类用法 —— 解析（yaml.Unmarshal → slot.Algorithm.Trace）、
//	赋值（admin.Plane 传输结构 / sparkd admin_handlers JSON）、写库读库
//	（store/admin.go 的三处列）—— **没有第四类**（比较、判定、阈值）：
//	  * `precomp.AlgoDef` **根本没有 Trace 字段**，`BuildRow` 从不读它；
//	  * `query.go` 组装 AlgoTrace 时**从不看 trace**，对所有派生列一律产出条目；
//	  * 于是 `trace: true` 与 `trace: false` 与「随便写」**行为完全等价**
//	    （DB 侧 0002 的 `DEFAULT true` 只是**入库兜底**，不等于 Go 侧有闸门）。
//
// 更严重的是**输出侧也没被消费**：docs/01 §5.5 与 docs/03 §5 明写
// AlgoTrace 的形态是 `{field, algo_id, slots, skipped, reason}`，docs/02 验收
// 「所有输出可沿 AlgoTrace 回溯」「每个金额可回溯算法与数据槽」，docs/01
// 「所有数值必须有 AlgoTrace」—— 但 `query.go` 只填了 `Field`/`AlgoID`，
// **`dataSlots` 永远为空、`skipped` 永远 false**。于是「回溯到哪些真实数据」
// 这条验收在实现侧**做不到**，且没有任何东西会变红。
//
// 与 G4 其余十三侧的关系（各侧互补，缺一即漏）：
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
//	13 CheckMissingPolicyDeclared       算法的**缺失策略**必须可解析
//	14 CheckTraceDeclared /             算法的**可回溯声明**必须可解析且物化算法必须为 true（本文件）
//	   CheckAlgoTraceCoversColumns      + 出站：每条派生列必须可回溯且带 dataSlots
//
// ★ 诚实说明：本侧把闸门从**声明层一路钉到出站路径**（承袭 missing_policy 的
// 「声明与运行时分属两层、必须两侧都查」纪律）：
//
//	声明层 —— 物化算法必须显式 `trace: true`（docs/02 验收「所有输出可沿 AlgoTrace 回溯」）；
//	出站层 —— 出站契约的每条派生列都必须有 AlgoTrace 条目且 `dataSlots` 非空。
//	它**不**替 query 断言「真实桶数据上 dataSlots 逐字正确」—— 那由 query 的
//	行为级用例承担（两者配合才闭环）。
package gate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
)

// DefaultTrace 是算法 `trace` 声明的缺省值。
//
// ★ 权威依据：sql/migrations/0002_slots_and_rules.sql 的
// `trace boolean NOT NULL DEFAULT true`（由 CheckTraceDefaultMatchesDB 钉住同源）。
//
// 缺省为 true 的语义：**输出默认可回溯**（docs/02 验收「所有输出可沿 AlgoTrace 回溯」）。
// 故「未声明」被当作 true 而不是 false —— 静默变成「不可回溯」比「多回溯」危险得多。
const DefaultTrace = true

// traceDefaultRe 匹配 `trace boolean NOT NULL DEFAULT true|false` 形态。
var traceDefaultRe = regexp.MustCompile(`(?is)\btrace\s+boolean\s+NOT\s+NULL\s+DEFAULT\s+(true|false)`)

// ParseTrace 解析 trace 声明的**原始值**，返回（布尔值, 是否可判定）。
//
// 只接受真正的布尔：YAML `true`/`false` 会被 yaml.v3 解成 bool；
// 字符串 `"true"`、数字 `1`、`yes`/`no` 一律不可判定（fail-closed）——
// 「写了等于没写」的入口正是「看起来像布尔但不是」。
func ParseTrace(raw any) (bool, bool) {
	if b, ok := raw.(bool); ok {
		return b, true
	}
	return false, false
}

// TraceDoc 是待判定的一条可回溯声明。
type TraceDoc struct {
	// AlgoID 算法 ID（仅用于报错定位）。
	AlgoID string
	// Trace 解析后的布尔值（仅在 Valid 为 true 时可信）。
	Trace bool
	// Declared 表示 YAML 里是否**真的写了** `trace` 这个键。
	Declared bool
	// Valid 表示声明值是否可判定为布尔（Declared 且类型正确）。
	Valid bool
	// Materializes 表示该算法的输出**会被物化进桶**
	// （writes_bucket 非空且非 PENDING）—— 这类算法的输出必须可回溯。
	Materializes bool
}

// CheckTraceDeclared 断言每条 trace 声明都**可解析**，且**物化算法必须为 true**
// （G4 第十四侧 / docs/02 验收「所有输出可沿 AlgoTrace 回溯」、docs/01 §5.5）。
//
// 校验口径（与 CheckMissingPolicyDeclared / CheckPermissionDeclared 同构）：
//   - 声明了但值不是布尔（`trace: "true"` / `trace: 1` / `trace: yes`）⇒ 违规；
//   - 物化算法（Materializes）声明 `trace: false` ⇒ 违规：
//     其输出将被写进桶却**不可回溯**，直接违反 docs/02 的验收项；
//     未声明（Declared=false）按缺省 true 处理（与 0002 DEFAULT true 同源），不报错。
//
// 返回人类可读原因（fail-closed：无法确认 ⇒ 不视为合规）。
func CheckTraceDeclared(docs []TraceDoc) []string {
	var violations []string
	for _, d := range docs {
		who := "算法 " + d.AlgoID
		switch {
		case d.Declared && !d.Valid:
			violations = append(violations, fmt.Sprintf(
				"%s 的 trace 声明不是布尔值 —— 只有 YAML 的 true/false 才是可判定的布尔；"+
					"字符串 \"true\"、数字 1、yes/no 都会让「是否可回溯」无法判定（fail-closed）", who))
		case d.Materializes && !d.Trace:
			violations = append(violations, fmt.Sprintf(
				"%s 声明 trace: false，但其输出会被物化进桶（writes_bucket 非空且非 PENDING）—— "+
					"物化输出必须可回溯（docs/02 验收「所有输出可沿 AlgoTrace 回溯」/ docs/01 §5.5）；"+
					"请改为 trace: true", who))
		}
	}
	sort.Strings(violations)
	return violations
}

// CheckTraceDefaultMatchesDB 断言 Go 侧 trace 缺省值与 DB 的 DEFAULT **同源**。
//
// 为什么单列一条：`0002_slots_and_rules.sql` 的 `trace boolean NOT NULL DEFAULT true`
// 是**入库兜底**，而本包的 DefaultTrace 是**加载期闸门**。二者若分叉
// （如 DB 改成 DEFAULT false 而 Go 侧没改），会出现「Go 加载按 true 放行、
// 入库被默认成 false」的静默不一致 —— 两侧各写一份默认值迟早漂移，
// 故在**加载期**就把 DB 的那份读出来比对（不连数据库，文本级核对，
// 与 CheckMissingPolicyVocabularyMatchesDB 同纪律）。
func CheckTraceDefaultMatchesDB(sqlText string) []string {
	var violations []string
	m := traceDefaultRe.FindStringSubmatch(sqlText)
	if m == nil {
		violations = append(violations, "0002 迁移里找不到 `trace boolean NOT NULL DEFAULT ...` 声明 —— "+
			"无法与 Go 侧 DefaultTrace 对平（默认值可能已分叉）")
		return violations
	}
	dbDefault := strings.EqualFold(m[1], "true")
	if dbDefault != DefaultTrace {
		violations = append(violations, fmt.Sprintf(
			"trace 缺省值分叉：Go 侧 DefaultTrace=%v，而 0002 迁移的 DEFAULT=%v"+
				"（Go 加载放行、入库默认成另一个值 ⇒ 加载期闸门与入库兜底不一致）",
			DefaultTrace, dbDefault))
	}
	return violations
}

// CheckAlgoTraceCoversColumns 断言出站契约里**每条派生列都可回溯**
// （G4 第十四侧 / docs/01「所有数值必须有 AlgoTrace」、docs/02 验收
// 「所有输出可沿 AlgoTrace 回溯」「每个金额可回溯算法与数据槽」）。
//
// 规则：
//   - 每条带 AlgoID 的列（派生列）必须有 Field 与之匹配的 AlgoTrace 条目；
//   - 该条目的 AlgoID 必须与列的 AlgoID 一致（防止「列说是 gp、溯源说是 cogs」）；
//   - 该条目的 DataSlots 必须非空 —— 否则「回溯到**哪些真实数据**」做不到
//     （docs/01 §5.5 的 trace 形态含 slots，docs/02 验收含「数据槽」）。
//
// ★ 这是 gate 判定函数在**出站路径**上的真实调用点（由 slot.ContractGuard 调用），
// 而不是又一条只被自己测试调用的恒真断言。
func CheckAlgoTraceCoversColumns(dc *contracts.DataContract) []string {
	if dc == nil {
		return []string{"DataContract 为 nil（不得出站空契约）"}
	}
	traceByField := map[string]contracts.AlgoTrace{}
	for _, t := range dc.AlgoTrace {
		traceByField[t.Field] = t
	}
	var violations []string
	for _, c := range dc.Columns {
		if strings.TrimSpace(c.AlgoID) == "" {
			continue // 非派生列（键/维度列）不要求 AlgoTrace
		}
		t, ok := traceByField[c.Key]
		if !ok {
			violations = append(violations, fmt.Sprintf(
				"派生列 %q（算法 %s）没有对应的 AlgoTrace 条目 —— 该列的值无法回溯到算法",
				c.Key, c.AlgoID))
			continue
		}
		if t.AlgoID != c.AlgoID {
			violations = append(violations, fmt.Sprintf(
				"派生列 %q 的 AlgoTrace.algoId=%q 与列的 algoId=%q 不一致 —— 溯源指向了另一条算法",
				c.Key, t.AlgoID, c.AlgoID))
		}
		if len(t.DataSlots) == 0 {
			violations = append(violations, fmt.Sprintf(
				"派生列 %q（算法 %s）的 AlgoTrace 未携带 dataSlots —— "+
					"无法回溯到「哪些真实数据」（docs/01 §5.5 / docs/02 验收「可回溯算法与数据槽」）",
				c.Key, c.AlgoID))
		}
	}
	sort.Strings(violations)
	return violations
}
