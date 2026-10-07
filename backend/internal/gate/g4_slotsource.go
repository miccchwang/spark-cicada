// G4 第六侧：槽的**数据来源**必须解析得到 —— 承袭本仓「字段被读进来、写进 DB，
// 却没有任何判定消费」这个病的**第十四个变种**。
//
// 缺口（2026-10-07 实测）：
//
//	`slots/*.yaml` 的 `source_kind` / `source_ref` 在此之前是**被读进来、写进 DB
//	（registry_slot.source_kind / source_ref）、但没有任何判定消费**的一对字段。
//	全仓对它们的唯一校验是 `slot.validateSlot` 里的 `s.SourceKind == ""` —— 也就是：
//	  * `source_kind: 随便写` 与 `source_kind: api` 完全等价（都不为空 ⇒ 都放行）；
//	  * `source_ref` **从未被任何代码校验过**，删掉它照样加载成功。
//
//	后果不是「少一条断言」，而是 docs/01 §5.5 的**卖点** ——「换数据源只改槽定义，
//	算法不动」—— 没有判据可依：`source_kind`/`source_ref` 决定采集层去哪取数、
//	以及写进 DB 后**必须满足 0005 迁移的 CHECK 约束**；写错它算法照样算，
//	取到的数却来自完全不同的地方 —— 静默的错数，且无人知道。
//
// 与 G4 其余五侧的关系（各侧互补，缺一即漏）：
//
//	1 CheckAlgorithmNoDataSource       算法里**不得**混入数据源（分离）
//	2 CheckSlotsRegistered             算法 → 槽 的引用完整性
//	3 CheckFormulaVariablesBound       公式自由变量的**值绑定**
//	4 CheckBucketProducersRegistered   桶 → 算法 的反向引用完整性
//	5 CheckRefreshDeclared             桶的**刷新节律**必须可解析
//	6 CheckSlotSourceResolvable        槽的**数据来源**必须可解析（本文件）
//
// ★ 诚实说明：本侧只断言「来源**声明**可解析、种类已知、引用非占位符」，
//
//	它**不**验证外部来源真实存在（那需要真库/连接器）。同 G2/G3/G10 的出站守卫：
//	在正常数据上恒放行 —— 价值是把「来源写错 / 漏写 / 写成占位符」变成一条
//	**可被拒绝的加载**，并让这条判定函数拥有真实生产调用点（`slot.LoadRegistry`）。
package gate

import (
	"fmt"
	"sort"
	"strings"
)

// 来源种类常量。
//
// ★ 单一事实源：与 sql/migrations/0005_collect_staging.sql 的
// `CHECK (source_kind IN ('api','mcp','skill'))` 以及 docs/02 M-COLLECT 表对齐。
// `master_table` / `upload` / `derived` 是**槽侧**的来源种类（采集任务只取 api/mcp/skill
// 三类外部源；主数据表与派生槽不经过采集任务）—— 两处的"交集"由接线闸门钉住。
// 来源种类常量。
//
// ★ 单一事实源：与 sql/migrations/0005_collect_staging.sql 的
// `CHECK (source_kind IN ('api','mcp','skill'))` 以及 docs/02 M-COLLECT 表对齐。
// `master_table` / `upload` / `derived` 是**槽侧**的来源种类（采集任务只取 api/mcp/skill
// 三类外部源；主数据表与派生槽不经过采集任务）—— 两处的包含关系由接线闸门钉住。
const (
	SourceKindAPI         = "api"
	SourceKindMCP         = "mcp"
	SourceKindSkill       = "skill"
	SourceKindMasterTable = "master_table"
	SourceKindUpload      = "upload"
)

// sourceKindSpec 描述一种来源种类的**引用形状**要求。
//
//	RequiresRef = true  ⇒ source_ref 必填（外部来源：不写就不知道去哪取）
//	RequiresRef = false ⇒ source_ref 可空（derived 由上游产出，天然无外部引用）
//	Scope       = 引用落在哪个命名空间（供人工核对；不做前缀强校验，
//	              以免把「换数据源只改槽定义」这条设计自由锁死）
type sourceKindSpec struct {
	Label       string
	RequiresRef bool
	Scope       string
}

