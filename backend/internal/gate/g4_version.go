// G4 第十侧：算法的**版本号**与 DB 种子/注册表同源 —— 承袭本仓
// 「字段被读进来、写进 DB，却没有任何判定消费」这个病的**第十八个变种**。
//
// 缺口（2026-10-08 实测）：
//
//	`algorithms/*.yaml` 的 `version` 看似「处处都在用」（`registry_algorithm.version`
//	列、`VersionDrift`、`AlgoVersions`、`AffectedBuckets` …），但把它的**值**当作
//	被校验对象来看，全链路只有三类用法 —— 解析（yaml.Unmarshal）、赋值、
//	**逐层搬运**（bucket.AlgoVersions → currentAlgoVersions → gate.VersionDrift）
//	—— **没有第四类**（比较 / 判定 / 阈值）：
//	  * 全仓非测试代码里 **没有一处**把 `Algorithm.Version` 与**算法身份**比较；
//	  * `admin.Plane.ValidateAlgorithm`（唯一的登记入口）对它**零校验** ——
//	    不查 >0、不查与既有声明是否倒退、不查与迁移种子是否一致；
//	  * `RegisterAlgorithm` 只在 `Version <= 0` 时静默把它改成 `1`
//	    （`a.Version` 本身已是 int，`<=0` 与 `==0` 在本路径等价）——
//	    **缺省填充，不是校验**；把 `3` 改成 `0`、`-7`、`999` 都能存进库。
//	  * DB 侧 0002 把 `version` 建成 `integer NOT NULL`，**无 CHECK**（0004 种子里
//	    却写了 `1/2/3` 这种很具体的值）⇒ `version: 想写什么写什么` 在加载期
//	    与入库期行为完全等价。
//
// 后果是**版本漂移检测静默失真**（G6 的全部结论都建在它上面）：
//
//  1. `algo.gp` 真值 3，桶记录 3（`buckets/pnl_month.yaml`），两侧**恰好一致**，
//     纯凭手工维护 —— 一侧改了另一侧没改，`VersionDrift` 会说「没有漂移」，
//     桶照旧沿用**按旧公式算出的陈旧数字**，全程零报错；
//  2. 版本号是「改了公式要重算哪些桶」的**唯一判据**。若改公式时忘了 +1，
//     G6 的 `AffectedBuckets` 返回空 ⇒ **改了公式却不重算**，
//     报表长期显示错误口径（这比崩溃危险得多）；
//  3. 版本可以**倒退**：登记入口没有任何「版本只能前进」的约束，
//     把 3 写回 2 会让已经重算过的桶被判为「版本超前」而**再次重算**（或反之）。
//
// 而 `docs/03 §3.1` 早已把 `version: <int>`（正整数）写进定义格式，
// 却从没有一行代码去核对它。
//
// ★ 本侧**刻意不做**的三件事（避免把「格式合法」包装成「版本语义已验证」）：
//
//	① **不做「YAML vs 0004 种子」的身份比对**。实测两侧已经真实分叉
//	   （`algo.cogs` YAML=3 / 种子=2；`algo.gp` YAML=3 / 种子=3 恰好一致；
//	   `algo.net_contrib` YAML=2 / 种子=1；`algo.gmp` YAML=3 / 种子=1），
//	   但**无法静态判定哪一侧是真相** —— 种子版本被迁移 `ON CONFLICT DO UPDATE`
//	   覆盖，YAML 与种子谁先谁后取决于部署动作顺序。
//	   若强行断言「必须相等」，就只能靠改数据去迁就一条未经验证的规则
//	   （参 G4 第八侧/第九侧的教训：**宁少一条真断言，不写一条假断言**）。
//	   真实分叉**如实记录**在 docs/03 §3.1 与 docs/05，交用户拍板（docs/06）。
//	② **不做「桶 algo_versions == 算法 version」的对平断言**。两处都出现真分叉
//	   （见上），对平会立刻把**正确数据判成违规**。桶侧的对平已由
//	   `VersionDrift` 在**运行时**（真注册表 vs 真桶状态）完成 —— 那才是有依据的比对。
//	③ **不做语义化版本解析**（`3.1.2` / `v3`）。`version: <int>` 是 docs/03 的定义，
//	   放宽写法只会制造第二套解析规则。
//
// 与 G4 其余九侧的关系（各侧互补，缺一即漏）：
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
//	10 CheckAlgorithmVersionDeclared    算法的**版本号**必须可解析且不倒退（本文件）
//
// ★ 诚实说明：本侧只断言「版本**声明**是正整数、与种子口径一致、且登记时不得倒退」，
//
//	它**不**验证「改公式是否真的 +1」（静态看不出来，公式变更与版本变更之间
//	没有可自动推导的对应关系），也**不**验证 YAML 与种子的版本是否相等（见①）。
//	同 G2/G3/G10 的出站守卫：在正常数据上恒放行 —— 价值是把「版本写错 /
//	漏写 / 写成自由文本 / 倒退」变成一条**可被拒绝的加载或登记**，
//	并让判定函数拥有真实生产调用点（`slot.Registry.Validate` +
//	`admin.Plane.ValidateAlgorithm`）。
package gate

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ───────────────────────── ① 声明层：版本必须可解析为正整数 ─────────────────────────

