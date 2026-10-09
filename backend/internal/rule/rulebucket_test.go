// rulebucket_test.go —— G4 第十六侧（规则 ⇄ 桶 双向声明对平）的**链路级**闸门。
//
// 承袭本仓「双向声明、零对平」这个病（第三处；前两处：G1 契约镜像 / G9 模块清单）：
//
//	「规则 ⇄ 桶」这一对关系在本仓由**两个目录、两套字段**各自声明：
//	  规则 → 桶：`rules/*.yaml` 的 `applies_to_buckets`（供 rule.AffectedBuckets）；
//	  桶 → 规则：`buckets/*.yaml` 的 `rule_versions`（供 gate.VersionDrift）。
//	此前两侧各自只校验「引用对象存在」，**从无对平** ⇒ 任一侧漏改都不会变红，
//	后果是 G6 的两条重算触发路径分叉（改了费率，两边对「该不该重算」说法不一）。
//
// 本文件从 `LoadRuleRegistry` / `LoadBucketRegistry` **入口**进、以
// `ValidateBucketLinks` 成败出，而不是直接调 `gate.CheckRuleBucketBidirectional`
// 看返回值 —— 判定函数自身的分层自测在 `internal/gate/g6_rulebucket_test.go`。
package rule_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ───────────────────── 正向：真仓库两侧声明必须一致 ─────────────────────

// TestRuleBucketLinks_RealRepoBidirectional 真仓库的 rules/*.yaml 与 buckets/*.yaml
// 必须双向一致（sparkd 启动时走的就是这条路径）。
func TestRuleBucketLinks_RealRepoBidirectional(t *testing.T) {
	_, buckets, rules := loadAll(t)

	// 防恒真：两侧输入都必须非空（空集合会让对平「因为没东西可查」而永远通过）。
	links := rules.RuleBucketLinks()
	if len(links) == 0 {
		t.Fatal("★ 规则集合为空 —— 双向对平会恒真（假闸门形态）")
	}
	docs := buckets.BucketDocs()
	if len(docs) == 0 {
		t.Fatal("★ 桶集合为空 —— 双向对平会恒真（假闸门形态）")
	}

	if err := rules.ValidateBucketLinks(docs); err != nil {
		t.Fatalf("★ 真仓库的规则⇄桶 双向声明不一致：%v", err)
	}

	// 真数据自证：rule.tk.fee 两侧都必须有这条关系（否则上面的「通过」可能因为
	// 夹具里根本没有这条关系而空转）。
	var seen bool
	for _, l := range links {
		if l.RuleID != "rule.tk.fee" {
			continue
		}
		for _, b := range l.AppliesToBuckets {
			if b == "pnl_month" {
				seen = true
			}
		}
	}
	if !seen {
		t.Fatal("夹具自证失败：rule.tk.fee 的 applies_to_buckets 应含 pnl_month")
	}
	b, ok := buckets.Bucket("pnl_month")
	if !ok {
		t.Fatal("夹具自证失败：pnl_month 应存在")
	}
	if _, ok := b.RuleVersions["rule.tk.fee"]; !ok {
		t.Fatal("夹具自证失败：pnl_month 的 rule_versions 应含 rule.tk.fee")
	}
}

// ───────────────────── 反向：两侧任一漏改都必须被拦下 ─────────────────────

// TestRuleBucketLinks_DetectsBucketSideSilence 桶侧漏掉规则的回指 ⇒ 生产方法必须报错。
//
// 这正是此前**静默**的场景：规则声明了 applies_to_buckets 含 pnl_month，
// 而桶的 rule_versions 把它删掉 —— 没有任何既有闸门会变红。
func TestRuleBucketLinks_DetectsBucketSideSilence(t *testing.T) {
	_, buckets, rules := loadAll(t)

	docs := buckets.BucketDocs()
	mutated := false
	for i := range docs {
		if docs[i].ID != "pnl_month" {
			continue
		}
		if _, ok := docs[i].RuleVersions["rule.tk.fee"]; ok {
			delete(docs[i].RuleVersions, "rule.tk.fee")
			mutated = true
		}
	}
	if !mutated {
		t.Fatal("夹具自证失败：未能从 pnl_month 的 rule_versions 里删掉 rule.tk.fee")
	}

	err := rules.ValidateBucketLinks(docs)
	if err == nil {
		t.Fatal("★ 桶侧删掉 rule.tk.fee 的回指后仍通过 ⇒ 双向对平未生效（G6 漂移检测会静默漏算）")
	}
	if !strings.Contains(err.Error(), "rule.tk.fee") {
		t.Errorf("报错应点名 rule.tk.fee，实际：%v", err)
	}
}

// TestRuleBucketLinks_EmptyBucketDocsFailClosed 空桶集合必须 fail-closed。
func TestRuleBucketLinks_EmptyBucketDocsFailClosed(t *testing.T) {
	_, _, rules := loadAll(t)
	if err := rules.ValidateBucketLinks(nil); err == nil {
		t.Fatal("★ 空桶集合必须报错（空集合会让双向对平恒真）")
	}
}

// ───────────────────── 静态接线：钉住生产调用点 ─────────────────────

