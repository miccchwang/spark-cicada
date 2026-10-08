// G4 第十二侧：桶的**粒度声明**（`buckets/*.yaml` 的 `grain`）与物理表的
// **唯一键**同源 —— 承袭本仓「字段被读进来、写进 DB，却没有任何判定消费」
// 这个病的**第二十个变种**。
//
// 缺口（2026-10-08 实测）：
//
//	`grain` 是桶的**定义性属性** —— 「一行 = 一个 grain 组合」。它的全链路
//	（YAML → `slot.Bucket.Grain` → `gate.BucketDoc.Grain` → `registry_bucket.grain` 列）
//	被读被存，且在**读取侧**被真正消费（`store/postgres.go` 按 grain 列把 DB 行
//	映射回 `RowResult.Keys`）。但**声明本身**只有一个校验：`len(b.Grain) == 0`。
//	  * 列名是否合法（能否与物理列对得上）—— 不校验；
//	  * 与物理表上的**唯一键**是否一致 —— **完全没有关联**（`grep -i grain` 在
//	    `internal/gate/` 里唯一命中是 `BucketDoc.Grain` 这一个字段声明）。
//
//	而真正保证「一行一个 grain」的是迁移里的唯一键：
//	  0003：CONSTRAINT uq_bucket_pnl_month_spark_cicada_grain
//	        UNIQUE (month, channel_code, shop_id, brand)
//	  0011：DROP 之，改建 uq_bucket_pnl_month_tenant_grain
//	        ON bucket_pnl_month (tenant_id, month, channel_code, shop_id, brand)
//	        WHERE tenant_id IS NOT NULL
//	两侧互不校验 ⇒ 改 YAML 的 grain（例如加一列 `spu`）而不动迁移，
//	声明与物理现实**静默分叉**：桶声称「一行一个 (…, spu)」，物理表却仍按旧键
//	去重 ⇒ 同一 grain 组合会被**拆成多行 / 互相覆盖**，而查询侧按**声明的** grain
//	去读，拿到的行集合与声明不符 —— 全程零报错。这正是本仓反复出现的
//	「声明与物理现实分叉、只在结果里看得见」的形态。
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
//	11 CheckBucketIndexesDeclared        桶的**索引声明**必须可解析
//	11b CheckBucketIndexesMatchDDL       索引声明与迁移 DDL 必须同源
//	12 CheckBucketGrainDeclared          桶的**粒度声明**必须可解析（本文件）
//	12b CheckBucketGrainMatchesDDL       粒度声明与物理**唯一键**必须同源（本文件）
//
// ★ 本侧**刻意不做**的两件事（避免把「格式合法」包装成「粒度真的成立」）：
//
//	① **不断言「grain ⊆ 表的全部列」**。列集合要读 `information_schema` 才权威，
//	   静态解析 CREATE TABLE 会漏掉 `ALTER TABLE ... ADD COLUMN`（本仓迁移里就有），
//	   据此报错只会制造**假红**。宁少一条真断言（参第八 / 九 / 十侧教训）。
//	② **不建模 `DROP CONSTRAINT` / `DROP INDEX`**。本仓 0011 会 DROP 0003 的唯一键
//	   再建租户版；不建模 DROP 意味着**两侧唯一键都被检查** —— 更保守、也更能拦住
//	   「改了 grain 却只改了一处」的破坏。代价是：若将来某条唯一键被合法地整体删除
//	   （且无替代），本侧仍会按旧键要求一致 —— 这是有意的保守方向，报错信息会
//	   明确指向是哪条键，便于人工判断。
//
// ★ 顺序不敏感（与第十一侧**相反**，这是刻意的）：
//
//	索引是 B-tree，**列序不同即不同的索引**，故第十一侧按有序序列比对；
//	而唯一键是**集合**语义（`UNIQUE(a,b)` 与 `UNIQUE(b,a)` 等价），
//	grain 的书写顺序也不影响 `store` 按列名取值的映射 —— 故本侧按**集合**比对。
//
// ★ 诚实说明：本侧只断言「grain **声明**可解析、且与迁移里手写的唯一键同源」。
// 它**不**验证真库上的唯一约束真的生效（需要真库），也**不**验证真实数据真的
// 满足该粒度（需要真数据）。与 `CheckBucketIndexesMatchDDL` 同性质：
// 加载期静态比对，不连数据库。
package gate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ───────────────────── ① 声明层：grain 必须可解析 ─────────────────────

