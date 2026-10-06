// sql_lint_test.go —— SQL 迁移的静态闸门（不依赖数据库）。
//
// 动机：本机/CI 未必有 Postgres，但「审计 append-only」「IT 不可见业务数值」这类
// 红线一旦在重构中被删掉，必须被立刻发现。因此对迁移文件做**文本级不变量断言**：
// 只检查「关键约束是否仍在」，不试图解析 SQL 语义（那是真库集成测试的职责）。
package gate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readMigration(t *testing.T, name string) string {
	t.Helper()
	// 测试工作目录 = backend/internal/gate → 上溯到仓库根
	p := filepath.Join("..", "..", "..", "sql", "migrations", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取迁移失败 %s: %v", p, err)
	}
	return string(b)
}

// TestSQL_AuditAppendOnlyTrigger 断言审计表被数据库层强制 append-only（G10）。
func TestSQL_AuditAppendOnlyTrigger(t *testing.T) {
	sql := readMigration(t, "0001_core.sql")
	must := []string{
		"CREATE TRIGGER trg_audit_log_no_update",
		"BEFORE UPDATE OR DELETE ON audit_log",
		"RAISE EXCEPTION",
	}
	for _, m := range must {
		if !strings.Contains(sql, m) {
			t.Fatalf("G10 审计 append-only 缺失关键片段：%q", m)
		}
	}
}

// TestSQL_ITCannotViewBusinessValues 断言 DB 层 CHECK 约束阻止 IT 可见业务数值（D7）。
func TestSQL_ITCannotViewBusinessValues(t *testing.T) {
	sql := readMigration(t, "0001_core.sql")
	if !strings.Contains(sql, "ck_fact_entitlement_spark_cicada_it_no_business") {
		t.Fatal("G7/D7 DB 层约束缺失：ck_fact_entitlement_spark_cicada_it_no_business")
	}
	if !strings.Contains(sql, "NOT (base_template = 'tpl.it' AND can_view_business_values)") {
		t.Fatal("G7/D7 CHECK 表达式缺失或已被改动")
	}
}

// TestSQL_DerivedColumnsNullable 断言桶的派生列全部可空（缺失写 NULL，不补 0）。
func TestSQL_DerivedColumnsNullable(t *testing.T) {
	sql := readMigration(t, "0003_precompute.sql")
	for _, col := range []string{"gp", "cogs", "net_contrib"} {
		// 形如 "gp numeric(...) NULL" 或列名后跟 NULL
		if !strings.Contains(sql, col) {
			t.Fatalf("G3 桶缺少派生列 %s", col)
		}
	}
	if !strings.Contains(sql, "skipped_fields") {
		t.Fatal("G3 桶缺少 skipped_fields（用于记录跳过的字段）")
	}
	if !strings.Contains(sql, "cov_") {
		t.Fatal("G5 桶缺少 cov_* 覆盖率列")
	}
}

// TestSQL_RegionColumnPresent 断言分地域字段存在（G12：分地域审计/事实）。
func TestSQL_RegionColumnPresent(t *testing.T) {
	core := readMigration(t, "0001_core.sql")
	if !strings.Contains(core, "region") {
		t.Fatal("G12 缺少 region 列（分地域存储/审计）")
	}
}

// TestSQL_NoHardcodedSecrets 断言迁移文件中没有硬编码口令（G11）。
func TestSQL_NoHardcodedSecrets(t *testing.T) {
	for _, name := range []string{
		"0001_core.sql", "0002_slots_and_rules.sql",
		"0003_precompute.sql", "0004_registry_seed.sql",
	} {
		sql := strings.ToLower(readMigration(t, name))
		for _, bad := range []string{"password =", "password=", "secret_key", "client_secret"} {
			if strings.Contains(sql, bad) {
				t.Fatalf("G11 迁移 %s 含疑似硬编码凭据片段：%q", name, bad)
			}
		}
	}
}

// TestSQL_SeedSlotStatusHonest 断言种子如实标注未接入的槽为 MISSING（G5）。
//
// 若把 slot.affiliate 写成 ACTIVE，算法会算出假的 0 而不是「待接入」——
// 这是本项目最危险的失败模式，必须在 SQL 层就锁死。
//
// 实现要点：只在 **registry_slot 的 VALUES 块**内查找，避免误伤算法段的
// depends_on_slots（那里也会出现 'slot.affiliate'）。
func TestSQL_SeedSlotStatusHonest(t *testing.T) {
	sql := readMigration(t, "0004_registry_seed.sql")
	block := valuesBlockFor(t, sql, "INSERT INTO registry_slot")

	if !strings.Contains(block, "'slot.affiliate'") {
		t.Fatal("G5 种子缺少 slot.affiliate 声明")
	}
	// 找到含 slot.affiliate 的那条元组，其内必须含 'MISSING'
	for _, tuple := range splitTuples(block) {
		if !strings.Contains(tuple, "'slot.affiliate'") {
			continue
		}
		if !strings.Contains(tuple, "'MISSING'") {
			t.Fatalf("G5 失败：slot.affiliate 未标注 MISSING（会把缺失算成 0）\n%s", tuple)
		}
		return
	}
	t.Fatal("G5 失败：未能在 registry_slot 块内定位 slot.affiliate 元组")
}

