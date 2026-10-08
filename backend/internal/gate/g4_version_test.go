// G4 第十侧（版本号）的**判定函数**自测。
//
// 分层纪律（与其余九侧一致）：本文件只测判定函数本身；
// 「生产路径是否真的调用了它」由 `slot/g4_version_wiring_test.go` 的
// 静态源码扫描负责 —— 两者缺一不可。
package gate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ───────────────── ParseAlgorithmVersion：解析自证 ─────────────────

func TestVersion_ParseAcceptsPositiveIntegers(t *testing.T) {
	cases := map[string]int{
		"1":      1,
		"3":      3,
		"42":     42,
		" 7 ":    7,
		`"3"`:    3, // YAML 里加引号允许
		"100000": 100000,
	}
	for raw, want := range cases {
		got, why := gate.ParseAlgorithmVersion(raw)
		if why != "" {
			t.Errorf("ParseAlgorithmVersion(%q) 应被接受，实际：%s", raw, why)
			continue
		}
		if got != want {
			t.Errorf("ParseAlgorithmVersion(%q)=%d，期望 %d", raw, got, want)
		}
	}
}

func TestVersion_ParseRejectsNonIntegerForms(t *testing.T) {
	// 每一条都是**看起来合理但解析不出数值**的写法 —— 版本是数值比较对象，
	// 解析不出来就退化成「不影响任何事」（本仓的病根）。
	bad := []string{
		"", // 缺失
		// 只有空白：Trim 后为空 ⇒ 必须拒
		"   ",
		// 只有引号：剥引号后为空 ⇒ 必须拒
		`""`,
		"v3",      // 常见但非整数写法
		"V3",      // 同上
		"3.1",     // 语义化版本
		"3.0",     // YAML 里会落到 float64 —— 必须拒，不得悄悄取整成 3
		"3-2",     // 自由文本
		"latest",  // 自由文本
		"三",       // 中文数字
		"3 4",     // 内含空格：不是「纯整数」写法
		"0",       // 非正数：0 与「未声明」无法区分
		"-1",      // 非正数
		"+3",      // 带符号：不是「纯整数」写法
		"1000001", // 超过合理上界（疑似多写一位）
	}
	for _, raw := range bad {
		if _, why := gate.ParseAlgorithmVersion(raw); why == "" {
			t.Errorf("★ ParseAlgorithmVersion(%q) 竟被接受 ⇒ 版本号失去判据", raw)
		}
	}
}

func TestVersion_ParseErrorNamesTheOffendingLiteral(t *testing.T) {
	// 报错必须点名**作者写下的那串字符**，否则定位价值等于零。
	_, why := gate.ParseAlgorithmVersion("v3")
	if !strings.Contains(why, "v3") {
		t.Fatalf("报错应点名原文 v3，实际：%s", why)
	}
	_, why = gate.ParseAlgorithmVersion("0")
	if !strings.Contains(why, "非正数") {
		t.Fatalf("0 的报错应说明「非正数」，实际：%s", why)
	}
}

// ───────────────── CheckAlgorithmVersionDeclared：声明层 ─────────────────

func TestVersion_DeclaredRejectsMissingAndGarbage(t *testing.T) {
	docs := []gate.VersionDoc{
		{AlgoID: "algo.ok", Raw: "3"},
		{AlgoID: "algo.missing", Raw: ""},
		{AlgoID: "algo.garbage", Raw: "latest"},
		{AlgoID: "algo.zero", Raw: "0"},
	}
	v := gate.CheckAlgorithmVersionDeclared(docs)
	if len(v) != 3 {
		t.Fatalf("应报 3 条（missing/garbage/zero），实际 %d 条：\n%s", len(v), strings.Join(v, "\n"))
	}
	joined := strings.Join(v, "\n")
	for _, id := range []string{"algo.missing", "algo.garbage", "algo.zero"} {
		if !strings.Contains(joined, id) {
			t.Errorf("报错应点名 %s，实际：\n%s", id, joined)
		}
	}
	if strings.Contains(joined, "algo.ok") {
		t.Errorf("合法的 algo.ok 不应被报，实际：\n%s", joined)
	}
	// 反向：全部合法时不报（防「恒真报警」）。
	if v := gate.CheckAlgorithmVersionDeclared([]gate.VersionDoc{
		{AlgoID: "algo.a", Raw: "1"}, {AlgoID: "algo.b", Raw: "9"},
	}); len(v) != 0 {
		t.Fatalf("全部合法的输入不应报违规，实际：\n%s", strings.Join(v, "\n"))
	}
}

// ───────────────── CheckAlgorithmVersionNotRegressing：唯一有依据的比较 ─────────────────

