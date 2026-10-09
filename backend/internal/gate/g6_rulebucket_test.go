// g6_rulebucket_test.go —— G4 第十六侧（规则 ⇄ 桶 双向声明对平）的**判定函数**自测。
//
// 分层纪律（与其余各侧一致）：本文件只测判定函数本身；
// 「真仓库 YAML 必须通过 + 生产调用点真的存在」由 `internal/rule/rulebucket_test.go`
// 从加载入口进、以成败出（那里才是链路级证据）。
package gate_test

import (
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// 与真仓库同形的夹具：规则 rule.r1 / rule.r2 都声明影响桶 b1；
// 桶 b1 的 rule_versions 同时记录两条规则 —— 双向一致。
func okRuleBucketFixture() ([]gate.RuleBucketLink, []gate.BucketDoc) {
	rules := []gate.RuleBucketLink{
		{RuleID: "rule.r1", AppliesToBuckets: []string{"b1"}},
		{RuleID: "rule.r2", AppliesToBuckets: []string{"b1"}},
	}
	buckets := []gate.BucketDoc{
		{ID: "b1", RuleVersions: map[string]int{"rule.r1": 3, "rule.r2": 1}},
	}
	return rules, buckets
}

func TestRuleBucket_BidirectionalAccepts(t *testing.T) {
	rules, buckets := okRuleBucketFixture()
	if v := gate.CheckRuleBucketBidirectional(rules, buckets); len(v) != 0 {
		t.Fatalf("双向一致的夹具不应报违规，实际：%v", v)
	}
}

// ★ 夹具自证（判别性）：把正向那条关系摘掉，必须**立刻变红** ——
// 否则上面那条「通过」可能只是因为判定函数恒放行。
func TestRuleBucket_FixtureSelfProof(t *testing.T) {
	rules, buckets := okRuleBucketFixture()
	// 摘掉桶侧对 rule.r1 的回指。
	delete(buckets[0].RuleVersions, "rule.r1")
	v := gate.CheckRuleBucketBidirectional(rules, buckets)
	if len(v) == 0 {
		t.Fatal("★ 夹具自证失败：摘掉 rule.r1 的回指后仍通过 ⇒ 上一条断言可能恒真")
	}
	if !strings.Contains(v[0], "rule.r1") || !strings.Contains(v[0], "b1") {
		t.Fatalf("报错应同时点名规则与桶，实际：%v", v)
	}
}

// 正向缺口：规则声明影响桶，桶侧 `rule_versions` 却沉默 ⇒
// G6 的版本漂移检测永远看不见这条规则（费率改了桶不重算）。
func TestRuleBucket_ForwardMissingOnBucketSide(t *testing.T) {
	rules := []gate.RuleBucketLink{
		{RuleID: "rule.r1", AppliesToBuckets: []string{"b1"}},
	}
	buckets := []gate.BucketDoc{
		{ID: "b1", RuleVersions: map[string]int{"rule.other": 1}},
	}
	v := gate.CheckRuleBucketBidirectional(rules, buckets)
	if len(v) != 1 {
		t.Fatalf("应恰好报 1 条正向缺口，实际 %d 条：%v", len(v), v)
	}
	if !strings.Contains(v[0], "rule.r1") || !strings.Contains(v[0], "rule_versions") {
		t.Fatalf("报错应指出「规则侧声明了、桶侧 rule_versions 没有」，实际：%v", v[0])
	}
}

// 反向缺口：桶记录某规则，规则侧 `applies_to_buckets` 却不认这个桶 ⇒
// `rule.AffectedBuckets` 漏掉该桶（按规则变更重算的流水线漏算）。
//
// 注意方向与正向是**互斥**的（同一对 (规则,桶) 不可能同时两个方向都缺），
// 故这里把规则侧的 applies_to_buckets 全部留空，只让反向暴露出来。
func TestRuleBucket_ReverseMissingOnRuleSide(t *testing.T) {
	rules := []gate.RuleBucketLink{
		{RuleID: "rule.r1"},
		{RuleID: "rule.r2"},
	}
	buckets := []gate.BucketDoc{
		{ID: "b1", RuleVersions: map[string]int{"rule.r1": 3}},
		{ID: "b2", RuleVersions: map[string]int{"rule.r2": 1}},
	}
	v := gate.CheckRuleBucketBidirectional(rules, buckets)
	if len(v) != 2 {
		t.Fatalf("应报 2 条反向缺口（b1→r1 与 b2→r2），实际 %d 条：%v", len(v), v)
	}
	for _, s := range v {
		if !strings.Contains(s, "applies_to_buckets") {
			t.Fatalf("反向报错应指出 applies_to_buckets 未回指，实际：%v", s)
		}
	}
}

// ★ 空输入不得视为通过：空集合会让「因为没东西可查」恒真（假闸门形态）。
func TestRuleBucket_EmptyInputsAreNotVacuous(t *testing.T) {
	if v := gate.CheckRuleBucketBidirectional(nil, []gate.BucketDoc{{ID: "b1"}}); len(v) == 0 {
		t.Fatal("★ 规则集合为空时必须报违规（否则对平恒真）")
	}
	if v := gate.CheckRuleBucketBidirectional([]gate.RuleBucketLink{{RuleID: "rule.r1"}}, nil); len(v) == 0 {
		t.Fatal("★ 桶集合为空时必须报违规（否则对平恒真）")
	}
}

// ★ 固化一条**否定结论**：本侧**不**重复报告「对侧不存在」——
// 那两类已由 CheckRuleIDsRegistered（桶不存在）与
// CheckBucketRuleVersionsRegistered（规则不存在）分别关把；
// 本侧若也报，同一处缺陷会被两条闸门各报一次，注入破坏的分辨会变含糊。
func TestRuleBucket_DoesNotReportNonexistentCounterparts(t *testing.T) {
	rules := []gate.RuleBucketLink{
		{RuleID: "rule.r1", AppliesToBuckets: []string{"ghost_bucket"}},
	}
	buckets := []gate.BucketDoc{
		{ID: "b1", RuleVersions: map[string]int{"rule.ghost": 1}},
	}
	if v := gate.CheckRuleBucketBidirectional(rules, buckets); len(v) != 0 {
		t.Fatalf("「对侧不存在」应由既有两条闸门报，本侧不应重复，实际：%v", v)
	}
}

// 空桶名（`applies_to_buckets` 里写了空串）必须被点名。
func TestRuleBucket_RejectsEmptyBucketName(t *testing.T) {
	rules := []gate.RuleBucketLink{
		{RuleID: "rule.r1", AppliesToBuckets: []string{"  "}},
	}
	// 桶侧不记录任何规则，隔离出「空桶名」这一条。
	buckets := []gate.BucketDoc{{ID: "b1", RuleVersions: map[string]int{}}}
	v := gate.CheckRuleBucketBidirectional(rules, buckets)
	if len(v) != 1 || !strings.Contains(v[0], "空桶名") {
		t.Fatalf("空桶名应被点名（且只报这一条），实际：%v", v)
	}
}

// 去重：同一规则重复声明同一个桶（YAML 里写两遍）不应报两条。
func TestRuleBucket_DuplicateDeclarationsAreDeduped(t *testing.T) {
	rules := []gate.RuleBucketLink{
		{RuleID: "rule.r1", AppliesToBuckets: []string{"b1", "b1"}},
	}
	buckets := []gate.BucketDoc{{ID: "b1", RuleVersions: map[string]int{}}}
	v := gate.CheckRuleBucketBidirectional(rules, buckets)
	if len(v) != 1 {
		t.Fatalf("重复声明应只报 1 条，实际 %d 条：%v", len(v), v)
	}
}

// 真仓库形状：两条规则都指向 pnl_month，桶记录两条 —— 必须通过。
func TestRuleBucket_RealRepoShape(t *testing.T) {
	rules := []gate.RuleBucketLink{
		{RuleID: "rule.tk.fee", AppliesToBuckets: []string{"pnl_month"}},
		{RuleID: "rule.cost.gate", AppliesToBuckets: []string{"pnl_month"}},
	}
	buckets := []gate.BucketDoc{
		{ID: "pnl_month", RuleVersions: map[string]int{"rule.tk.fee": 6, "rule.cost.gate": 1}},
	}
	if v := gate.CheckRuleBucketBidirectional(rules, buckets); len(v) != 0 {
		t.Fatalf("真仓库形状应通过，实际：%v", v)
	}
}