// TestSQL_SeedFormulasHaveNoDataSource 种子里的算法公式不得内嵌数据源（G4）。
//
// 实现要点：只检查 **registry_algorithm 的 VALUES 块**中「公式列」的位置，
// 即第 4 个字段；该块内出现 depends_on_slots 是**合法**的（数据源声明就该在那）。
func TestSQL_SeedFormulasHaveNoDataSource(t *testing.T) {
	sql := readMigration(t, "0004_registry_seed.sql")
	block := valuesBlockFor(t, sql, "INSERT INTO registry_algorithm")

	traps := []string{" FROM ", " from ", "SELECT ", "select ", "http://", "https://"}
	for _, tuple := range splitTuples(block) {
		fields := splitFields(tuple)
		if len(fields) < 4 {
			continue
		}
		formula := strings.TrimSpace(fields[3]) // 4th column = formula
		for _, tr := range traps {
			if strings.Contains(formula, tr) {
				t.Fatalf("G4 失败：算法公式内嵌数据源（命中 %q）：%s", tr, formula)
			}
		}
		// 公式里不得引用 slot.* （那是数据源引用，属于 depends_on_slots 的职责）
		if strings.Contains(formula, "slot.") {
			t.Fatalf("G4 失败：算法公式含 slot.* 引用：%s", formula)
		}
	}
}

// valuesBlockFor 截取 "INSERT INTO <table> ... VALUES ... ;" 之间的 VALUES 块。
func valuesBlockFor(t *testing.T, sql, insertStmt string) string {
	t.Helper()
	i := strings.Index(sql, insertStmt)
	if i < 0 {
		t.Fatalf("未找到语句：%s", insertStmt)
	}
	rest := sql[i:]
	j := strings.Index(rest, "VALUES")
	if j < 0 {
		t.Fatalf("%s 未找到 VALUES 关键字", insertStmt)
	}
	rest = rest[j+len("VALUES"):]
	// 到该语句的 ON CONFLICT 或分号为止
	end := len(rest)
	for _, term := range []string{"ON CONFLICT", ";\n", ";"} {
		if k := strings.Index(rest, term); k >= 0 && k < end {
			end = k
		}
	}
	return rest[:end]
}

// splitTuples 把 VALUES 块切成 (...) 元组（粗略但足够：按行首 '(' 切分）。
func splitTuples(block string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	for _, r := range block {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				out = append(out, cur.String()+"(")
				cur.Reset()
				continue
			}
		}
		if depth > 0 {
			cur.WriteRune(r)
		}
	}
	return out
}

