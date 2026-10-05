package db

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ★ 回归测试：stripOuterTx 绝不能删掉 plpgsql 函数体内的 BEGIN。
//
// 真实事故：早期实现用正则 ReplaceAll 全文删除行首 BEGIN/COMMIT，
// 把 audit_log_immutable() 的 `BEGIN ... END;` 打断 → 函数体残缺
// → 真库迁移失败。本地因无库全程 skip，直到 CI 起真 Postgres 才暴露。
//
// 本测试**不需要数据库**，因此每次 `go test ./...` 都会跑 —— 这正是关键：
// 没有真库也能拦住这个 bug。
func TestStripOuterTx_KeepsFunctionBodyBegin(t *testing.T) {
	in := `-- 注释
BEGIN;

CREATE OR REPLACE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'append-only: % not allowed', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE t (id int);

COMMIT;
`
	out := stripOuterTx(in)

	// 外层 BEGIN/COMMIT 必须被去掉
	if regexp.MustCompile(`(?im)^\s*BEGIN\s*;\s*$`).MatchString(out) {
		t.Error("外层 BEGIN; 未被移除")
	}
	if regexp.MustCompile(`(?im)^\s*COMMIT\s*;\s*$`).MatchString(out) {
		t.Error("外层 COMMIT; 未被移除")
	}

	// ★ 函数体的 BEGIN 与 END 必须**原样保留**
	if !strings.Contains(out, "BEGIN\n    RAISE EXCEPTION") {
		t.Errorf("函数体内的 BEGIN 被误删（这正是历史 bug）:\n%s", out)
	}
	if !strings.Contains(out, "END;") {
		t.Errorf("函数体内的 END; 丢失:\n%s", out)
	}
	if !strings.Contains(out, "CREATE OR REPLACE FUNCTION audit_log_immutable()") {
		t.Error("函数定义被破坏")
	}
	if !strings.Contains(out, "LANGUAGE plpgsql") {
		t.Error("函数尾部被破坏")
	}
	// 非事务语句必须保留
	if !strings.Contains(out, "CREATE TABLE t (id int);") {
		t.Error("普通语句被误删")
	}
}

// 无外层事务包裹时，必须原样返回（不做任何删改）。
func TestStripOuterTx_NoOuterTxLeavesContentIntact(t *testing.T) {
	in := "CREATE TABLE a (id int);\nCREATE TABLE b (id int);\n"
	if got := stripOuterTx(in); got != in {
		t.Errorf("无外层事务时被改动:\n原: %q\n新: %q", in, got)
	}
}

// 只有 BEGIN 没有 COMMIT ⇒ 保守起见不删（避免误伤）。
func TestStripOuterTx_OnlyBeginNotStripped(t *testing.T) {
	in := "BEGIN;\nCREATE TABLE a (id int);\n"
	if got := stripOuterTx(in); got != in {
		t.Error("不成对的 BEGIN 不应被删除（保守优先）")
	}
}

// 注释与空行包裹的成对事务应被识别并移除。
func TestStripOuterTx_HandlesCommentsAndBlankLines(t *testing.T) {
	in := "-- head\n\nBEGIN;\n\nSELECT 1;\n\nCOMMIT;\n\n-- tail\n"
	out := stripOuterTx(in)
	if strings.Contains(out, "BEGIN;") {
		t.Error("外层 BEGIN 未移除")
	}
	if strings.Contains(out, "COMMIT;") {
		t.Error("外层 COMMIT 未移除")
	}
	if !strings.Contains(out, "SELECT 1;") {
		t.Error("正文被误删")
	}
}

// ★ 端到端回归：真实迁移文件经 stripOuterTx 后，函数体必须完好。
//
// 这直接覆盖 `audit_log_immutable()`（G10 append-only 铁律的实现载体）——
// 若它残缺，审计防篡改就形同虚设。
func TestStripOuterTx_RealMigrationsSurvive(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "sql", "migrations")
	migs, err := LoadMigrations(dir)
	if err != nil {
		t.Fatalf("加载迁移失败：%v", err)
	}
	if len(migs) == 0 {
		t.Fatal("未发现迁移文件")
	}

	beginEnd := regexp.MustCompile(`(?is)AS \$\$(.*?)\$\$ LANGUAGE`)
	checked := 0
	for _, m := range migs {
		stripped := stripOuterTx(m.SQL)
		for _, fm := range beginEnd.FindAllStringSubmatch(m.SQL, -1) {
			body := fm[1]
			if !strings.Contains(strings.ToUpper(body), "BEGIN") {
				continue
			}
			checked++
			// 原文与剥离后的函数体必须一致
			gotBody := regexp.MustCompile(`(?is)AS \$\$(.*?)\$\$ LANGUAGE`).
				FindStringSubmatch(stripped)
			if gotBody == nil || gotBody[1] != body {
				t.Errorf("迁移 %s：函数体在 stripOuterTx 后被改动\n原: %q\n新: %v",
					m.Name, body, gotBody)
			}
		}
		// 剥离后不得残留行首裸 COMMIT;
		if regexp.MustCompile(`(?im)^\s*COMMIT\s*;\s*$`).MatchString(stripped) {
			t.Errorf("迁移 %s：剥离后仍残留行首 COMMIT;（会在事务内报错）", m.Name)
		}
	}
	if checked == 0 {
		t.Skip("当前迁移集未含 plpgsql 函数体，本断言无对象")
	}
	t.Logf("已校验 %d 个 plpgsql 函数体在剥离后保持原样", checked)
}