// SourceKindDerived = "derived" 表示槽由上游**派生**（docs/03 §2.2 的「派生」来源类型）。
//
// ★ 与其余种类的关键差别：derived 的 `source_ref` 指向的是**逻辑上游**，
//
//	  而逻辑上游有**两种合法形态**（docs/01 §12.1 事实表 + 本仓物化桶）：
//
//		fact_channel_sales → 事实表引用（`channel_sales` 是事实表简名，
//		                      docs/03 §2.2 的 slot.revenue / slot.qty / slot.return_qty）
//		pnl_sku_month      → 物化桶引用（buckets/*.yaml 的桶 ID，
//		                      docs/03 §2.2 的 slot.gross_profit 取「预计算桶的 gp 物化列」）
//
//	  → 故 derived 的 ref 必须**能解析到其中一种**；只校验非空会让
//	    `source_ref: 幽灵上游` 永远静默（同 pnl_month 引 4 个幽灵算法那类缺陷）。
const SourceKindDerived = "derived"

// factTables 是 docs/01 §12.1 事实表的**简名 → 表名**映射（唯一权威表）。
//
// 为什么在这里放一份：槽定义与 docs/03 §2.2 表用的是**简名**（`channel_sales`），
// 而 docs/01 §12.1 的事实表是 `fact_channel_sales`。若不建立简名 → 表名的解析，
// 就只能「只查非空」—— 那正是本仓反复出现的假闸门形态。
//
// ★ 新增事实表时：先改 docs/01 §12.1，再往本表加一行（有闸门钉住两处一致）。
var factTables = map[string]string{
	"sku_financial":    "fact_sku_financial",
	"channel_sales":    "fact_channel_sales",
	"platform_finance": "fact_platform_finance",
	"ad_spend":         "fact_ad_spend",
	"stock_snapshot":   "fact_stock_snapshot",
}

// FactTables 暴露事实表简名集合（供接线闸门与 docs/01 §12.1 比对，防两侧漂移）。
func FactTables() map[string]bool {
	out := make(map[string]bool, len(factTables))
	for k := range factTables {
		out[k] = true
	}
	return out
}

// slotSourceKinds 是允许的槽来源种类（**唯一权威表**）。
//
// ★ 为什么把 `db` / `file` 排除：`internal/admin/plane.go` 的注释曾把
//
//	`db | file` 列进 source_kind 的取值，但 0005 迁移的 CHECK 约束里根本没有这两个值。
//	两处口径分叉 ⇒ 槽定义写 `db` 在加载侧看起来合法、写进 DB 却会**违反 CHECK 约束**
//	（插入失败或静默被拒，取决于调用方是否检查错误）。现以本表为唯一权威，
//	并由接线闸门钉住「本表 == 0005 迁移枚举 == 0005 枚举 ⊆ 本表」的包含关系。
var slotSourceKinds = map[string]sourceKindSpec{
	SourceKindMasterTable: {"主数据表（product_master / cost_master.v2 …）", true, "master"},
	SourceKindAPI:         {"第三方 API（tiktok.finance.ads / fx.daily …）", true, "connector"},
	SourceKindMCP:         {"MCP 连接器（与 api 同等待遇，docs/02）", true, "connector"},
	SourceKindSkill:       {"Skill 适配器（docs/01 §7.4 数据源 Skill）", true, "connector"},
	SourceKindUpload:      {"人工上传/导入", true, "upload"},
	SourceKindDerived:     {"上游派生（事实表或物化桶；无外部连接器）", false, "derived"},
}

// SlotSourceKinds 暴露允许的来源种类（供接线闸门与 0005 迁移比对，防两侧漂移）。
func SlotSourceKinds() map[string]bool {
	out := make(map[string]bool, len(slotSourceKinds))
	for k := range slotSourceKinds {
		out[k] = true
	}
	return out
}

// SourceKindRequiresRef 报告该来源种类是否**必须**带 source_ref。
// 未知种类返回 false（未知本身由 CheckSlotSourceResolvable 报错）。
func SourceKindRequiresRef(kind string) bool {
	spec, ok := slotSourceKinds[strings.TrimSpace(strings.ToLower(kind))]
	return ok && spec.RequiresRef
}