// TestWiring_RuleBucketLinkGateHasProductionCallSite ★ 接线：
// 判定函数必须有**非测试**调用点，且 sparkd 启动期必须真的执行它。
//
// 断的是**完整调用语句**（而不是函数名）—— 参 G4 第十四侧的教训：
// `_ = f(...)`（引用但丢弃结果）会骗过只断函数名的静态断言。
func TestWiring_RuleBucketLinkGateHasProductionCallSite(t *testing.T) {
	root := repoRoot(t)
	backend := filepath.Join(root, "backend")

	// ① 纯判定函数：必须有非测试调用点，且在 rule 包内（ValidateBucketLinks 包装它）。
	sites := nonTestCallSites(t, backend, "CheckRuleBucketBidirectional")
	if len(sites) == 0 {
		t.Fatal("★ gate.CheckRuleBucketBidirectional 没有任何**非测试**调用点 ⇒ 该闸门恒真")
	}
	foundRule := false
	for _, s := range sites {
		if strings.Contains(s, "internal/rule/rule.go") {
			foundRule = true
		}
	}
	if !foundRule {
		t.Errorf("★ 期望生产调用点在 internal/rule/rule.go，实际 %v", sites)
	}

	// ② 包装方法：必须被 sparkd 在启动期调用（否则该调用点在生产部署时不会执行）。
	//
	// ★ 断「**完整调用语句**」（含 `if err := ` 前缀与 `; err != nil {` 后缀），
	//   而不是只断函数名 —— 实测教训（本轮注入 E 首轮 MISSED）：只断
	//   `rules.ValidateBucketLinks(buckets.BucketDocs())` 这段子串时，
	//   把结果包进 `func() error { _ = rules.ValidateBucketLinks(...); return nil }()`
	//   仍能命中（子串还在），于是「引用但丢弃结果」的假接线骗过了断言。
	//   与 G4 第十四侧的教训同族：**任何存在性断言都要先问「这段文本里什么才算真的调用」**。
	wiring, err := os.ReadFile(filepath.Join(backend, "cmd", "sparkd", "wiring.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(wiring)
	const wantStmt = "if err := rules.ValidateBucketLinks(buckets.BucketDocs()); err != nil {"
	if !strings.Contains(body, wantStmt) {
		t.Fatal("★ sparkd/wiring.go 未以**完整语句**调用 rules.ValidateBucketLinks(buckets.BucketDocs())" +
			"（须含 `if err := …; err != nil {`）—— 规则⇄桶 双向对平在生产部署时不存在或被丢弃结果")
	}
	// 结果必须进 specIssues（可观测，不得静默降级）。
	idx := strings.Index(body, wantStmt)
	tail := body[idx:]
	if len(tail) > 400 {
		tail = tail[:400]
	}
	if !strings.Contains(tail, "specIssues") {
		t.Error("★ 对平失败必须进 specIssues（/healthz 可观测），不得静默")
	}
}

// 固化一条**否定结论**（本轮注入 E 首轮 MISSED 逼出来的）：
// 「断完整语句」的判据必须能区分真接线与「包一层、丢弃结果」的假接线。
//
// 若哪天有人把断言放宽回「只断函数名子串」，本用例会立刻变红。
func TestWiring_RuleBucketLinkStatementIsNotSubstringFooled(t *testing.T) {
	const wantStmt = "if err := rules.ValidateBucketLinks(buckets.BucketDocs()); err != nil {"
	// 假接线：调用还在（子串仍在），但结果被丢弃 —— 闸门在生产上不生效。
	fake := "if err := func() error { _ = rules.ValidateBucketLinks(buckets.BucketDocs()); return nil }(); err != nil {"
	if strings.Contains(fake, wantStmt) {
		t.Fatal("★ 断言被『包一层丢弃结果』的写法骗过 —— 必须断**完整调用语句**，不能只断子串")
	}
	// 真接线（含完整语句 + 错误分支）必须命中。
	real := wantStmt + "\n\t\tp.specIssues = append(p.specIssues, \"x\")\n\t\treturn\n\t}"
	if !strings.Contains(real, wantStmt) {
		t.Fatal("★ 真接线的完整语句反而没命中 ⇒ 断言写错了")
	}
}

// 证明「桶→gate.BucketDoc 的字段映射只有一处」：BucketDocs() 必须被 validate 复用，
// 否则 sparkd 侧与加载期的口径会各写一份、必然漂移（G1/G9 的教训）。
func TestWiring_BucketDocsIsSingleSourceOfMapping(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "backend", "internal", "slot", "bucket.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	// BucketDoc{ 字面量只应出现在 BucketDocs() 一处（validate 复用该方法）。
	if n := strings.Count(body, "gate.BucketDoc{"); n != 1 {
		t.Errorf("★ gate.BucketDoc{ 字面量应恰好出现 1 次（BucketDocs()），实际 %d 次 —— "+
			"多份手抄的字段映射必然漂移", n)
	}
	if !strings.Contains(body, "docs := r.BucketDocs()") {
		t.Error("★ validate 应复用 r.BucketDocs()，而不是内联再抄一份字段映射")
	}
}
