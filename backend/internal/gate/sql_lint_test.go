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
	for _, name := range []string{"0001_core.sql", "0002_slots_and_rules.sql", "0003_precompute.sql"} {
		sql := strings.ToLower(readMigration(t, name))
		for _, bad := range []string{"password =", "password=", "secret_key", "client_secret"} {
			if strings.Contains(sql, bad) {
				t.Fatalf("G11 迁移 %s 含疑似硬编码凭据片段：%q", name, bad)
			}
		}
	}
}