// 迁移文件必须真实存在且可解析（防止目录写错导致静默通过）。
func TestLoadMigrations_RealDir(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "sql", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取迁移目录失败：%v", err)
	}
	var want int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			want++
		}
	}
	migs, err := LoadMigrations(dir)
	if err != nil {
		t.Fatalf("LoadMigrations 失败：%v", err)
	}
	if len(migs) != want {
		t.Errorf("加载 %d 个，目录中有 %d 个 .sql", len(migs), want)
	}
	// 版本必须唯一且升序
	for i := 1; i < len(migs); i++ {
		if migs[i-1].Version >= migs[i].Version {
			t.Errorf("版本未严格升序：%s 后出现 %s", migs[i-1].Version, migs[i].Version)
		}
	}
}

// TestDSNFromEnv_AcceptsBothNames 锁定「两个 DSN 变量名都必须认」。
//
// ★ 这是 CI 真库步骤第二次失败的根因回归测试：
//   迁移器只认 SPARK_DB_DSN，CI 只设 SPARK_TEST_DB_DSN
//   ⇒ 真库迁移死于「未配置连接信息」，其后所有真库闸门全部 skip。
//
// 危险之处在于**沉默**：两侧各自看都自洽，谁也没报错到「配置名不一致」上，
// 只有在同一个 CI 作业里合排才炸。故这里把契约钉死。
func TestDSNFromEnv_AcceptsBothNames(t *testing.T) {
	// 三个都清干净，避免宿主机环境污染
	clearDSNEnv := func() {
		for _, k := range []string{
			"SPARK_DB_DSN", "SPARK_TEST_DB_DSN",
			"SPARK_PG_HOST", "SPARK_PG_USER",
		} {
			t.Setenv(k, "")
		}
	}

	t.Run("主名 SPARK_DB_DSN 生效", func(t *testing.T) {
		clearDSNEnv()
		t.Setenv("SPARK_DB_DSN", "postgres://u:p@h:5432/main?sslmode=disable")
		got, err := DSNFromEnv()
		if err != nil {
			t.Fatalf("期望成功，实际报错：%v", err)
		}
		if !strings.Contains(got, "/main") {
			t.Errorf("取到的不是主名 DSN：%s", RedactDSN(got))
		}
	})

	t.Run("测试期名 SPARK_TEST_DB_DSN 也生效（CI 只设这个）", func(t *testing.T) {
		clearDSNEnv()
		t.Setenv("SPARK_TEST_DB_DSN", "postgres://u:p@h:5432/citest?sslmode=disable")
		got, err := DSNFromEnv()
		if err != nil {
			t.Fatalf("★ 未识别 SPARK_TEST_DB_DSN —— CI 真库迁移会死于「未配置连接信息」：%v", err)
		}
		if !strings.Contains(got, "/citest") {
			t.Errorf("取到的不是测试期 DSN：%s", RedactDSN(got))
		}
	})

	t.Run("主名优先于测试期名", func(t *testing.T) {
		clearDSNEnv()
		t.Setenv("SPARK_DB_DSN", "postgres://u:p@h:5432/primary?sslmode=disable")
		t.Setenv("SPARK_TEST_DB_DSN", "postgres://u:p@h:5432/secondary?sslmode=disable")
		got, _ := DSNFromEnv()
		if !strings.Contains(got, "/primary") {
			t.Errorf("主名应优先，实际取到：%s", RedactDSN(got))
		}
	})

	t.Run("全空时报错（不静默返回空串）", func(t *testing.T) {
		clearDSNEnv()
		if got, err := DSNFromEnv(); err == nil {
			t.Fatalf("全空时应报错，实际返回 %q —— 空串会被当成有效配置往下走", got)
		}
	})

	t.Run("报错信息提到两个变量名，便于排障", func(t *testing.T) {
		clearDSNEnv()
		_, err := DSNFromEnv()
		if err == nil {
			t.Fatal("期望报错")
		}
		for _, want := range []string{"SPARK_DB_DSN", "SPARK_TEST_DB_DSN"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("报错未提及 %s，运维会找不到该设哪个：%v", want, err)
			}
		}
	})
}