// VersionDoc 是一个算法的**版本声明**（对应 algorithms/*.yaml 的 `version` 键）。
//
// 为什么单列一个类型而不是复用 AlgoDoc：AlgoDoc 是「算法定义」的解析产物，
// 而本侧判定的对象是**版本这一个字段**，且要能被 `slot.AlgorithmVersionDocs()`
// 以最小改动接进生产校验 —— 与 UnitDoc / FreshnessDoc / PermissionDoc 同纪律。
type VersionDoc struct {
	// AlgoID 算法 ID（报错时可定位）。
	AlgoID string
	// Raw 声明原文（YAML 里的 `version` 值；非整数形态会以文本带下来）。
	Raw string
	// Value 解析出的整数值（仅当可解析时有效）。
	Value int
	// Parsed 报告 Raw 是否可解析为**正整数**。
	Parsed bool
}

// maxAlgoVersion 是版本号的**合理性上界**。
//
// 取 1e6（一百万）：真实算法每年改几次，宁可留足余量也不做无依据的紧约束。
// 它的作用是拦住「多写一个 0」这类手滑（`30` → `30000000`），
// 而不是限制业务演进 —— 所以定得很宽，**只拦明显荒谬值**。
const maxAlgoVersion = 1_000_000

var versionIntRe = regexp.MustCompile(`^\d+$`)

// ParseAlgorithmVersion 是算法版本号的**唯一解析权威**。
//
// 口径（有意保守 —— 只认「明确无歧义」的写法）：
//   - 必须非空；
//   - 必须形如 `<正整数>`（纯十进制数字），前后空白忽略；
//   - `v3` / `3.1` / `3-2` / `latest` / `三` 一律**拒绝**：
//     它们看起来合理，但版本是「是否需要重算」的比较对象，
//     解析不出来就退化成「不影响任何事」；
//   - 非正值（`0`）拒绝：0 与「未声明」无法区分，且会让 VersionDrift 失去意义；
//   - 超过 maxAlgoVersion 拒绝（明显手滑）。
//
// 返回 (值, 失败原因)。失败原因非空即视为不合法。
func ParseAlgorithmVersion(raw string) (int, string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, "版本号为空（版本是 G6『改了公式要重算哪些桶』的唯一判据）"
	}
	// 先剥一层合法引号（YAML 里 `version: "3"` 允许 —— 与其它侧同纪律）。
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	if s == "" {
		return 0, fmt.Sprintf("版本号 %q 只有引号没有内容", raw)
	}
	if !versionIntRe.MatchString(s) {
		return 0, fmt.Sprintf(
			"版本号 %q 不是纯整数写法（允许：正整数，如 3；"+
				"不接受 `v3` / `3.1` / `latest` —— 版本是数值比较对象，非整数值无法比较）", raw)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Sprintf("版本号 %q 无法转换为整数：%v", raw, err)
	}
	if n <= 0 {
		return 0, fmt.Sprintf(
			"版本号 %d 非正数（0 与『未声明』无法区分，且会让 VersionDrift 的对平失去意义）", n)
	}
	if n > maxAlgoVersion {
		return 0, fmt.Sprintf(
			"版本号 %d 超过合理上界 %d（疑似多写一位；若确有需要请同步调整 maxAlgoVersion）",
			n, maxAlgoVersion)
	}
	return n, ""
}