func TestVersion_NotRegressing(t *testing.T) {
	// 首次登记（无基准）⇒ 放行，不得因为「没有历史」而拒绝。
	if v := gate.CheckAlgorithmVersionNotRegressing("algo.a", 0, 1); len(v) != 0 {
		t.Fatalf("首次登记应放行，实际：%s", strings.Join(v, "\n"))
	}
	// 前进与持平 ⇒ 放行。
	if v := gate.CheckAlgorithmVersionNotRegressing("algo.a", 2, 3); len(v) != 0 {
		t.Fatalf("版本前进应放行，实际：%s", strings.Join(v, "\n"))
	}
	if v := gate.CheckAlgorithmVersionNotRegressing("algo.a", 3, 3); len(v) != 0 {
		t.Fatalf("版本持平（如仅改注释）应放行，实际：%s", strings.Join(v, "\n"))
	}
	// 倒退 ⇒ 必须拒。
	v := gate.CheckAlgorithmVersionNotRegressing("algo.a", 3, 2)
	if len(v) == 0 {
		t.Fatal("★ 版本从 3 倒退到 2 竟被放行 ⇒ 版本号没有被真正比较（本侧的全部价值就在这条）")
	}
	if !strings.Contains(v[0], "3") || !strings.Contains(v[0], "2") {
		t.Fatalf("报错应同时点出前后版本，实际：%s", v[0])
	}
}

// ───────────────── CheckAlgorithmVersionMatchesDB：与 DB 语义同源 ─────────────────

func TestVersion_MatchesDB_RealMigrations(t *testing.T) {
	// ★ 用**仓库里的真迁移文本**核对（不是手搓串）。
	root := repoRoot(t)
	ddl, err := os.ReadFile(filepath.Join(root, "sql", "migrations", "0002_slots_and_rules.sql"))
	if err != nil {
		t.Fatalf("读 0002 迁移失败: %v", err)
	}
	seed, err := os.ReadFile(filepath.Join(root, "sql", "migrations", "0004_registry_seed.sql"))
	if err != nil {
		t.Fatalf("读 0004 种子失败: %v", err)
	}
	if v := gate.CheckAlgorithmVersionMatchesDB(string(ddl), string(seed)); len(v) != 0 {
		t.Fatalf("真迁移应同源（种子里 version 列的字面量必须都是正整数）：\n%s", strings.Join(v, "\n"))
	}
	// 夹具自证：确实读到了内容（否则「空文件也绿」）。
	if len(ddl) < 1000 || len(seed) < 1000 {
		t.Fatalf("迁移文本过短（%d/%d 字节）—— 夹具没读到真文件，本用例是假的", len(ddl), len(seed))
	}
	if !strings.Contains(string(seed), "registry_algorithm") {
		t.Fatalf("0004 种子里没有 registry_algorithm —— 夹具盯错文件了")
	}
}

func TestVersion_MatchesDB_DetectsSeedDrift(t *testing.T) {
	// 反向自证①：种子里 version 列出现非整数字面量 ⇒ 必须被检出。
	seed := "INSERT INTO registry_algorithm (id, version, unit) VALUES ('algo.x', 'v3', 'THB');"
	v := gate.CheckAlgorithmVersionMatchesDB("", seed)
	if len(v) == 0 {
		t.Fatal("种子里 version 写成 'v3' 应被检出（否则种子入库成功、加载期认不出）")
	}
	if !strings.Contains(strings.Join(v, "\n"), "v3") {
		t.Fatalf("报错应点名 v3，实际：\n%s", strings.Join(v, "\n"))
	}

	// 反向自证②：必须**按列位取** —— version 不在列清单里时不得臆测。
	noVer := "INSERT INTO registry_algorithm (id, unit) VALUES ('algo.x', 'THB');"
	if v := gate.CheckAlgorithmVersionMatchesDB("", noVer); len(v) != 0 {
		t.Fatalf("列清单里没有 version 时不应报（不臆测），实际：\n%s", strings.Join(v, "\n"))
	}

	// 反向自证③：别的列里的合法数字**不得**被误当成版本号。
	otherCols := "INSERT INTO registry_algorithm (id, version, unit) VALUES ('algo.x', 1, 'THB');"
	if v := gate.CheckAlgorithmVersionMatchesDB("", otherCols); len(v) != 0 {
		t.Fatalf("合法种子不应报违规，实际：\n%s", strings.Join(v, "\n"))
	}
}

func TestVersion_MatchesDB_DetectsDDLCheckDrift(t *testing.T) {
	// 0002 若给 version 加了**更严的下界**，Go 侧能解析、DB 侧却会拒 ⇒ 必须报。
	ddl := "version integer NOT NULL CHECK (version > 1000),"
	v := gate.CheckAlgorithmVersionMatchesDB(ddl, "")
	if len(v) == 0 {
		t.Fatal("0002 的 version CHECK 要求 > 1000 而 Go 侧接受任意正整数 ⇒ 应被检出")
	}
	if !strings.Contains(strings.Join(v, "\n"), "1000") {
		t.Fatalf("报错应点名 1000，实际：\n%s", strings.Join(v, "\n"))
	}
	// 反向：`CHECK (version > 0)` 与本侧口径一致 ⇒ 不得报（防「恒真报警」）。
	if v := gate.CheckAlgorithmVersionMatchesDB("version integer NOT NULL CHECK (version > 0),", ""); len(v) != 0 {
		t.Fatalf("与 Go 侧一致的 DDL CHECK 不应报违规，实际：\n%s", strings.Join(v, "\n"))
	}
}
