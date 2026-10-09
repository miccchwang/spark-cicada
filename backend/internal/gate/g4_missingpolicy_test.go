package gate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// G4 第十三侧：算法的**缺失策略**（missing_policy, skip/null/error）必须真实可解析、
// 与 DB CHECK 词表同源，并区分「漏写」与「写了裸 null」两种不同缺陷。
//
// 病：`missing_policy` 被读进来、写进 DB，却没有任何判定消费其声明值（第二十一个变种）；
// 更严重的是运行时只实现 skip，`error`/`null`/乱写行为完全等价（声明等于装饰）。
//
// ★ 本轮还抓出一个真实陷阱：`missing_policy: null`（裸写，按 docs 三值定义）
// 会被 YAML 解析成**空值** ⇒ 被缺省填充静默改成 "skip"（语义反转）。本文件
// 断言闸门能把这种情形与「漏写」分开报。

// ── 1. 声明层：词表与解析 ───────────────────────────────────────────────────

func TestMissingPolicy_AuthoritativeTable(t *testing.T) {
	got := gate.MissingPolicies()
	want := []string{"skip", "null", "error"}
	if len(got) != len(want) {
		t.Fatalf("缺失策略数量 = %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("缺失策略[%d] = %q，期望 %q（docs/03 §3.1）", i, got[i], want[i])
		}
	}
}

func TestMissingPolicy_Parse_AcceptsKnownAndNormalizesCase(t *testing.T) {
	cases := map[string]string{
		"skip": "skip", "SKIP": "skip", "  Skip ": "skip",
		"null": "null", "NULL": "null",
		"error": "error", "Error": "error",
	}
	for raw, want := range cases {
		got, ok := gate.ParseMissingPolicy(raw)
		if !ok || got != want {
			t.Fatalf("ParseMissingPolicy(%q) = (%q,%v)，期望 (%q,true)", raw, got, ok, want)
		}
	}
	// 夹具自证：权威表里每个值都必须真的能被解析出来（否则上一条可能恒真）。
	for _, p := range gate.MissingPolicies() {
		if _, ok := gate.ParseMissingPolicy(p); !ok {
			t.Fatalf("夹具自证失败：%q 应是已知策略", p)
		}
	}
}

func TestMissingPolicy_Parse_RejectsUnknown(t *testing.T) {
	for _, raw := range []string{"", "   ", "ignore", "raise", "skip_if_missing", "报错", "0", "nul"} {
		if _, ok := gate.ParseMissingPolicy(raw); ok {
			t.Fatalf("ParseMissingPolicy(%q) 不应被接受（别名/自由文本是「写了等于没写」的入口）", raw)
		}
	}
}

func TestMissingPolicy_Declared_ReportsSubject(t *testing.T) {
	v := gate.CheckMissingPolicyDeclared([]gate.MissingPolicyDoc{
		{AlgoID: "algo.a", Raw: "", Declared: false},
		{AlgoID: "algo.b", Raw: "ignore", Declared: true},
	})
	if len(v) != 2 {
		t.Fatalf("应报 2 条，得到 %d：%v", len(v), v)
	}
	joined := strings.Join(v, "\n")
	for _, want := range []string{"algo.a", "algo.b"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("报错未定位到 %s：%v", want, v)
		}
	}
}

// ── 2. ★ 裸 null 陷阱：区分「漏写」与「写了 YAML 空值」────────────────────────

func TestMissingPolicy_Declared_DistinguishesYAMLNullFromMissing(t *testing.T) {
	// 漏写：Declared=false, Raw=""
	missing := gate.CheckMissingPolicyDeclared([]gate.MissingPolicyDoc{
		{AlgoID: "algo.x", Raw: "", Declared: false},
	})
	if len(missing) != 1 || !strings.Contains(missing[0], "未声明") {
		t.Fatalf("漏写应报「未声明」，得到：%v", missing)
	}

	// 写了裸 null（YAML 空值）：Declared=true, Raw=""
	yamlNull := gate.CheckMissingPolicyDeclared([]gate.MissingPolicyDoc{
		{AlgoID: "algo.y", Raw: "", Declared: true},
	})
	if len(yamlNull) != 1 {
		t.Fatalf("裸 null 应报 1 条，得到 %d：%v", len(yamlNull), yamlNull)
	}
	if !strings.Contains(yamlNull[0], "YAML 空值") {
		t.Fatalf("裸 null 的报错应点明「YAML 空值」，得到：%v", yamlNull)
	}
	if !strings.Contains(yamlNull[0], `"null"`) {
		t.Fatalf("裸 null 的报错应给出「写带引号的 \"null\"」的可行动提示，得到：%v", yamlNull)
	}
	// 两种缺陷的报错必须**不同**（修法不同），否则提示无意义。
	if yamlNull[0] == missing[0] {
		t.Fatal("「漏写」与「写了裸 null」的报错不应完全相同（两者修法不同）")
	}
}

func TestMissingPolicy_Declared_AcceptsAllThree(t *testing.T) {
	docs := make([]gate.MissingPolicyDoc, 0, 3)
	for _, p := range gate.MissingPolicies() {
		docs = append(docs, gate.MissingPolicyDoc{AlgoID: "algo." + p, Raw: p, Declared: true})
	}
	if v := gate.CheckMissingPolicyDeclared(docs); len(v) != 0 {
		t.Fatalf("三个已知策略不应报违规：%v", v)
	}
}

// ── 3. 与 DB CHECK 词表同源 ─────────────────────────────────────────────────

func TestMissingPolicy_VocabularyMatchesDB(t *testing.T) {
	root := repoRoot(t)
	sqlPath := filepath.Join(root, "sql", "migrations", "0002_slots_and_rules.sql")
	body, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("读迁移失败 %s：%v", sqlPath, err)
	}
	if v := gate.CheckMissingPolicyVocabularyMatchesDB(string(body)); len(v) != 0 {
		t.Fatalf("Go 侧缺失策略词表与 0002 的 CHECK 不同源（fail-closed）：%v", v)
	}
}

func TestMissingPolicy_VocabularyMatchesDB_DetectsDrift(t *testing.T) {
	// 负向：DB 词表少了 error 必须被检出。
	tampered := "CREATE TABLE x (missing_policy text NOT NULL CHECK (missing_policy IN ('skip','null')))"
	v := gate.CheckMissingPolicyVocabularyMatchesDB(tampered)
	if len(v) == 0 {
		t.Fatal("DB 词表缺 error 应被检出（Go 放行而入库被拒 ⇒ 两侧分叉）")
	}
	if !strings.Contains(strings.Join(v, "\n"), "error") {
		t.Fatalf("报错应点名 error：%v", v)
	}
	// 反向：DB 多出一个 Go 侧不认的取值，也必须被检出。
	extra := "CREATE TABLE x (missing_policy text NOT NULL CHECK (missing_policy IN ('skip','null','error','zero')))"
	v2 := gate.CheckMissingPolicyVocabularyMatchesDB(extra)
	if len(v2) == 0 {
		t.Fatal("DB 含 zero 应被检出（入库允许而加载期认不出 ⇒ 字段名存实亡）")
	}
	if !strings.Contains(strings.Join(v2, "\n"), "zero") {
		t.Fatalf("报错应点名 zero：%v", v2)
	}
}
