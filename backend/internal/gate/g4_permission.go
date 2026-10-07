// G4 第八侧：槽与算法的**密级**（permission, L1..L4）必须真实可解析、并与
// DB 的 CHECK 约束**同源** —— 承袭本仓「字段被读进来、写进 DB，却没有任何判定消费」
// 这个病的**第十六个变种**。
//
// 缺口（2026-10-07 实测）：
//
//	`slots/*.yaml` 与 `algorithms/*.yaml` 的 `permission` 在此之前是
//	**被读进来、写进 DB（registry_slot.permission / registry_algorithm.permission）、
//	但没有任何判定消费**的一个字段。全链路只有三类用法 —— 解析（yaml.Unmarshal）、
//	赋值（admin.Plane 传输结构）、写库/读库（store/admin.go 的列）—— **没有第四类**
//	（比较、判定、阈值）。Go 侧唯一的校验是 `validateSlot` 的 `s.Permission == ""`：
//	  * 16 个槽 + 4 个算法全部声明了 permission，但 `L4` 与 `想写什么写什么`
//	    在 Go 侧行为完全等价（都能加载成功、都能写库、都不影响任何结果）；
//	  * DB 侧 0002 迁移有 `CHECK (permission IN ('L1','L2','L3','L4'))`，但那是
//	    **入库时的兜底**，不是「Go 侧有判定」。本仓纪律恰恰是「兜底 ≠ 闸门」——
//	    不接数据库的纯 Go 加载路径（LoadRegistry）此前完全裸奔。
//
// ★★ 本侧**刻意不做**「算法密级 ≤ 原料槽密级」这类单调性断言 —— 已实测证伪：
//
//	algo.cogs = L4 由 slot.qty(L2) × slot.cost_unit(L4) 得出：**销量是 L2、
//	成本是 L4、成本额合计是 L4** —— 高密级成品由低密级**体量**原料造出，
//	这在业务上完全正当（「卖了多少件」属内部，而「每件成本多少钱」属核心）；
//	反向亦然：algo.gp = L3 由 slot.revenue(L3) − slot.cogs(L4) 得出，恒等式
//	减法**降低**了敏感度（汇总毛利额 ≤ 原始成本明细的可推断信息）。
//	⇒ 密级**不是**沿依赖链单调的，而是逐项业务判断（docs/01 §13.3 只给档位定义，
//	从未承诺单调）。若强行断言单调，就只能靠「抬高 slot.qty 到 L4」或
//	「压低 algo.cogs 到 L2」来满足 —— 前者过度限权，后者**把核心成本降级，
//	是真正的安全回归**。故本闸门只钉「可解析 + 与 DB 词表同源 + 无未决遗漏」，
//	不钉它装不下的东西（宁少一条真断言，不写一条假断言）。
//
// 与 G4 其余七侧的关系（各侧互补，缺一即漏）：
//
//	1 CheckAlgorithmNoDataSource       算法里**不得**混入数据源（分离）
//	2 CheckSlotsRegistered             算法 → 槽 的引用完整性
//	3 CheckFormulaVariablesBound       公式自由变量的**值绑定**
//	4 CheckBucketProducersRegistered   桶 → 算法 的反向引用完整性
//	5 CheckRefreshDeclared             桶的**刷新节律**必须可解析
//	6 CheckSlotSourceResolvable        槽的**数据来源**必须可解析
//	7 CheckKeyStrategyDeclared         槽的**匹配键策略**必须可解析
//	8 CheckPermissionDeclared          槽/算法的**密级**必须可解析（本文件）
//
// ★ 诚实说明：本侧只断言「密级**声明**是已知的、可解析的、与 DB CHECK 同源的」，
//
//	它**不**验证密级在真实鉴权链路上被正确执行（那需要 authz 与真账号数据）。
//	同 G2/G3/G10 的出站守卫：在正常数据上恒放行 —— 价值是把「密级写错 / 漏写 /
//	写成自由文本」变成一条**可被拒绝的加载**，并让这条判定函数拥有真实生产调用点
//	（`slot.Registry.Validate`）。
package gate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// permissionInClauseRe 匹配 `IN ('L1','L2','L3','L4')` 形态的枚举（捕获括号内全部）。
var permissionInClauseRe = regexp.MustCompile(`(?is)\bIN\s*\(\s*((?:'L[0-9]+'\s*,?\s*)+)\)`)

// permissionLevels 是允许的密级档位（**唯一权威表**）。
//
// ★ 权威依据：docs/01 §13.3「L1 公开 / L2 内部 / L3 敏感 / L4 核心」，
// 并与 sql/migrations/0002_slots_and_rules.sql 的
// `CHECK (permission IN ('L1','L2','L3','L4'))` **同源**。
//
// ★ 本表只描述**档位本身**，不复制 authz 的等级次序（levelRank）——
//
//	次序由 authz 包唯一持有，防止「两处各写一份次序、日后分叉」。
//	本包只回答「这是不是一个合法档位」，不回答「L3 是否高于 L2」。
var permissionLevels = []string{"L1", "L2", "L3", "L4"}

