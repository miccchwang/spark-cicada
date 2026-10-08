// G4 第十一侧：桶的**索引声明**（`buckets/*.yaml` 的 `indexes`）与物理 DDL 同源
// —— 承袭本仓「字段被读进来、写进 DB，却没有任何判定消费」这个病的
// **第十九个变种**。
//
// 缺口（2026-10-08 实测）：
//
//	`buckets/*.yaml` 的 `indexes` 全链路只有「解析（yaml.Unmarshal）→ 赋值 →
//	结束」三类用法，**没有第四类**（比较 / 判定 / 阈值）：
//	  * 全仓 `grep "\.Indexes"` 的命中数为 **0** —— 连序列化都没有，这个字段
//	    从被解析出来那一刻起就再也没被任何人读过（`slot.Bucket.Indexes` 与
//	    `precomp.BucketDef.Indexes` 两处都是**结构性死字段**）；
//	  * `gate.BucketDoc`（桶 YAML 的解析产物）**根本没有 Indexes 字段**，
//	    所以它连「进闸门」的机会都没有；
//	  * 真正建索引的是迁移 `sql/migrations/0003_precompute.sql` 里**手写**的
//	    四条 `CREATE INDEX ... ON bucket_pnl_month(...)`。
//
//	于是 YAML 的 `indexes` 与物理索引之间**没有任何一致性约束**：改迁移删掉
//	一条索引、或改 YAML 写错列，两侧静默分叉、**零报错**。而索引是查询能否走
//	index scan 的**唯一依据** —— 一条被删掉的索引意味着该过滤维度退化成
//	**全表扫描**（docs/01 §11「基础毫秒级」的物理基础失效），且**只在慢查询里
//	才看得见**，没有任何断言会把它照出来。
//
// 与 G4 其余各侧的关系（各侧互补，缺一即漏）：
//
//	 1 CheckAlgorithmNoDataSource        算法里**不得**混入数据源（分离）
//	 2 CheckSlotsRegistered              算法 → 槽 的引用完整性
//	 3 CheckFormulaVariablesBound        公式自由变量的**值绑定**
//	 4 CheckBucketProducersRegistered    桶 → 算法 的反向引用完整性
//	 5 CheckRefreshDeclared              桶的**刷新节律**必须可解析
//	 6 CheckSlotSourceResolvable         槽的**数据来源**必须可解析
//	 7 CheckKeyStrategyDeclared          槽的**匹配键策略**必须可解析
//	 8 CheckPermissionDeclared           槽/算法的**密级**必须可解析
//	 9 CheckUnitDeclared                 算法的**计量单位**必须可解析
//	10 CheckAlgorithmVersionDeclared     算法的**版本号**必须可解析且不倒退
//	11 CheckBucketIndexesDeclared        桶的**索引声明**必须可解析（本文件）
//	11b CheckBucketIndexesMatchDDL       索引声明与迁移 DDL 必须同源（本文件）
//
// ★ 本侧**刻意不做**的两件事（避免把「格式合法」包装成「索引真的在起作用」）：
//
//	① **不断言「索引列 ⊆ grain」**。物理表上除 grain 列外还有度量列 / 覆盖率列 /
//	   元数据列，在度量列上建索引在物理上完全合法（例如按 `gp` 做范围扫描）；
//	   把「索引必须落在 grain 上」当成不变量属于**无依据的紧约束**，
//	   真按它执行就只能靠改数据去迁就一条未经验证的规则（参第八/九/十侧教训）。
//	   真正有依据的一致性由 ② 承担：声明必须与迁移里**手写的 DDL** 逐条吻合。
//	② **不解析表达式索引 / 部分索引**：表达式索引（`... ON t(lower(name))`）的
//	   「列清单」不是纯列名，硬解析只会把 `lower(name)` 当成列名而**误报**；
//	   部分索引（`... ON t(a) WHERE cond`）只覆盖部分行，与「该过滤维度能否走
//	   index scan」不等价。两者都**整条跳过**（不纳入比对，也不据此报错）。
//
// ★ 诚实说明：本侧只断言「索引**声明**可解析、且与迁移里手写的 CREATE INDEX
//
//	同源（双向）」。它**不**验证索引在真实库上真的被创建（需要真库），
//	也**不**验证查询计划真的走了索引（需要真数据 + EXPLAIN）。与
//	CheckUnitVocabularyMatchesDB / CheckAlgorithmVersionMatchesDB 同性质：
//	加载期静态比对，不连数据库。
package gate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ───────────────────── ① 声明层：索引必须可解析 ─────────────────────

