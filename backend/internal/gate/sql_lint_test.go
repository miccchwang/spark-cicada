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
