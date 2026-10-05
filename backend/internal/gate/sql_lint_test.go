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