// indexColumnRe 是索引列名的合法形态（归一为小写后匹配）。
//
// 有意收紧：只认 `[a-z_][a-z0-9_]*`。自由文本（`月份` / `month, day` /
// `1st_month`）一律拒绝 —— 列名是**用于比对**的标识符，解析不出来就退化成
// 「不影响任何事」，正是本仓反复出现的静默失效形态。
var indexColumnRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// normalizeIndexColumn 归一一个索引列名（去空白 + 转小写），并判定其合法性。
//
// 只做**大小写归一**：Postgres 对未加引号的标识符一律折叠为小写，故 YAML 写
// `Month` 与 DDL 写 `month` 指的是同一列 —— 归一后比较不会误报。
// 但**不做任何语义猜测**（`month, day` 当成一列 ⇒ 直接判非法，不拆开）。
func normalizeIndexColumn(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if !indexColumnRe.MatchString(s) {
		return "", false
	}
	return s, true
}

// ParseBucketIndexes 把「一个桶的 indexes 声明」解析为规范形（每列已归一为小写）。
//
// 返回 (规范化的索引列表, 违规描述)。违规描述非空即视为该声明不合法。
//
// 逐条纪律：
//   - 空索引（`[]`）不构成索引声明 ⇒ 拒绝；
//   - 列名必须匹配 `[a-z_][a-z0-9_]*`（归一后）；
//   - 同一条索引内不得重复列（`[month, month]` 是手滑，不是「更快的索引」）。
//
// ★ 只做「逐条」校验；「同一桶内是否重复声明同一条索引」由调用方
// （CheckBucketIndexesDeclared）跨条判定 —— 与 KeyStrategy 的「声明层 / 跨槽
// 一致性」分成两条同纪律。
func ParseBucketIndexes(bucketID string, raw [][]string) ([][]string, []string) {
	var bad []string
	out := make([][]string, 0, len(raw))
	for i, idx := range raw {
		if len(idx) == 0 {
			bad = append(bad, fmt.Sprintf(
				"桶 %s 的第 %d 条索引为空（`[]` 不构成索引声明）", bucketWho(bucketID), i+1))
			continue
		}
		norm := make([]string, 0, len(idx))
		seen := map[string]bool{}
		ok := true
		for _, c := range idx {
			nc, valid := normalizeIndexColumn(c)
			if !valid {
				bad = append(bad, fmt.Sprintf(
					"桶 %s 的第 %d 条索引含非法列名 %q（列名须匹配 `[a-z_][a-z0-9_]*`；"+
						"自由文本 / 含空格 / 含逗号 / 以数字开头一律拒绝）",
					bucketWho(bucketID), i+1, c))
				ok = false
				continue
			}
			if seen[nc] {
				bad = append(bad, fmt.Sprintf(
					"桶 %s 的第 %d 条索引重复列 %q", bucketWho(bucketID), i+1, nc))
				ok = false
				continue
			}
			seen[nc] = true
			norm = append(norm, nc)
		}
		if ok {
			out = append(out, norm)
		}
	}
	return out, bad
}

// CheckBucketIndexesDeclared 断言每个桶的 `indexes` 声明都**可解析为合法索引**。
//
// 返回人类可读原因（fail-closed：无法确认索引声明 ⇒ 不视为合规）。
func CheckBucketIndexesDeclared(docs []BucketDoc) []string {
	var violations []string
	for _, d := range docs {
		norm, bad := ParseBucketIndexes(d.ID, d.Indexes)
		violations = append(violations, bad...)
		// 跨条：同桶内不得重复声明同一条索引。
		// 顺序敏感 —— B-tree 的列序不同即**不同的索引**，故按有序序列比对。
		seen := map[string]bool{}
		for _, idx := range norm {
			key := strings.Join(idx, ",")
			if seen[key] {
				violations = append(violations, fmt.Sprintf(
					"桶 %s 重复声明同一条索引 [%s]", bucketWho(d.ID), strings.Join(idx, ", ")))
			}
			seen[key] = true
		}
	}
	sort.Strings(violations)
	return violations
}

