package gate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// G4 第八侧：槽与算法的**密级**（permission, L1..L4）必须真实可解析。
//
// 病：`permission` 被读进来、写进 DB，却没有任何判定消费（第十六个变种）。
// 本文件覆盖：① 声明层解析；② 与 DB CHECK 词表同源；③ 反向「不误伤」；
// ④ ★ 澄清：本侧**刻意不**断言「算法密级 ≤ 原料槽密级」的单调性 —— 已实测证伪。
//
// ★ 命名同源：本文件断言用到的档位，必须与 gate 包权威表一致 ——
// 若权威表改名而本文件没改，TestPermission_AuthoritativeTable 会先失败。

// ── 1. 声明层：可解析 / 不可解析 ────────────────────────────────────────────

func TestPermission_AuthoritativeTable(t *testing.T) {
	got := gate.PermissionLevels()
	want := []string{"L1", "L2", "L3", "L4"}
	if len(got) != len(want) {
		t.Fatalf("密级档位数量 = %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("密级档位[%d] = %q，期望 %q（docs/01 §13.3）", i, got[i], want[i])
		}
	}
}

func TestPermission_Declared_AcceptsKnownLevels(t *testing.T) {
	levels := gate.PermissionLevels()
	docs := make([]gate.PermissionDoc, 0, len(levels))
	for _, l := range levels {
		docs = append(docs, gate.PermissionDoc{Kind: "槽", ID: "slot." + l, Raw: l})
	}
	if v := gate.CheckPermissionDeclared(docs); len(v) != 0 {
		t.Fatalf("已知密级不应报违规，得到：%v", v)
	}
	// 夹具自证：每个声明都必须真的被 ParsePermission 认出，否则上一条可能恒真。
	for _, l := range levels {
		if _, ok := gate.ParsePermission(l); !ok {
			t.Fatalf("夹具自证失败：%q 应是已知密级", l)
		}
	}
}

func TestPermission_Declared_AcceptsLowercase(t *testing.T) {
	// 大小写归一：`l4` → `L4`（语义无歧义，容忍手写小写）。
	if got, ok := gate.ParsePermission("l4"); !ok || got != "L4" {
		t.Fatalf("ParsePermission(l4) = (%q,%v)，期望 (L4,true)", got, ok)
	}
	if got, ok := gate.ParsePermission("  l2 "); !ok || got != "L2" {
		t.Fatalf("ParsePermission( l2 ) = (%q,%v)，期望 (L2,true)", got, ok)
	}
}

func TestPermission_Declared_RejectsEmptyAndFreeText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"空声明", "", "未声明"},
		{"纯空白", "   ", "未声明"},
		{"中文自由文本", "高密", "不是已知密级"},
		{"中文档名", "核心", "不是已知密级"},
		{"越界档位", "L5", "不是已知密级"},
		{"零档", "L0", "不是已知密级"},
		{"英文别名", "internal", "不是已知密级"},
		{"带后缀", "L4级", "不是已知密级"},
		{"数字裸写", "4", "不是已知密级"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := gate.CheckPermissionDeclared([]gate.PermissionDoc{
				{Kind: "槽", ID: "slot.x", Raw: tc.raw},
			})
			if len(v) != 1 {
				t.Fatalf("raw=%q 应恰好报 1 条违规，得到 %d 条：%v", tc.raw, len(v), v)
			}
			if !strings.Contains(v[0], tc.want) {
				t.Fatalf("raw=%q 的报错 %q 未含 %q", tc.raw, v[0], tc.want)
			}
		})
	}
}

// 反向不误伤：报错必须定位到具体主体（槽/算法），否则无法修复。
func TestPermission_Declared_ReportsSubject(t *testing.T) {
	v := gate.CheckPermissionDeclared([]gate.PermissionDoc{
		{Kind: "槽", ID: "slot.qty", Raw: ""},
		{Kind: "算法", ID: "algo.cogs", Raw: "L9"},
	})
	if len(v) != 2 {
		t.Fatalf("应报 2 条，得到 %d：%v", len(v), v)
	}
	joined := strings.Join(v, "\n")
	for _, want := range []string{"slot.qty", "algo.cogs"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("报错未定位到 %s：%v", want, v)
		}
	}
}

// ── 2. 与 DB CHECK 词表同源 ─────────────────────────────────────────────────