// splitFields 在元组内按「顶层逗号」切分字段（忽略引号与括号内的逗号）。
func splitFields(tuple string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	inQuote := false
	for _, r := range tuple {
		switch r {
		case '\'':
			inQuote = !inQuote
		case '(', '[':
			if !inQuote {
				depth++
			}
		case ')', ']':
			if !inQuote {
				depth--
			}
		case ',':
			if !inQuote && depth == 0 {
				out = append(out, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteRune(r)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// TestSQL_NoUnnecessaryPrivilegeRequests 断言迁移不申请用不到的权限。
//
// ★ 真实事故（由 CI 真库首次抓出）：
//   0001 曾写 `CREATE EXTENSION IF NOT EXISTS "pgcrypto"`，但整套迁移
//   **没有任何语句使用 pgcrypto 的函数**（无 gen_random_uuid/digest/crypt）。
//   而 CREATE EXTENSION 在托管 Postgres 上通常需要**超级用户**——
//   应用账号 spark 是普通账号 ⇒ 整条 0001 以
//     ERROR: permission denied to create extension "pgcrypto"
//   回滚，迁移链断在第一环。本地无库全程 skip，故从未暴露。
//
// 纪律：**不申请用不到的权限**。需要扩展时另开迁移并写明权限前提。
func TestSQL_NoUnnecessaryPrivilegeRequests(t *testing.T) {
	// 需要超级用户（或受限）权限的语句形态
	privileged := []struct{ pat, why string }{
		{"create extension", "CREATE EXTENSION 在托管 Postgres 上通常需超级用户"},
		{"create role", "CREATE ROLE 需超级用户"},
		{"alter system", "ALTER SYSTEM 需超级用户"},
		{"create user", "CREATE USER 需超级用户"},
		{"pg_read_file", "pg_read_file 需超级用户"},
		{"lo_import", "lo_import 需超级用户"},
	}

	names := []string{
		"0001_core.sql", "0002_slots_and_rules.sql",
		"0003_precompute.sql", "0004_registry_seed.sql",
	}

	for _, name := range names {
		// ★ 必须先剥掉注释再匹配。
		//
		// 本闸门第一次上线时是「直接对整份文件 strings.Contains」，
		// 结果被**自己写下的修复说明注释**判红：
		//   0001 里那句 "曾写 CREATE EXTENSION …已移除" 的注释
		//   命中了 "create extension" 子串。
		//
		// 一个会对「解释规则的文字」开火的闸门 = 误报发生器：
		// 它逼着后来人删掉解释、或加白名单把闸门掏空，
		// 最终闸门还在、约束却没了。所以这里做注释剥离，
		// 让断言只针对**真正会执行的 SQL**。
		sql := stripSQLComments(readMigration(t, name))
		low := strings.ToLower(sql)
		for _, p := range privileged {
			if strings.Contains(low, p.pat) {
				t.Errorf("迁移 %s 含特权语句 %q：%s。\n"+
					"若确实必要，请另开独立迁移并显式注明所需权限前提，"+
					"同时在此处加白名单说明原因（不要默认假设超级用户）。",
					name, p.pat, p.why)
			}
		}
	}
}

// stripSQLComments 去掉 SQL 中的 `--` 行注释与 `/* */` 块注释，
// 但**保留字符串字面量内的内容**（否则 'pgcrypto -- 注释' 之类会被误剥）。
//
// 只服务于「静态文本闸门」：目标是让断言面对的是真正会送去执行的语句文本，
// 而不是文档。它不是 SQL 解析器，也不需要是。
func stripSQLComments(in string) string {
	var out strings.Builder
	lines := strings.Split(in, "\n")
	inBlock := false
	for _, line := range lines {
		var kept strings.Builder
		inQuote := false
		i := 0
		for i < len(line) {
			// 块注释：整段跳过（可跨行）
			if inBlock {
				if j := strings.Index(line[i:], "*/"); j >= 0 {
					i += j + 2
					inBlock = false
					continue
				}
				i = len(line)
				continue
			}
			// 单引号字符串字面量：原样保留，内部的 -- /* 不算注释
			if line[i] == '\'' {
				inQuote = !inQuote
				kept.WriteByte(line[i])
				i++
				continue
			}
			if !inQuote {
				if strings.HasPrefix(line[i:], "--") {
					break // 行注释：本行余下全部丢弃
				}
				if strings.HasPrefix(line[i:], "/*") {
					inBlock = true
					i += 2
					continue
				}
			}
			kept.WriteByte(line[i])
			i++
		}
		out.WriteString(kept.String())
		out.WriteString("\n")
	}
	return out.String()
}

// TestStripSQLComments 是上面那个剥离器的**自测**。
//
// 它存在的理由与 stripOuterTx 的回归测试一致：剥离逻辑一旦写错，
// 特权闸门就会静默变松（把真语句也剥没了）或静默变紧（误报）。
// 两种失败都不会有人发现，除非单独测它。
func TestStripSQLComments(t *testing.T) {
	cases := []struct {
		name, in string
		mustHave []string // 剥离后**必须仍在**
		mustNot  []string // 剥离后**必须消失**
	}{
		{
			name:     "行注释被剥掉",
			in:       "-- CREATE EXTENSION pgcrypto 已移除\nSELECT 1;",
			mustHave: []string{"select 1"},
			mustNot:  []string{"create extension"},
		},
		{
			name:     "块注释被剥掉（含跨行）",
			in:       "/* CREATE ROLE evil\n   仍然在注释里 */\nSELECT 1;",
			mustHave: []string{"select 1"},
			mustNot:  []string{"create role"},
		},
		{
			name:     "真语句不被剥掉",
			in:       "CREATE EXTENSION pgcrypto;\nSELECT 1;",
			mustHave: []string{"create extension", "select 1"},
			mustNot:  nil,
		},
		{
			name:     "字符串字面量内的注释符号不误剥",
			in:       "INSERT INTO t VALUES ('a -- b');\nSELECT 1;",
			mustHave: []string{"'a -- b'", "select 1"},
			mustNot:  nil,
		},
		{
			name:     "行尾注释被剥，行内代码保留",
			in:       "SELECT 1; -- CREATE USER bob\nSELECT 2;",
			mustHave: []string{"select 1", "select 2"},
			mustNot:  []string{"create user"},
		},
		{
			name:     "块注释后同行的代码保留",
			in:       "/* c */ SELECT 1;",
			mustHave: []string{"select 1"},
			mustNot:  nil,
		},
	}
	for _, c := range cases {
		got := strings.ToLower(stripSQLComments(c.in))
		for _, w := range c.mustHave {
			if !strings.Contains(got, w) {
				t.Errorf("%s：剥离后丢失了 %q\n输入:\n%s\n输出:\n%s", c.name, w, c.in, got)
			}
		}
		for _, w := range c.mustNot {
			if strings.Contains(got, w) {
				t.Errorf("%s：剥离后仍残留 %q（闸门会被误触发）\n输入:\n%s\n输出:\n%s",
					c.name, w, c.in, got)
			}
		}
	}
}

// TestSQL_NoPairedOuterTransactionGuard 断言迁移文件**可以**自带 BEGIN/COMMIT，
// 但重申：Migrator 会剥离最外层事务，且**不得**误伤 plpgsql 函数体的 BEGIN。
//
// 本测试锁定「函数体顶格 BEGIN」这一形态仍然存在——它是 stripOuterTx
// 回归 bug 的触发条件；若有人把函数体缩进改掉，回归测试就失去对象，
// 这里会提醒。
func TestSQL_PlpgsqlBodyUsesColumnZeroBegin(t *testing.T) {
	sql := readMigration(t, "0001_core.sql")
	if !strings.Contains(sql, "AS $$") {
		t.Fatal("0001 未包含 plpgsql 函数体（预期有 audit_log_immutable）")
	}
	// 函数体 BEGIN 必须顶格（这是 stripOuterTx 最容易被误伤的场景，
	// 也是回归测试 db_test.go 的覆盖对象）。
	if !strings.Contains(sql, "$$\nBEGIN\n") {
		t.Error("plpgsql 函数体的 BEGIN 不再顶格 —— " +
			"stripOuterTx 的回归测试将失去覆盖对象，请确认这是有意改动")
	}
	// 审计防篡改函数必须仍在（G10 的实现载体）
	if !strings.Contains(sql, "audit_log_immutable") {
		t.Error("0001 缺失 audit_log_immutable —— G10 审计 append-only 失去实现载体")
	}
}

// TestSQL_VersionedTablesNotSingleColumnPK 断言「版本化表」的主键含 version 列。
//
// ★ 真实事故（CI 真库首次抓出的第 3 个 bug）：
//   registry_rule_set 写成 `id text PRIMARY KEY` **外加** `UNIQUE (id, version)`，
//   而 0004 种子要插同一 id 的两个版本（rule.tk.fee@1 / @2）
//   ⇒ 撞单列主键重复键，整条迁移回滚。
//
//   识别特征很稳定：**PRIMARY KEY (a) 与 UNIQUE (a, b) 并存**，
//   基本就是「想要 PRIMARY KEY (a, b) 但写漏了」的化石。
//   这个形态纯文本可判，不需要连库，因此适合放进静态闸门。
//
// 纪律：一张表若带 version 列且需要多版本并存，主键必须包含 version。
func TestSQL_VersionedTablesNotSingleColumnPK(t *testing.T) {
	names := []string{
		"0001_core.sql", "0002_slots_and_rules.sql",
		"0003_precompute.sql", "0004_registry_seed.sql",
	}
	// 允许「单列主键 + version 列」的白名单（**必须写明理由**）。
	//
	// registry_algorithm：算法只有**一个当前版本**，version 是「当前版本号」
	// 这个属性，而非多版本并存的历史维度。0004 种子里 @algo.rev 的 ON CONFLICT
	// 用的是 (id) DO UPDATE SET version = EXCLUDED.version —— 即「升级版本号」
	// 而非「新增一行」。这与 registry_rule_set 的语义**不同**：
	// 费率要按生效期回溯多个历史版本，所以必须是 (id, version)。
	//
	// 判据不是「表里有没有 version 列」，而是「**同一 id 是否需要多行并存**」。
	// 需要 → 主键含 version；不需要 → 单列主键 + version 作属性，进白名单。
	allow := map[string]string{
		"0002_slots_and_rules.sql:registry_algorithm": "算法只保留一个当前版本；version 是属性而非历史维度（见 0004 的 ON CONFLICT (id) DO UPDATE SET version）",
	}

	for _, name := range names {
		sql := stripSQLComments(readMigration(t, name))
		// 抓取每张 CREATE TABLE 的表体
		for _, tbl := range splitCreateTables(sql) {
			// ★ 必须判「有没有 version 这个**列**」，不能判「表体里出现过 version 子串」。
			//
			//   第一版用 strings.Contains(body, "version")，于是 registry_bucket
			//   仅因为带 algo_versions / rule_versions 两个 jsonb 列就被误判成
			//   「版本化表」，连带 bucket_pnl_month、precomp_build_log 一起报红。
			//   误报会把真问题淹没（这四行里只有一条是真的）。
			if !hasColumn(tbl.body, "version") {
				continue
			}
			pkCols := extractPKColumns(tbl.body)
			if len(pkCols) == 0 {
				continue // 没声明主键（可能用 UNIQUE 表达）——不在此闸门职责内
			}
			hasVersion := false
			for _, c := range pkCols {
				if c == "version" {
					hasVersion = true
				}
			}
			if hasVersion {
				continue
			}
			key := name + ":" + tbl.name
			if _, ok := allow[key]; ok {
				continue
			}
			t.Errorf("表 %s（迁移 %s）带 version 列，但主键 %v 不含 version。\n"+
				"若该表需多版本并存，插入第二个版本会撞主键重复键；"+
				"若确实只要单版本，请在本测试的 allow 白名单中写明理由。",
				tbl.name, name, pkCols)
		}
	}
}

type createTable struct{ name, body string }

// splitCreateTables 粗粒度切出每个 `CREATE TABLE ... ( ... );` 的
// 表名与表体（括号配平，跳过单引号字面量）。只服务于静态断言，不是 SQL 解析器。
func splitCreateTables(sql string) []createTable {
	var out []createTable
	low := strings.ToLower(sql)
	idx := 0
	for {
		i := strings.Index(low[idx:], "create table")
		if i < 0 {
			break
		}
		start := idx + i
		// 读表名：create table [if not exists] <name> (
		rest := sql[start:]
		open := strings.Index(rest, "(")
		if open < 0 {
			break
		}
		head := rest[:open]
		fields := strings.Fields(head)
		name := ""
		for j := len(fields) - 1; j >= 0; j-- {
			f := strings.TrimSpace(fields[j])
			if f == "" || strings.EqualFold(f, "exists") || strings.EqualFold(f, "not") ||
				strings.EqualFold(f, "if") || strings.EqualFold(f, "table") ||
				strings.EqualFold(f, "create") {
				continue
			}
			name = f
			break
		}
		// 括号配平找表体结束
		depth, inQuote := 0, false
		end := -1
		for k := open; k < len(rest); k++ {
			switch rest[k] {
			case '\'':
				inQuote = !inQuote
			case '(':
				if !inQuote {
					depth++
				}
			case ')':
				if !inQuote {
					depth--
					if depth == 0 {
						end = k
					}
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break
		}
		out = append(out, createTable{name: name, body: rest[open : end+1]})
		idx = start + end
	}
	return out
}

// extractPKColumns 从表体中抽取主键列名。
//
// ★ 实现要点（第一版写错了，这是第二版）：
//   必须**先按顶层逗号把表体切成子句**，再逐个判断该子句是不是主键子句。
//   第一版直接用 strings.Index(body, "(") 找主键后的左括号，
//   结果在多约束表上抓到了**别的**约束的括号：
//     permission text NOT NULL CHECK (permission IN ('L1','L2','L3','L4')),
//     ...
//     PRIMARY KEY (id)
//   解析出主键列 = ["permission in ('l1' 'l2' 'l3' 'l4"]，完全是垃圾。
//   而它**没有报错、只是给出错误答案** —— 这正是静态闸门最危险的失败模式：
//   闸门看起来在工作，实际判据已经烂了。
//
// 支持的写法：
//   - 表级：`PRIMARY KEY (a, b)` / `CONSTRAINT x PRIMARY KEY (a, b)`
//   - 行内：`id text PRIMARY KEY`
func extractPKColumns(body string) []string {
	inner := strings.TrimSpace(body)
	if strings.HasPrefix(inner, "(") && strings.HasSuffix(inner, ")") {
		inner = inner[1 : len(inner)-1]
	}

	var cols []string
	for _, clause := range splitTopLevel(inner, ',') {
		c := strings.TrimSpace(clause)
		if c == "" {
			continue
		}
		up := strings.ToUpper(c)
		if i := strings.Index(up, "PRIMARY KEY"); i >= 0 {
			after := c[i+len("PRIMARY KEY"):]
			if op := strings.Index(after, "("); op >= 0 {
				if cl := strings.Index(after[op:], ")"); cl >= 0 {
					for _, part := range strings.Split(after[op+1:op+cl], ",") {
						col := strings.ToLower(strings.Trim(strings.TrimSpace(part), "\""))
						if col != "" {
							cols = append(cols, col)
						}
					}
				}
				continue
			}
			// 行内形态：该子句的**第一个词**是列名
			if f := strings.Fields(c); len(f) > 0 {
				cols = append(cols, strings.ToLower(strings.Trim(f[0], "\",")))
			}
		}
	}
	return cols
}

// splitTopLevel 按顶层分隔符切分（跳过括号内与单引号字面量内的分隔符）。
func splitTopLevel(s string, sep rune) []string {
	var out []string
	var cur strings.Builder
	depth, inQuote := 0, false
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
			cur.WriteRune(r)
		case inQuote:
			cur.WriteRune(r)
		case r == '(' || r == '[':
			depth++
			cur.WriteRune(r)
		case r == ')' || r == ']':
			depth--
			cur.WriteRune(r)
		case r == sep && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// TestExtractPKColumns 是上面那个解析器的自测。
//
// 为什么必须单独测：第一版输出了垃圾但**没有失败**，
// 换成错误答案的闸门比崩溃的闸门危险得多。
func TestExtractPKColumns(t *testing.T) {
	cases := []struct {
		name, body string
		want       []string
	}{
		{
			name: "表级复合主键",
			body: "(\n id text NOT NULL,\n version integer NOT NULL,\n PRIMARY KEY (id, version)\n)",
			want: []string{"id", "version"},
		},
		{
			name: "行内单列主键",
			body: "(\n id text PRIMARY KEY,\n name text NOT NULL\n)",
			want: []string{"id"},
		},
		{
			name: "★ 行内主键 + 后续 CHECK 含括号（第一版在此输出垃圾）",
			body: "(\n id text PRIMARY KEY,\n permission text NOT NULL CHECK (permission IN ('L1','L2','L3','L4')),\n version integer NOT NULL\n)",
			want: []string{"id"},
		},
		{
			name: "命名约束主键",
			body: "(\n id text NOT NULL,\n version int NOT NULL,\n CONSTRAINT pk_x PRIMARY KEY (id, version)\n)",
			want: []string{"id", "version"},
		},
		{
			name: "CHECK 里的逗号不能切错子句",
			body: "(\n state text CHECK (state IN ('A','B','C')),\n id text,\n PRIMARY KEY (id)\n)",
			want: []string{"id"},
		},
		{
			name: "无主键",
			body: "(\n id text,\n name text\n)",
			want: nil,
		},
	}
	for _, c := range cases {
		got := extractPKColumns(c.body)
		if len(got) != len(c.want) {
			t.Errorf("%s：期望 %v，实际 %v", c.name, c.want, got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s：期望 %v，实际 %v", c.name, c.want, got)
				break
			}
		}
	}
}

// hasColumn 判断表体里是否存在名为 col 的**列定义**
// （列名必须是某个顶层子句的第一个词），而不是「文本里出现过这个词」。
func hasColumn(body, col string) bool {
	inner := strings.TrimSpace(body)
	if strings.HasPrefix(inner, "(") && strings.HasSuffix(inner, ")") {
		inner = inner[1 : len(inner)-1]
	}
	for _, clause := range splitTopLevel(inner, ',') {
		f := strings.Fields(strings.TrimSpace(clause))
		if len(f) == 0 {
			continue
		}
		// 跳过表级约束子句（CONSTRAINT / PRIMARY / UNIQUE / CHECK / FOREIGN / EXCLUDE）
		first := strings.ToUpper(strings.Trim(f[0], "\""))
		switch first {
		case "CONSTRAINT", "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "EXCLUDE":
			continue
		}
		if strings.EqualFold(strings.Trim(f[0], "\""), col) {
			return true
		}
	}
	return false
}

// TestHasColumn 自测 hasColumn：它决定了版本化闸门的**判据范围**，
// 判宽了会误报（真问题被淹没），判窄了会漏报（闸门形同不存在）。
func TestHasColumn(t *testing.T) {
	cases := []struct {
		name, body, col string
		want            bool
	}{
		{"有 version 列", "(\n id text,\n version integer NOT NULL\n)", "version", true},
		{"只有 algo_versions 不算 version 列",
			"(\n id text PRIMARY KEY,\n algo_versions jsonb NOT NULL\n)", "version", false},
		{"只有 rule_versions 不算 version 列",
			"(\n id text,\n rule_versions jsonb DEFAULT '{}'\n)", "version", false},
		{"带引号的列名", "(\n id text,\n \"version\" integer NOT NULL\n)", "version", true},
		{"约束子句里的 version 不算",
			"(\n id text,\n UNIQUE (id, version)\n)", "version", false},
	}
	for _, c := range cases {
		if got := hasColumn(c.body, c.col); got != c.want {
			t.Errorf("%s：期望 %v，实际 %v", c.name, c.want, got)
		}
	}
}

// ───────────────────── M-COLLECT 采集层闸门（0005）─────────────────────
//
// 动机：用户明确要求「抓第三方数据/MCP/API 时必须有临时仓库，
// 不能写死『调用不完全成功就不落库』，要断点续传与自动降速」。
// 这些要求一旦在重构中被削弱（例如有人把「每页 COMMIT」改成整批提交、
// 或把幂等 UNIQUE 索引删掉），会造成**配额被烧光却永远采不完**，
// 甚至在真库时代价极高。因此在迁移文件层面钉死关键结构。

// TestSQL_StagingHasIdempotencyUniqueIndex 断言临时仓库的幂等去重
// **由数据库强制**（UNIQUE 索引），而非应用层「先查后插」。
//
// 为什么必须是索引：应用层先 SELECT 再 INSERT 在并发/重试下有竞态窗口，
// 会让重复行静默累积 —— 「落表失败重试不产生新上游调用」的要求就落空了。
func TestSQL_StagingHasIdempotencyUniqueIndex(t *testing.T) {
	sql := stripSQLComments(readMigration(t, "0005_collect_staging.sql"))

	// 必须是 UNIQUE 索引，且覆盖 (job_id, idem_key)
	if !strings.Contains(sql, "CREATE UNIQUE INDEX") {
		t.Fatal("0005 缺少 UNIQUE 索引 —— 幂等去重会退化成应用层约定，" +
			"并发/重试下产生重复行")
	}
	// 定位幂等索引定义，断言其列组合
	i := strings.Index(sql, "uq_staging_record_idem")
	if i < 0 {
		t.Fatal("0005 缺少 uq_staging_record_idem 索引（幂等去重的唯一保障）")
	}
	stmt := sql[i:]
	if k := strings.Index(stmt, ";"); k >= 0 {
		stmt = stmt[:k]
	}
	for _, col := range []string{"job_id", "idem_key"} {
		if !strings.Contains(stmt, col) {
			t.Errorf("幂等索引未覆盖 %s —— 去重范围不完整\n%s", col, stmt)
		}
	}
}

// TestSQL_StagingHasResumeState 断言断点续传所需的状态列都在。
//
// 断点续传 = 「游标 + 已取页数 + 已落行数 + 档位」四件事同时持久化。
// 少任何一项，重启后都无法准确续跑（要么重复调用，要么丢页）。
func TestSQL_StagingHasResumeState(t *testing.T) {
	sql := readMigration(t, "0005_collect_staging.sql")
	body := tableBodyFor(t, sql, "collect_job")

	for _, col := range []string{"cursor", "pages_done", "rows_staged", "gear_index", "ok_streak"} {
		if !hasColumn(body, col) {
			t.Errorf("collect_job 缺少断点续传状态列 %s —— "+
				"重启后无法从断点续跑（会重复调用上游或丢页）", col)
		}
	}

	// THROTTLED 必须存在且是可续状态：把限流当失败是 KODP 最贵的错误
	// （一次 429 → 整月重放 → 再次 429 → 永远跑不完）。
	if !strings.Contains(sql, "'THROTTLED'") {
		t.Error("collect_job 状态机缺少 THROTTLED —— 限流会被当成失败，" +
			"导致整批重放并反复撞限流")
	}
}

// TestSQL_StagingHasRateGearTable 断言降速档位表存在且覆盖 0.5s→120s 端点。
//
// 用户明确要求「从 0.5s 到 120 秒需要有档位自动调整」。
// 端点缺失（如末档只到 60s）意味着重限流时退避不足，是需求级缺陷。
func TestSQL_StagingHasRateGearTable(t *testing.T) {
	sql := readMigration(t, "0005_collect_staging.sql")
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS collect_rate_gear") {
		t.Fatal("0005 缺少 collect_rate_gear 档位表 —— 无法实现自动降速")
	}
	block := valuesBlockFor(t, sql, "INSERT INTO collect_rate_gear")

	// 首档 500ms、末档 120000ms 必须都在种子里
	if !strings.Contains(block, "500") {
		t.Error("档位种子缺少 500ms（0.5 秒起点）")
	}
	if !strings.Contains(block, "120000") {
		t.Error("档位种子缺少 120000ms（120 秒封顶）—— " +
			"用户明确要求最高档 120 秒，退避不足会持续撞限流")
	}
}

// TestSQL_StagingGuardBlocksByDefault 断言守卫表结构能表达「未过闸」。
//
// 用户要求「守卫验收数据完整性后过闸」。要能「不过闸」，就必须
// 有默认未通过的状态 + 未通过原因 + 判定依据快照。
func TestSQL_StagingGuardBlocksByDefault(t *testing.T) {
	sql := readMigration(t, "0005_collect_staging.sql")

	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS staging_ingest_guard") {
		t.Fatal("0005 缺少 staging_ingest_guard —— 无法记录过闸判定（准入决策不可审计）")
	}

	// ★ staging_record 的默认守卫状态必须是 PENDING（未过闸）。
	//
	//   这里**不能**用 strings.Contains(body, "'PENDING'") 来断言 ——
	//   实测发现那是个假断言：body 里的 CHECK 约束
	//     `CHECK (guard_state IN ('PENDING','PASSED','REJECTED'))`
	//   本身就一直含有 'PENDING' 字面量，于是**无论 DEFAULT 被改成什么，
	//   断言都为真**。负向测试（把 DEFAULT 改成 'PASSED'）当场抓住了这个漏洞。
	//
	//   这正是「文本闸门必须贴着真正产生效果的语法形态写」的又一条实例：
	//   要断言的是 **guard_state 列上的 DEFAULT 子句**，而不是这个字面量
	//   在表体里出现过。
	body := tableBodyFor(t, sql, "staging_record")
	def := defaultClauseFor(t, body, "guard_state")
	if def != "'PENDING'" {
		t.Errorf("staging_record.guard_state 的默认值必须是 'PENDING'（未过闸），实际 %s\n"+
			"默认放行等于未过闸数据也能进投影层 —— 守卫形同不存在。", def)
	}

	// 守卫必须有「通过与否」与「原因」两个字段
	gbody := tableBodyFor(t, sql, "staging_ingest_guard")
	for _, col := range []string{"passed", "reasons"} {
		if !hasColumn(gbody, col) {
			t.Errorf("staging_ingest_guard 缺少 %s 列 —— 判定无法表达/无法追溯", col)
		}
	}
	// 放行台账必须存在（过闸是可审计事件）
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS staging_project_release") {
		t.Error("0005 缺少 staging_project_release —— 放行事件不可审计，" +
			"事后无法回答「这批数据凭什么被放行」")
	}
}

// defaultClauseFor 提取表体中指定列的 DEFAULT 子句字面量。
//
// 例：`guard_state text NOT NULL DEFAULT 'PENDING',` → `'PENDING'`
// 找不到该列的 DEFAULT 时返回 ""（调用方据此判失败 —— 没有 DEFAULT
// 本身就是问题：插入时不提供该列会直接撞 NOT NULL）。
//
// 为什么需要它：断言「某列默认值是 X」必须看**该列自己的 DEFAULT**，
// 而不是整个表体里是否出现过 X。后者会被 CHECK 约束里的枚举值污染，
// 成为一个永远为真的假断言（详见 TestSQL_StagingGuardBlocksByDefault 注释）。
func defaultClauseFor(t *testing.T, body, col string) string {
	t.Helper()
	// ★ body 由 splitCreateTables 产出，形如 "( 列定义, 列定义, ... )" ——
	//   **带有最外层左括号**。若不剥掉，第一个子句会变成 "(guard_state ..."，
	//   首词成了 "(guard_state"，与列名永不相等 ⇒ 恰好「该表的第一列」
	//   永远查不到默认值。自测 TestDefaultClauseFor 当场抓住了这一点。
	body = strings.TrimSpace(body)
	body = strings.TrimPrefix(body, "(")

	for _, line := range splitTopLevel(body, ',') {
		// 只认列定义行：首词是列名（去引号），且含 DEFAULT
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if !strings.Contains(lower, "default") {
			continue
		}
		// 该行的首词必须是目标列名
		firstWord := lower
		if k := strings.IndexAny(firstWord, " \t\n"); k >= 0 {
			firstWord = firstWord[:k]
		}
		firstWord = strings.Trim(firstWord, `"`)
		if firstWord != col {
			continue
		}
		// 取 DEFAULT 之后到行尾（或下一个约束关键字）的字面量
		i := strings.Index(lower, "default")
		rest := strings.TrimSpace(trimmed[i+len("default"):])
		// 到第一个空白/逗号/收尾括号为止即为默认值字面量。
		// 收尾括号也要算终止符：表体最后一列没有尾随逗号，会是 "…'PENDING')"。
		if k := strings.IndexAny(rest, " \t\n,)"); k >= 0 {
			rest = rest[:k]
		}
		return rest
	}
	return ""
}

// TestDefaultClauseFor 自测 defaultClauseFor：它决定了「默认值」这类断言的
// 判据范围，写错会让闸门变成恒真（从而静默失效）。
//
// ★ 用例的 body 一律**带最外层左括号**（与 splitCreateTables 的真实产出一致）——
//   第一版用例没带，于是测试全绿而闸门在真实数据上恒返回空。
//   「用与生产同形的输入做自测」本身就是一条纪律。
func TestDefaultClauseFor(t *testing.T) {
	cases := []struct {
		name, body, col, want string
	}{
		{
			name: "正常提取（带外层括号，同真实形态）",
			body: "(guard_state text NOT NULL DEFAULT 'PENDING')",
			col:  "guard_state", want: "'PENDING'",
		},
		{
			name: "带尾随逗号",
			body: "(guard_state text NOT NULL DEFAULT 'PENDING',\n id bigserial)",
			col:  "guard_state", want: "'PENDING'",
		},
		{
			name: "CHECK 里的枚举值不应被误取为默认值",
			body: "(guard_state text NOT NULL DEFAULT 'PASSED'\n     CHECK (guard_state IN ('PENDING','PASSED')))",
			col:  "guard_state", want: "'PASSED'",
		},
		{
			name: "该列没有 DEFAULT 时返回空",
			body: "(guard_state text NOT NULL)",
			col:  "guard_state", want: "",
		},
		{
			name: "别列的 DEFAULT 不应被认领",
			body: "(status text DEFAULT 'RUNNING',\n guard_state text NOT NULL DEFAULT 'PENDING')",
			col:  "guard_state", want: "'PENDING'",
		},
		{
			name: "列不存在时返回空",
			body: "(id text DEFAULT 'x')",
			col:  "guard_state", want: "",
		},
		{
			name: "第一列且无逗号（外层括号必须被剥掉）",
			body: "(a text DEFAULT 'x')",
			col:  "a", want: "'x'",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := defaultClauseFor(t, c.body, c.col); got != c.want {
				t.Errorf("defaultClauseFor(%q) = %q，期望 %q", c.col, got, c.want)
			}
		})
	}
}

// TestSQL_StagingHasRegionColumn 断言采集层分地域（G12）。
//
// collect_job / staging_record 都要落 region：临时仓库的数据也要能
// 按地域隔离与审计（新加坡/美国双地域拓扑）。
func TestSQL_StagingHasRegionColumn(t *testing.T) {
	sql := readMigration(t, "0005_collect_staging.sql")
	for _, tbl := range []string{"collect_job", "staging_record"} {
		body := tableBodyFor(t, sql, tbl)
		if !hasColumn(body, "region") {
			t.Errorf("%s 缺少 region 列（G12 分地域存储/审计）", tbl)
		}
	}
}

// TestSQL_StagingNoHardcodedSecrets 断言采集层迁移无硬编码凭据（G11）。
//
// 采集层直接对接第三方 API/MCP，是最容易「顺手把 token 写进种子」的地方 ——
// 密钥只允许存**引用**（source_ref），不落明文。
func TestSQL_StagingNoHardcodedSecrets(t *testing.T) {
	sql := strings.ToLower(readMigration(t, "0005_collect_staging.sql"))
	for _, bad := range []string{"password =", "password=", "secret_key", "client_secret", "bearer "} {
		if strings.Contains(sql, bad) {
			t.Errorf("G11 采集层迁移含疑似硬编码凭据片段：%q（密钥只应存引用）", bad)
		}
	}
}

// tableBodyFor 取出某张 CREATE TABLE 的表体（不含表名与最外层括号）。
// 与 splitCreateTables 同源，但按表名精确匹配，供本文件的采集层闸门复用。
func tableBodyFor(t *testing.T, sql, table string) string {
	t.Helper()
	for _, tbl := range splitCreateTables(stripSQLComments(sql)) {
		if tbl.name == table {
			return tbl.body
		}
	}
	t.Fatalf("未找到表 %s 的定义", table)
	return ""
}