// ParseBucketGrain 把「一个桶的 grain 声明」解析为规范形（每列已归一为小写）。
//
// 返回 (规范化的 grain 列序列, 违规描述)。违规描述非空即视为该声明不合法。
//
// 逐条纪律：
//   - 空 grain（未声明 / `[]`）不构成粒度声明 ⇒ 拒绝（预计算粒度是桶的定义性属性）；
//   - 列名必须匹配 `[a-z_][a-z0-9_]*`（归一后）—— 与索引列同一套规则，
//     自由文本 / 含空格 / 含逗号 / 以数字开头一律拒绝；
//   - 不得重复列（`[month, month]` 是手滑，不是「更细的粒度」）。
//
// ★ 只做**大小写归一**，不做任何语义猜测（与 normalizeIndexColumn 同纪律）。
func ParseBucketGrain(bucketID string, raw []string) ([]string, []string) {
	if len(raw) == 0 {
		return nil, []string{fmt.Sprintf(
			"桶 %s 未声明 grain（预计算粒度是桶的定义性属性：一行 = 一个 grain 组合）",
			bucketWho(bucketID))}
	}
	var bad []string
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, c := range raw {
		nc, ok := normalizeIndexColumn(c)
		if !ok {
			bad = append(bad, fmt.Sprintf(
				"桶 %s 的 grain 含非法列名 %q（列名须匹配 `[a-z_][a-z0-9_]*`；"+
					"自由文本 / 含空格 / 含逗号 / 以数字开头一律拒绝）",
				bucketWho(bucketID), c))
			continue
		}
		if seen[nc] {
			bad = append(bad, fmt.Sprintf(
				"桶 %s 的 grain 重复列 %q", bucketWho(bucketID), nc))
			continue
		}
		seen[nc] = true
		out = append(out, nc)
	}
	return out, bad
}

// CheckBucketGrainDeclared 断言每个桶的 `grain` 都**可解析为合法列序列**。
//
// 返回人类可读原因（fail-closed：无法确认粒度声明 ⇒ 不视为合规）。
//
// ★ 这是本侧对 `slot.BucketRegistry.validate` 里那句 `len(b.Grain) == 0` 的**替代**：
// 原先只查「非空」，`grain: [月份, 月份]` 或 `grain: [month day]` 一律照过 ——
// 而它们要么让读取侧按一个**不存在的列名**去取值，要么让粒度声明与物理列对不上。
func CheckBucketGrainDeclared(docs []BucketDoc) []string {
	var violations []string
	for _, d := range docs {
		_, bad := ParseBucketGrain(d.ID, d.Grain)
		violations = append(violations, bad...)
	}
	sort.Strings(violations)
	return violations
}

// ───────────── ② 与物理 DDL 同源：grain 必须等于表的唯一键 ─────────────

// createTableRe 匹配 `CREATE TABLE [IF NOT EXISTS] <name> (`，用于定位表体。
var createTableRe = regexp.MustCompile(
	`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*\(`)

// uniqueConstraintRe 匹配表体里的表级 `UNIQUE (<cols>)`（可带 `CONSTRAINT <name>` 前缀）。
//
// 只认字面量 `UNIQUE`：`PRIMARY KEY (…)` / `CHECK (…)` 不会被命中。
// `[^)]*` 对唯一键列清单是安全的（列清单里不会出现嵌套括号）。
var uniqueConstraintRe = regexp.MustCompile(`(?is)\bUNIQUE\s*\(([^)]*)\)`)