// PermissionLevels 返回允许的密级档位（升序，与 docs/01 §13.3 同序）。
//
// 供接线闸门比对（防「文档改了、实现没改」两侧漂移）。
func PermissionLevels() []string {
	out := make([]string, len(permissionLevels))
	copy(out, permissionLevels)
	return out
}

// ParsePermission 解析密级声明，返回**规范化**档位（大写去空白）。
//
// 第二个返回值为 false 表示「不是已知密级」。注意本函数**不做**大小写之外的
// 任何猜测或模糊匹配 —— `高密`、`L5`、`internal` 一律 false（fail-closed）。
//
// 允许大小写差异（`l4` → `L4`）：YAML 里手写小写是常见笔误且语义无歧义；
// 但**不允许**别名（`核心`/`L4级`），那正是「写了等于没写」的入口。
func ParsePermission(raw string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return "", false
	}
	for _, l := range permissionLevels {
		if s == l {
			return l, true
		}
	}
	return "", false
}

// KnownPermissions 返回允许的密级（供报错信息稳定可读）。
func KnownPermissions() []string {
	return PermissionLevels()
}

// PermissionDoc 是待判定的一条密级声明。
type PermissionDoc struct {
	// Kind 声明主体类别（"slot" / "algorithm"），仅用于报错定位。
	Kind string
	// ID 主体 ID（槽 ID 或算法 ID）。
	ID string
	// Raw 声明原文（如 `L4`）。
	Raw string
}

// CheckPermissionDeclared 断言每条密级声明都**非空、可解析、且是已知档位**
// （G4 第八侧 / docs/01 §13.3）。
//
// 校验口径（与 CheckFreshnessDeclared / CheckRefreshDeclared / CheckKeyStrategyDeclared
// 同构，有意保守）：
//   - 必须非空（缺声明 ⇒ 无密级判据，可见性与脱敏无从判断）；
//   - 必须是 L1/L2/L3/L4 之一（`L5`、`高密`、`internal`、`L4级` 一律拒）。
//
// 返回人类可读原因（fail-closed：无法确认密级 ⇒ 不视为合规）。
func CheckPermissionDeclared(docs []PermissionDoc) []string {
	var violations []string
	for _, d := range docs {
		raw := strings.TrimSpace(d.Raw)
		who := permissionWho(d)
		if raw == "" {
			violations = append(violations, fmt.Sprintf(
				"%s 未声明 permission —— 无密级判据，可见性与脱敏无从判断", who))
			continue
		}
		if _, ok := ParsePermission(raw); !ok {
			violations = append(violations, fmt.Sprintf(
				"%s 的 permission=%q 不是已知密级（允许：%s）",
				who, raw, strings.Join(KnownPermissions(), "/")))
		}
	}
	sort.Strings(violations)
	return violations
}

// CheckPermissionVocabularyMatchesDB 断言 Go 侧密级词表与 DB 的 CHECK 约束**同源**。
//
// 为什么单列一条：`0002_slots_and_rules.sql` 的
// `CHECK (permission IN ('L1','L2','L3','L4'))` 是**入库兜底**，而本包的
// permissionLevels 是**加载期闸门**。二者若分叉（如 DB 加了 L5 而 Go 侧没加），
// 会出现「Go 加载放行、入库被拒」或反之的静默不一致 —— 两侧各写一份词表
// 迟早漂移，故在**加载期**就把 DB 的那份读出来逐个比对。
//
// sqlText 为 0002 迁移的全文；本函数只做**文本级**核对（不连数据库），
// 与 `slot.ParseSeedItems/CompareSeed` 的静态比对纪律一致。
func CheckPermissionVocabularyMatchesDB(sqlText string) []string {
	var violations []string
	// 允许的 DB 词表形态：permission 列上的 CHECK (permission IN ('L1','L2','L3','L4'))
	// 逐条枚举 `IN (...)` 里出现的 'Lx' 字面量，要求与 Go 侧词表**集合等价**。
	dbSet := map[string]bool{}
	for _, m := range permissionInClauseRe.FindAllStringSubmatch(sqlText, -1) {
		for _, tok := range strings.Split(m[1], ",") {
			t := strings.ToUpper(strings.Trim(strings.TrimSpace(tok), "'\" "))
			if t != "" {
				dbSet[t] = true
			}
		}
	}
	goSet := map[string]bool{}
	for _, l := range permissionLevels {
		goSet[l] = true
	}
	for _, l := range permissionLevels {
		if !dbSet[l] {
			violations = append(violations, fmt.Sprintf(
				"Go 侧密级词表含 %s，但 0002 迁移的 permission CHECK 里没有它"+
					"（Go 放行、入库被拒 ⇒ 加载期闸门与入库兜底分叉）", l))
		}
	}
	for l := range dbSet {
		if !goSet[l] {
			violations = append(violations, fmt.Sprintf(
				"0002 迁移的 permission CHECK 含 %s，但 Go 侧密级词表没有它"+
					"（入库允许、加载期却认不出 ⇒ 字段名存实亡）", l))
		}
	}
	sort.Strings(violations)
	return violations
}

// permissionWho 生成报错定位串。
func permissionWho(d PermissionDoc) string {
	if strings.TrimSpace(d.Kind) == "" {
		return d.ID
	}
	return d.Kind + " " + d.ID
}