// knownSourceKinds 返回排序后的已知种类（供报错信息稳定可读）。
func knownSourceKinds() []string {
	out := make([]string, 0, len(slotSourceKinds))
	for k := range slotSourceKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SlotSourceDoc 是一条槽的数据来源声明（供 CheckSlotSourceResolvable 判定）。
type SlotSourceDoc struct {
	// SlotID 槽 ID（报错时可定位）。
	SlotID string
	// Kind 声明原文（如 `api` / `master_table` / `derived`）。
	Kind string
	// Ref 声明原文（如 `channel_sales` / `pnl_sku_month` / `fx.daily`）。
	Ref string
	// DerivedResolved 由调用方（slot 包）判定：derived 槽的 ref 是否能解析到
	// 一个**真实上游** —— 事实表简名（docs/01 §12.1）**或**物化桶 ID（buckets/*.yaml）。
	DerivedResolved bool
	// DerivedChecked 由调用方声明「是否真的核对过 derived 引用」。
	// false ⇒ 本闸门**拒判**（不能假装核对过 —— 那正是本仓反复踩的坑）。
	DerivedChecked bool
}

// CheckSlotSourceResolvable 断言每个槽的**数据来源**可解析（G4 第六侧 / docs/03 §2.3）。
//
// 校验口径（有意保守 —— 只认「明确无歧义」的写法）：
//   - `source_kind` 必填，且必须是 slotSourceKinds 里的**已知种类**
//     （`table` / `api2` / `随便写` 一律拒 —— 它们看着合理，采集层却无从下手）；
//   - 需要外部来源的种类（api / mcp / skill / master_table / upload）必须带
//     `source_ref`，且 ref 不得是占位符（`TODO` / `tbd` / `x` / `-` / `?` / `N/A`）；
//   - `derived` 槽：ref 可空（上游产出，天然无外部引用）；但**填了**就必须能解析到
//     事实表简名或物化桶 —— 否则是幽灵引用（同 `pnl_month.yaml` 引 4 个不存在算法
//     那类缺陷，已知实害）。**未核对时拒判**，绝不假装核对过。
//
// 返回人类可读原因（fail-closed：无法确认来源 ⇒ 不视为合规）。
func CheckSlotSourceResolvable(docs []SlotSourceDoc) []string {
	var violations []string
	for _, d := range docs {
		kindRaw := strings.TrimSpace(d.Kind)
		refRaw := strings.TrimSpace(d.Ref)

		if kindRaw == "" {
			violations = append(violations, fmt.Sprintf(
				"槽 %s 未声明 source_kind —— 「数据从哪来」无判据，采集层取到什么都不影响加载", d.SlotID))
			continue
		}
		kind := strings.ToLower(kindRaw)
		spec, known := slotSourceKinds[kind]
		if !known {
			violations = append(violations, fmt.Sprintf(
				"槽 %s 的 source_kind=%q 不是已知来源种类（允许：%s；"+
					"自由文本会让「换数据源只改槽定义」这条承诺失去判据）",
				d.SlotID, kindRaw, strings.Join(knownSourceKinds(), "/")))
			continue
		}

		if SourceKindRequiresRef(kindRaw) {
			if refRaw == "" {
				violations = append(violations, fmt.Sprintf(
					"槽 %s 的 source_kind=%s（%s）必须带 source_ref —— 不写就不知道去哪取数",
					d.SlotID, kind, spec.Label))
				continue
			}
			if IsPlaceholderRef(refRaw) {
				violations = append(violations, fmt.Sprintf(
					"槽 %s 的 source_ref=%q 是占位符（看似填了、实则未填）", d.SlotID, refRaw))
			}
			continue
		}

		// derived：ref 可空；填了就必须能解析到真实上游。
		if refRaw == "" {
			continue
		}
		if !d.DerivedChecked {
			// 不能假装核对过：调用方没给解析结果时拒判（fail-closed）。
			violations = append(violations, fmt.Sprintf(
				"槽 %s（derived）声明了 source_ref=%q，但没有可用的解析结果 —— "+
					"无法确认该上游是否真实存在（fail-closed）", d.SlotID, refRaw))
			continue
		}
		if !d.DerivedResolved {
			violations = append(violations, fmt.Sprintf(
				"槽 %s（derived）的 source_ref=%q 无法解析到任何事实表或物化桶 —— 幽灵引用"+
					"（合法形态：事实表简名 %s，或 buckets/*.yaml 的桶 ID）",
				d.SlotID, refRaw, strings.Join(knownFactTableNames(), "/")))
		}
	}
	sort.Strings(violations)
	return violations
}

// knownFactTableNames 返回排序后的事实表简名（供报错信息稳定可读）。
func knownFactTableNames() []string {
	out := make([]string, 0, len(factTables))
	for k := range factTables {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsKnownFactTable 报告 s 是否为 docs/01 §12.1 定义的事实表**简名**或表名。
//
// 两种写法都接受：槽定义用简名（`channel_sales`），事实表全名是 `fact_channel_sales`。
func IsKnownFactTable(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, ok := factTables[s]; ok {
		return true
	}
	for _, full := range factTables {
		if s == full {
			return true
		}
	}
	return false
}

// placeholderRefs 是「看似填了、实则未填」的占位符集合（归一化后精确匹配）。
//
// ★ 为什么要归一化：`TBD` / `tbd ` / `-` / `N/A` 都能过 TrimSpace 非空检查，
//
//	但都不是真来源。只做「非空」检查会全部放行 —— 同 freshness 那轮的教训
//	（`7d` 与「随便写」在行为上等价的根源就是"只查非空"）。
var placeholderRefs = map[string]bool{
	"todo": true, "tbd": true, "tba": true, "xxx": true, "xx": true,
	"x": true, "-": true, "?": true, "n/a": true, "na": true, "none": true,
	"unknown": true, "later": true, "pending": true, "placeholder": true,
	"待定": true, "无": true, "暂缺": true,
}

// IsPlaceholderRef 报告 ref 是否为占位符。
//
// 归一化：小写 → 去空格/下划线/连字符以外的空白。保留 `-` 本身（它是典型占位符）。
func IsPlaceholderRef(ref string) bool {
	n := strings.ToLower(strings.TrimSpace(ref))
	n = strings.NewReplacer(" ", "", "\t", "", "_", "").Replace(n)
	return placeholderRefs[n]
}

// ───────────────────── G4 第六侧附：算法的 writes_bucket 必须指向真实桶 ─────────────────────

// AlgoBucketDoc 是一条算法的落桶声明（供 CheckAlgoWritesBucketRegistered 判定）。
type AlgoBucketDoc struct {
	// AlgoID 算法 ID（报错时可定位）。
	AlgoID string
	// WritesBucket 声明原文（`writes_bucket` 字段）；可空。
	WritesBucket string
	// Status 算法状态（PENDING 的算法不参与落桶校验 —— 它本就不该入桶）。
	Status string
}

// CheckAlgoWritesBucketRegistered 断言算法的 `writes_bucket` 指向**真实注册的桶**。
//
// 为什么必须有这条（本闸门上线首刻即抓到 3 处真缺陷）：
//
//	`writes_bucket` **是被运行时真正消费的**（sparkd `currentAlgoVersions` 用它
//	按桶汇总算法版本，G6 据此判定「桶的 algo_version 是否落后」）——
//	但它的值此前**从未被校验过**：
//
//	  algorithms/{cogs,gmp,gp}.yaml 都写 `writes_bucket: pnl_sku_month`，
//	  而 `buckets/` 下唯一的桶 ID 是 `pnl_month`。
//
//	后果：`currentAlgoVersions("pnl_month")` 永远返回**空 map**
//	（因为没有任何算法的 writes_bucket 等于该桶 ID）⇒
//	  * 「桶的算法版本落后 ⇒ 标记需重算」（G6）对该桶**恒不成立**；
//	  * `slot.gross_profit` 的 `source_ref: pnl_sku_month` 也是幽灵引用；
//	  * 全程零报错 —— 报表口径静默失真。
//
// 校验口径：
//   - `writes_bucket` 可空（不是所有算法都落桶）；
//   - 非空时必须是 `registeredBuckets` 里的真实桶 ID；
//   - PENDING 算法跳过（它本就不该入桶，由 CheckPendingAlgosNotProducing 管）。
func CheckAlgoWritesBucketRegistered(docs []AlgoBucketDoc, registeredBuckets map[string]bool) []string {
	var violations []string
	for _, d := range docs {
		wb := strings.TrimSpace(d.WritesBucket)
		if wb == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(d.Status), StatusPending) {
			continue // PENDING 不入桶，由 CheckPendingAlgosNotProducing 负责
		}
		if !registeredBuckets[wb] {
			violations = append(violations, fmt.Sprintf(
				"算法 %s 的 writes_bucket=%q 不是已注册桶 —— "+
					"该声明被运行时用于按桶汇总算法版本（G6 漂移检测），"+
					"写错会让「桶版本落后⇒需重算」对该桶恒不成立且零报错", d.AlgoID, wb))
		}
	}
	sort.Strings(violations)
	return violations
}