// ────────────── ② 与物理 DDL 同源：声明必须对应真建出来的索引 ──────────────

// bucketTablePrefix 是「桶 ID → 物理表名」的命名约定（`pnl_month` → `bucket_pnl_month`）。
//
// 该约定同时出现在迁移 0003 与 `store/postgres.go` 的 `specs` 表里；
// 本侧**依赖**它并在此显式声明 —— 若哪天约定变了，本闸门会立刻报出来
// （而不是静默按新表名去比对另一张表，让比对恒真）。
const bucketTablePrefix = "bucket_"

// BucketTableName 返回桶 ID 对应的物理表名（供测试与运维核对）。
func BucketTableName(bucketID string) string {
	return bucketTablePrefix + strings.TrimSpace(bucketID)
}

// createIndexRe 匹配 `CREATE [UNIQUE] INDEX [IF NOT EXISTS] <name> ON <table>(<cols>)`
// 并捕获可选的 `WHERE`（用于识别**部分索引**）。
//
// 只认**普通列索引**：
//   - 列清单里出现非列名（表达式 / 函数调用，如 `lower(name)`）⇒ 由
//     parseDDLIndexColumn 判非法 ⇒ **整条索引跳过**（不猜、不误报）；
//   - **部分索引**（`... ON t(a) WHERE cond`）也**整条跳过** —— 它只覆盖部分行，
//     与「该过滤维度能否走 index scan」不等价，纳进来会把两者当成同一个索引。
var createIndexRe = regexp.MustCompile(
	`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?` +
		`[A-Za-z_][A-Za-z0-9_]*\s+ON\s+([A-Za-z_][A-Za-z0-9_.]*)\s*\(([^)]*)\)(\s+WHERE\b)?`)

// ddlIndexColumnRe 匹配一个 DDL 索引列：纯列名，可选 `ASC` / `DESC` 排序后缀。
//
// ★ 必须**要求空白**再跟 ASC/DESC：否则列名 `last_desc` 会被误剥成 `last_`
// （那会制造一条幽灵列，把「列名写错」变成一条**假红**）。
var ddlIndexColumnRe = regexp.MustCompile(
	`(?i)^([A-Za-z_][A-Za-z0-9_]*)(?:\s+(?:ASC|DESC))?$`)

// parseDDLIndexColumn 解析 DDL 里的一个索引列（剥掉 ASC/DESC 后缀后归一）。
//
// 返回 (归一列名, 是否合法)。排序方向不影响「索引了哪些列」这一事实，故剥掉。
func parseDDLIndexColumn(raw string) (string, bool) {
	m := ddlIndexColumnRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", false
	}
	return normalizeIndexColumn(m[1])
}

// stripSQLComments 去掉 SQL 里的行注释（`-- …`）与块注释（`/* … */`）。
//
// ★ 为什么必须去：迁移文件里大量使用 `--` 注释（0003 就有十几行）。
// 若不去，一条**被注释掉的** `-- CREATE INDEX ... ON t(a)` 会被当成真索引 ⇒
// 「声明 vs DDL」的比对**恒真**（假绿）—— 正是本仓反复在打的病。
//
// 已知局限（如实标注）：不处理字符串字面量里出现 `--` 的情形
// （本仓迁移的 DDL 里没有；若将来有，需换成带引号状态的扫描器）。
func stripSQLComments(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	for i := 0; i < len(sql); {
		if i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' { // 块注释
			j := strings.Index(sql[i+2:], "*/")
			if j < 0 {
				break // 未闭合：其余全部视为注释
			}
			i += 2 + j + 2
			continue
		}
		if i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-' { // 行注释
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				break
			}
			i += j // 保留换行本身（下一轮写出）
			continue
		}
		b.WriteByte(sql[i])
		i++
	}
	return b.String()
}