// CheckAlgorithmVersionDeclared 断言每个算法的 `version` 都**可解析为正整数**。
//
// 返回人类可读原因（fail-closed：无法确认版本 ⇒ 不视为合规）。
func CheckAlgorithmVersionDeclared(docs []VersionDoc) []string {
	var violations []string
	for _, d := range docs {
		if _, why := ParseAlgorithmVersion(d.Raw); why != "" {
			violations = append(violations, fmt.Sprintf(
				"算法 %s 的 version 不合法：%s", algoWho(d.AlgoID), why))
		}
	}
	sort.Strings(violations)
	return violations
}

// ────────────── ② 登记时不得倒退：版本只能前进（唯一有依据的比较） ──────────────

// CheckAlgorithmVersionNotRegressing 断言**登记动作**不会让版本倒退。
//
// 为什么这一条才是「版本真的被消费」的关键：
//   声明层（①）只证明「它是个正整数」；而版本的全部意义在于**比较**。
//   `admin.Plane.ValidateAlgorithm` 此前对它零校验 ⇒ 把 `3` 写成 `2` 会
//   安然入库，随后 `VersionDrift` 判定「桶版本 3 > 注册表 2」⇒ 桶被判为
//   「版本超前」，重算与否**取决于比较方向**，两种结果都是错的。
//
// prev==0 表示该算法**尚未登记**（首次登记）：此时**没有比较基准**，
// 一律放行 —— 绝不因为「没有历史」就判违规（否则新算法永远登记不进去）。
//
// 返回违规描述（空 = 合法）。
func CheckAlgorithmVersionNotRegressing(algoID string, prev, next int) []string {
	if prev <= 0 {
		return nil // 首次登记：无比较基准，放行
	}
	if next < prev {
		return []string{fmt.Sprintf(
			"算法 %s 的版本从 %d 倒退到 %d —— 版本只能前进（倒退会让已重算过的桶"+
				"被判为『版本超前』，重算结论随之失真）", algoID, prev, next)}
	}
	return nil
}

// ───────────────────── ③ 与 DB 语义同源：口径必须一致 ─────────────────────

// seedAlgoVersionRe 匹配 `INSERT INTO registry_algorithm (<列清单>) VALUES <行组>;`
//
// 与 g4_unit.go 的 registryAlgoInsertRe 同形，但**本文件不复用**它：
// 两者各自承担不同断言，耦合会让「单位闸门被摘掉」连带影响版本闸门，
// 使注入破坏的分辨变得含糊（看不出是哪一条判定在起作用）。
var seedAlgoVersionRe = regexp.MustCompile(
	`(?is)INSERT\s+INTO\s+registry_algorithm\s*\(([^)]*)\)\s*VALUES\s*(.*?);`)