// createUniqueIndexRe 匹配 `CREATE UNIQUE INDEX ... ON <table> (<cols>)`。
//
// ★ 与 `createIndexRe`（第十一侧）的区别：这里**只认 UNIQUE**，且**不过滤 WHERE** ——
// 0011 的租户唯一索引正是**部分索引**（`WHERE tenant_id IS NOT NULL`），
// 它才是桶表当前的**有效**唯一键；把部分索引整条跳过会让本侧「无键可比」而恒放行。
// （第十一侧跳过部分索引是对的：那里判的是「该过滤维度能否走 index scan」，
// 部分索引只覆盖部分行，两者不等价。两处纪律不同源于问题不同，非笔误。）
var createUniqueIndexRe = regexp.MustCompile(
	`(?is)CREATE\s+UNIQUE\s+INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?` +
		`[A-Za-z_][A-Za-z0-9_]*\s+ON\s+([A-Za-z_][A-Za-z0-9_.]*)\s*\(([^)]*)\)`)

// parseCreateTableBodies 返回「表名（小写，去 schema 前缀）→ CREATE TABLE 的括号体」。
//
// 用**括号配平**截取表体（不能用 `[^)]*` —— 表体里有 `CHECK (...)` 等嵌套括号）。
func parseCreateTableBodies(ddlText string) map[string]string {
	sql := stripSQLComments(ddlText)
	out := map[string]string{}
	for _, loc := range createTableRe.FindAllStringSubmatchIndex(sql, -1) {
		name := strings.ToLower(strings.TrimSpace(sql[loc[2]:loc[3]]))
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:] // 去掉 schema 前缀
		}
		open := loc[1] - 1 // 正则末尾即左括号
		if body, ok := balancedParenBody(sql, open); ok {
			out[name] = body
		}
	}
	return out
}

// balancedParenBody 返回 s 中从 open（`(` 的位置）开始、配平括号之间的内容。
func balancedParenBody(s string, open int) (string, bool) {
	if open < 0 || open >= len(s) || s[open] != '(' {
		return "", false
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], true
			}
		}
	}
	return "", false
}