func TestPermission_VocabularyMatchesDB(t *testing.T) {
	// 与实现同源：从 0002 迁移里读出真实 CHECK，而不是抄一份到测试里。
	root := repoRoot(t)
	sqlPath := filepath.Join(root, "sql", "migrations", "0002_slots_and_rules.sql")
	body, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("读迁移失败 %s：%v", sqlPath, err)
	}
	if v := gate.CheckPermissionVocabularyMatchesDB(string(body)); len(v) != 0 {
		t.Fatalf("Go 侧密级词表与 0002 的 CHECK 不同源（fail-closed）：%v", v)
	}
}

func TestPermission_VocabularyMatchesDB_DetectsDrift(t *testing.T) {
	// 负向：篡改 DB 词表（把 L4 换掉）必须被检出。
	tampered := "CREATE TABLE x (permission text NOT NULL CHECK (permission IN ('L1','L2','L3')))"
	v := gate.CheckPermissionVocabularyMatchesDB(tampered)
	if len(v) == 0 {
		t.Fatal("DB 词表缺 L4 应被检出（Go 放行而入库被拒 ⇒ 两侧分叉）")
	}
	if !strings.Contains(strings.Join(v, "\n"), "L4") {
		t.Fatalf("报错应点名 L4：%v", v)
	}
	// 反向：DB 多出一个 Go 侧不认的档位，也必须被检出。
	extra := "CREATE TABLE x (permission text NOT NULL CHECK (permission IN ('L1','L2','L3','L4','L5')))"
	v2 := gate.CheckPermissionVocabularyMatchesDB(extra)
	if len(v2) == 0 {
		t.Fatal("DB 含 L5 应被检出（入库允许而加载期认不出 ⇒ 字段名存实亡）")
	}
	if !strings.Contains(strings.Join(v2, "\n"), "L5") {
		t.Fatalf("报错应点名 L5：%v", v2)
	}
}

// ── 3. ★ 澄清：本侧刻意不做单调性断言 ──────────────────────────────────────

// TestPermission_NoMonotonicityAssertion —— 记录一个**已验证的否定结论**。
//
// 曾经的初版闸门断过「算法密级不得高于其原料槽密级」，上线首刻即报：
//
//	algo.cogs(L4) 密级高于其原料槽 slot.qty(L2)
//
// 复核后判定**该规则本身错误、而非数据错误**：
//   - algo.cogs = Σ(qty × cost_unit)；销量(L2) × 单件成本(L4) = 成本额合计(L4)，
//     业务上完全正当（「卖了多少件」属内部，而「每件成本多少钱」属核心）；
//   - 反向亦然：algo.gp(L3) = revenue(L3) − cogs(L4)，减法**降低**敏感度。
//
// 若强行断言单调，只能靠抬高 slot.qty 到 L4（过度限权）或压低 algo.cogs 到 L2
// （把核心成本降级 = 真正的安全回归）来满足 —— 两者都是伪造。故本侧放弃该断言，
// 只钉「可解析 + 与 DB 同源」。本用例把这个否定结论固化下来，防止日后有人
// 又把单调性写回去（那样会再次误报、并诱使有人去改数据迁就错误的规则）。
func TestPermission_NoMonotonicityAssertion(t *testing.T) {
	// 真实的、正当的、非单调的样例：高低两个方向各一。
	docs := []gate.PermissionDoc{
		{Kind: "槽", ID: "slot.qty", Raw: "L2"},
		{Kind: "槽", ID: "slot.cost_unit", Raw: "L4"},
		{Kind: "算法", ID: "algo.cogs", Raw: "L4"}, // L4 成品 ← L2 体量原料（合法）
		{Kind: "槽", ID: "slot.revenue", Raw: "L3"},
		{Kind: "槽", ID: "slot.cogs", Raw: "L4"},
		{Kind: "算法", ID: "algo.gp", Raw: "L3"}, // L3 成品 ← L4 原料（合法）
	}
	if v := gate.CheckPermissionDeclared(docs); len(v) != 0 {
		t.Fatalf("这些非单调但正当的密级不应报违规（单调性不是不变量）：%v", v)
	}
}

// repoRoot 从本测试文件位置回溯到仓库根（gate 包在 backend/internal/gate）。
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// backend/internal/gate → backend → <root>
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}