// CheckAlgorithmVersionMatchesDB 断言 Go 侧版本口径与 0002 / 0004 的**类型语义**同源。
//
// 口径（只钉「两侧都能表达同一件事」，不替数据做决定）：
//
//	① 0002 的 `version` 列若写了 CHECK，其约束必须能被本包解析器满足
//	   （例如 `CHECK (version > 0)` 合法；`CHECK (version > 1000)` 则与本侧
//	   上界冲突 ⇒ 报出来，避免「入库允许、加载期却认不出」）；
//	② 0004 种子里 `registry_algorithm.version` 列的**每一个字面量**都必须
//	   能被 ParseAlgorithmVersion 解析 —— 否则「种子入库成功、加载期认不出」，
//	   版本字段名存实亡；
//	③ **刻意不比对 YAML 与种子的具体数值**（见文件头 ★①）：两侧已真实分叉，
//	   静态无法判定谁是真值。越权替数据做决定，只会逼出「改数据迁就错误规则」。
//
// 静态比对，**不连数据库**（与 CheckUnitVocabularyMatchesDB / rule.CompareSeed 同纪律）。
func CheckAlgorithmVersionMatchesDB(ddlText, seedText string) []string {
	var violations []string

	// ① 0002 的 version 列若带 CHECK，其数值下界必须与本侧「正整数」口径相容。
	verCheckRe := regexp.MustCompile(
		`(?is)\bversion\s+integer[^,]*?CHECK\s*\(\s*version\s*([<>=]+)\s*(\d+)\s*\)`)
	if m := verCheckRe.FindStringSubmatch(ddlText); m != nil {
		op, lit := m[1], m[2]
		n, _ := strconv.Atoi(lit)
		// 本侧只允许 > 0。若 DDL 要求更严（下界 >0 之外，如 >1000），
		// 则 Go 侧能解析、DB 侧却会拒 ⇒ 两侧口径不一致，必须报出来。
		switch {
		case op == ">=" && n > 1:
			violations = append(violations, fmt.Sprintf(
				"0002 的 version CHECK 要求 >= %d，而 Go 侧解析器接受任意正整数"+
					"（两侧口径不一致 ⇒ 会出现「加载期放行、入库被拒」）", n))
		case op == ">" && n > 0:
			violations = append(violations, fmt.Sprintf(
				"0002 的 version CHECK 要求 > %d，而 Go 侧解析器接受任意正整数"+
					"（两侧口径不一致）", n))
		}
	}

	// ② 0004 种子里 `registry_algorithm.version` 列的每个字面量必须可解析。
	//
	// ★ 必须**按列位取**，不能全文扫数字 —— 否则会把 depends_on_slots 的数字、
	//   行号、注释里的数字都当成版本号（参 g4_unit.go 初版全文扫的教训）。
	for _, m := range seedAlgoVersionRe.FindAllStringSubmatch(seedText, -1) {
		idx := columnIndex(m[1], "version")
		if idx < 0 {
			continue // 该 INSERT 没有 version 列，跳过（不臆测）
		}
		for _, row := range splitTupleRows(m[2]) {
			fields := splitTopLevelCommas(row)
			if idx >= len(fields) {
				continue
			}
			lit := strings.TrimSpace(fields[idx])
			// version 是 integer 列，种子里的写法是裸数字（非引号字符串）。
			if lit == "" {
				continue
			}
			if _, why := ParseAlgorithmVersion(lit); why != "" {
				violations = append(violations, fmt.Sprintf(
					"0004 种子的 registry_algorithm.version 列取值 %q 不合法：%s", lit, why))
			}
		}
	}

	sort.Strings(violations)
	return violations
}

// columnIndex 在列清单里找 name 的下标（0 基）；找不到返回 -1。
//
// 与 g4_unit.go 的 unitColumnIndex 同形但**按名取**，便于本文件复用。
func columnIndex(cols, name string) int {
	for i, p := range strings.Split(cols, ",") {
		if strings.EqualFold(strings.TrimSpace(p), name) {
			return i
		}
	}
	return -1
}

// ───────────────────────────── 辅助 ─────────────────────────────

// algoWho 生成报错定位串。
func algoWho(id string) string {
	if strings.TrimSpace(id) == "" {
		return "<未声明 id>"
	}
	return id
}