// normalizeKeyColumns 把一个逗号分隔的 DDL 列清单归一为小写列序列。
//
// 复用 `parseDDLIndexColumn`（剥 `ASC`/`DESC` 后缀 + 归一 + 合法性校验）。
// **任一列**无法归一（表达式 / 函数 / 非法字符）⇒ 整条键判为「无法解析」，
// 由调用方跳过 —— 不猜、不误报。
func normalizeKeyColumns(raw string) ([]string, bool) {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		nc, ok := parseDDLIndexColumn(p)
		if !ok {
			return nil, false
		}
		out = append(out, nc)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// ParseUniqueKeys 从迁移 SQL 文本里解析出「表名 → 唯一键列清单序列」。
//
// 来源两处都要收：
//   - `CREATE TABLE` 体里的表级 `UNIQUE (<cols>)`（0003 的桶唯一键即是）；
//   - `CREATE UNIQUE INDEX ... ON <table> (<cols>)`（0011 的租户唯一索引即是）。
//
// 纪律：先剥注释（被注释掉的唯一键不算数，否则比对恒真假绿）；表名 / 列名一律
// 归一小写；表名去 schema 前缀；任一列无法归一 ⇒ 整条键跳过。
//
// 返回的切片顺序 = SQL 文本出现顺序，便于诊断信息稳定。
func ParseUniqueKeys(ddlText string) map[string][][]string {
	sql := stripSQLComments(ddlText)
	out := map[string][][]string{}

	// ① 表级 UNIQUE 约束（需先定位表体才能归属到表）。
	for table, body := range parseCreateTableBodies(ddlText) {
		for _, m := range uniqueConstraintRe.FindAllStringSubmatch(body, -1) {
			if cols, ok := normalizeKeyColumns(m[1]); ok {
				out[table] = append(out[table], cols)
			}
		}
	}
	// ② CREATE UNIQUE INDEX（表名直接写在语句里，无需表体）。
	for _, m := range createUniqueIndexRe.FindAllStringSubmatch(sql, -1) {
		table := strings.ToLower(strings.TrimSpace(m[1]))
		if i := strings.LastIndex(table, "."); i >= 0 {
			table = table[i+1:]
		}
		if cols, ok := normalizeKeyColumns(m[2]); ok {
			out[table] = append(out[table], cols)
		}
	}
	return out
}

// tenantPrefixColumn 是租户迁移在业务唯一键前**追加**的框架前缀列。
//
// 依据：`0011_rls.sql` 明确把桶的全局唯一键改为「租户内唯一」——
// `uq_bucket_pnl_month_tenant_grain ON bucket_pnl_month (tenant_id, month, channel_code, shop_id, brand)`。
// 即：**业务粒度（grain）不变，唯一性收窄到租户内**。故比对时把唯一键首列的
// `tenant_id` 剥掉，再看是否与 grain 相等。
const tenantPrefixColumn = "tenant_id"

// stripTenantPrefix 剥掉唯一键的租户前缀列（仅当它恰好是**首列**时）。
func stripTenantPrefix(cols []string) []string {
	if len(cols) > 0 && cols[0] == tenantPrefixColumn {
		return cols[1:]
	}
	return cols
}

// equalColumnSets 按**集合**语义比较两个列序列（顺序不敏感）。
//
// ★ 与第十一侧刻意相反：索引的列序决定 B-tree 形态（有序比对），
// 而唯一键是集合语义（`UNIQUE(a,b)` ≡ `UNIQUE(b,a)`），grain 的书写顺序
// 也不影响 `store` 按列名取值的映射 —— 故此处按集合比对，避免无意义的假红。
func equalColumnSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// CheckBucketGrainMatchesDDL 断言每个桶**声明的 grain** 与物理表的**唯一键**同源。
//
// 为什么这一条才是「粒度真的被保障」的关键：
//
//	声明层（①）只证明「它是一串合法列名」；而 `grain` 的全部意义在于
//	**物理表上真的按这些列去重**。此前 YAML 与迁移各写一份、互不校验 ⇒
//	改 YAML 的 grain（例如加一列）而不动迁移，声明与物理去重键静默分叉：
//	桶声称「一行一个 (…, spu)」，物理表却仍按旧键去重 ⇒ 同一 grain 组合
//	被拆成多行 / 互相覆盖，而查询侧按**声明的** grain 去读。
//
// 规则：
//   - 表上**必须至少有一条**唯一键（否则「一行一个 grain」失去保障）；
//   - 每一条唯一键，**剥离 tenant_id 前缀**后必须与声明的 grain **集合相等**。
//
// 声明本身不合法（① 会报）时**跳过本桶**，避免重复报错把真正的原因淹没。
//
// 静态比对，**不连数据库**（与 CheckBucketIndexesMatchDDL 同纪律）。
func CheckBucketGrainMatchesDDL(docs []BucketDoc, ddlText string) []string {
	keys := ParseUniqueKeys(ddlText)
	var violations []string
	for _, d := range docs {
		grain, bad := ParseBucketGrain(d.ID, d.Grain)
		if len(bad) > 0 {
			continue // 声明不合法：已由 CheckBucketGrainDeclared 报出，不重复
		}
		table := BucketTableName(d.ID)
		uk := keys[table]
		if len(uk) == 0 {
			violations = append(violations, fmt.Sprintf(
				"桶 %s 对应的物理表 %s 上**没有任何唯一键**（UNIQUE 约束 / 唯一索引）"+
					" ⇒ 「一行一个 grain」失去保障：同一 grain 组合可被写入多行",
				bucketWho(d.ID), table))
			continue
		}
		for _, k := range uk {
			if !equalColumnSets(stripTenantPrefix(k), grain) {
				violations = append(violations, fmt.Sprintf(
					"桶 %s 声明 grain [%s]，但物理表 %s 的唯一键是 [%s]"+
						"（剥离 tenant_id 前缀后仍不等）⇒ 声明与物理去重键分叉："+
						"同一 grain 组合会被拆成多行 / 互相覆盖，而查询侧按声明的 grain 读取",
					bucketWho(d.ID), strings.Join(grain, ", "),
					table, strings.Join(k, ", ")))
			}
		}
	}
	sort.Strings(violations)
	return violations
}