// ParseCreateIndexes 从迁移 SQL 文本里解析出「表名 → 索引列清单序列」。
//
// 纪律：
//   - **先去注释**（见 stripSQLComments）：被注释掉的 `CREATE INDEX` 不算数；
//   - 表名与列名一律归一小写（Postgres 未加引号的标识符即小写）；
//   - 表名去掉 schema 前缀（`public.bucket_pnl_month` → `bucket_pnl_month`）；
//   - `DESC` / `ASC` 后缀剥掉（排序方向不改变「索引哪些列」）；
//   - **部分索引**（带 `WHERE`）整条跳过 —— 只覆盖部分行，与全量索引不等价；
//   - **任何一列**无法归一（表达式 / 函数 / 非法字符）⇒ **整条索引跳过**，
//     不把它当成一条「列清单」纳入比对（避免误报）。
//
// 返回的切片顺序 = SQL 文本出现顺序，便于诊断信息稳定。
func ParseCreateIndexes(ddlText string) map[string][][]string {
	out := map[string][][]string{}
	for _, m := range createIndexRe.FindAllStringSubmatch(stripSQLComments(ddlText), -1) {
		if strings.TrimSpace(m[3]) != "" {
			continue // 部分索引（WHERE）：整条跳过
		}
		table := strings.ToLower(strings.TrimSpace(m[1]))
		if i := strings.LastIndex(table, "."); i >= 0 {
			table = table[i+1:] // 去掉 schema 前缀
		}
		cols := strings.Split(m[2], ",")
		norm := make([]string, 0, len(cols))
		ok := true
		for _, c := range cols {
			nc, valid := parseDDLIndexColumn(c)
			if !valid {
				ok = false
				break
			}
			norm = append(norm, nc)
		}
		if ok && len(norm) > 0 {
			out[table] = append(out[table], norm)
		}
	}
	return out
}

// CheckBucketIndexesMatchDDL 断言每个桶**声明的索引**与迁移里**手写的 CREATE INDEX**
// 逐条一致（**双向**：声明的必须在 DDL 里存在；DDL 里的必须被声明）。
//
// 为什么这一条才是「索引真的被消费」的关键：
//
//	声明层（①）只证明「它是一串合法列名」；而 `indexes` 的全部意义在于
//	**对应一张物理表上真的建了这些索引**。此前 YAML 与迁移各写一份、互不校验 ⇒
//	删掉迁移里的一条索引（该维度退化成全表扫描）不会有任何东西变红。
//
// 顺序敏感：B-tree 的列序不同即不同的索引，故按**有序序列**比对。
// 双向：只查单向会让「迁移多建了一条索引但 YAML 没记」也静默通过 ——
// 那同样是「声明与物理现实分叉」。
//
// 声明本身不合法（① 会报）时**跳过本桶**，避免重复报错把真正的原因淹没。
//
// 静态比对，**不连数据库**（与 CheckUnitVocabularyMatchesDB 同纪律）。
func CheckBucketIndexesMatchDDL(docs []BucketDoc, ddlText string) []string {
	ddl := ParseCreateIndexes(ddlText)
	var violations []string
	for _, d := range docs {
		want, bad := ParseBucketIndexes(d.ID, d.Indexes)
		if len(bad) > 0 {
			continue // 声明不合法：已由 CheckBucketIndexesDeclared 报出，不重复
		}
		table := BucketTableName(d.ID)
		have := ddl[table]

		haveSet := map[string]bool{}
		for _, idx := range have {
			haveSet[strings.Join(idx, ",")] = true
		}
		wantSet := map[string]bool{}
		for _, idx := range want {
			wantSet[strings.Join(idx, ",")] = true
		}

		for _, idx := range want {
			if !haveSet[strings.Join(idx, ",")] {
				violations = append(violations, fmt.Sprintf(
					"桶 %s 声明了索引 [%s]，但迁移里表 %s 上**没有对应的 CREATE INDEX**"+
						"（该过滤维度会退化成全表扫描，且只在慢查询里才看得见）",
					bucketWho(d.ID), strings.Join(idx, ", "), table))
			}
		}
		for _, idx := range have {
			if !wantSet[strings.Join(idx, ",")] {
				violations = append(violations, fmt.Sprintf(
					"迁移里表 %s 有索引 [%s]，但桶 %s 的 YAML **未声明**"+
						"（改 DDL 时忘了同步 YAML ⇒ 声明与物理现实分叉）",
					table, strings.Join(idx, ", "), bucketWho(d.ID)))
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// ───────────────────────────── 辅助 ─────────────────────────────

// bucketWho 生成报错定位串。
func bucketWho(id string) string {
	if strings.TrimSpace(id) == "" {
		return "<未声明 id>"
	}
	return id
}
