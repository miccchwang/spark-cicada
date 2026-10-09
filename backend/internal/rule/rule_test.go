// rule_test.go —— ★ 链路级闸门：规则 → 槽/桶 引用完整性（G4 第三侧）+ 生产调用点钉住。
//
// 承袭本仓「定义文件全仓从没被任何代码读过」这个病（第十个变种）：
//
//	`rules/*.yaml` 是 docs/01 §0.3 明写的「费率/口径的**唯一事实源**」，
//	但在本包出现之前 **`grep "rules/"` 全仓唯一命中是 README 的一句话** ——
//	没有任何代码读过它。于是：
//	  * 该目录可被整体删除、可被写成任意内容，而 `LoadRegistry` /
//	    `LoadBucketRegistry` 依然全绿 ⇒ 「唯一事实源」在实现侧不成立；
//	  * 桶的 `rule_versions`（G6 规则漂移检测的输入）声明了 `rule.tk.fee`，
//	    但「规则改了 ⇒ 哪些桶要重算」的映射（`applies_to_buckets`）**没有来源**。
//
// 本文件从 `LoadRuleRegistry` **入口**进、以「注册表加载成功/失败」**出**，
// 而不是直接调 `gate.CheckRuleIDsRegistered` 看返回值。
package rule_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/rule"
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// repoRoot 返回仓库根（含 slots/ algorithms/ buckets/ rules/ sql/）。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// loadAll 加载槽/算法注册表 + 桶注册表 + 规则注册表（全部走真实生产入口）。
func loadAll(t *testing.T) (*slot.Registry, *slot.BucketRegistry, *rule.Registry) {
	t.Helper()
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(
		filepath.Join(root, "slots"),
		filepath.Join(root, "algorithms"),
	)
	if err != nil {
		t.Fatalf("槽/算法注册表加载失败: %v", err)
	}
	buckets, err := slot.LoadBucketRegistry(filepath.Join(root, "buckets"), reg)
	if err != nil {
		t.Fatalf("桶注册表加载失败: %v", err)
	}
	rules, err := rule.LoadRuleRegistry(
		filepath.Join(root, "rules"),
		reg.RegisteredSlotIDs(),
		buckets.RegisteredBucketIDs(),
	)
	if err != nil {
		t.Fatalf("规则注册表加载失败: %v", err)
	}
	return reg, buckets, rules
}

// writeTempRules 把给定 YAML 写入临时 rules 目录并加载（走真实入口：读盘 + 过真闸门）。
func writeTempRules(t *testing.T, reg *slot.Registry, buckets *slot.BucketRegistry,
	files map[string]string) (*rule.Registry, error) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return rule.LoadRuleRegistry(dir, reg.RegisteredSlotIDs(), buckets.RegisteredBucketIDs())
}

// ───────────────────── 正向：真仓库的规则定义必须自洽 ─────────────────────

// TestRuleRegistry_RealRepoLoads 真仓库的 rules/*.yaml 必须能加载成功。
func TestRuleRegistry_RealRepoLoads(t *testing.T) {
	_, _, rules := loadAll(t)

	ids := rules.IDs()
	if len(ids) == 0 {
		t.Fatal("★ 规则注册表为空 —— 空注册表会让引用完整性『因为没东西可查』永远通过")
	}
	if !contains(ids, "rule.tk.fee") {
		t.Fatalf("★ 期望 rule.tk.fee 存在（它是 P&L 平台费用的计提依据），实际 %v", ids)
	}
}

// TestRuleRegistry_RealRepoAffectsPnlMonth ★ 规则变更必须能定位到受影响桶。
//
// 这条断言防的正是「G6 规则漂移检测静默失效」：桶的 rule_versions 声明了
// rule.tk.fee，但若「规则 ⇒ 桶」映射为空，则规则升版本后没有任何桶会被重算。
func TestRuleRegistry_RealRepoAffectsPnlMonth(t *testing.T) {
	_, _, rules := loadAll(t)

	m := rules.ApplyToBuckets()
	if len(m) == 0 {
		t.Fatal("★ ApplyToBuckets 为空 —— 规则变更后无桶被标记 stale（G6 恒真）")
	}

	got := rules.AffectedBuckets("rule.tk.fee")
	if !contains(got, "pnl_month") {
		t.Errorf("★ 规则 rule.tk.fee 变更后应影响 pnl_month，实际 %v", got)
	}
	// 反向：改一条谁都不影响的规则，不应误伤任何桶。
	if x := rules.AffectedBuckets("rule.does.not.exist"); len(x) != 0 {
		t.Errorf("不存在的规则不应影响任何桶，实际 %v", x)
	}
}

// TestRuleRegistry_RealRepoSlotsAndBucketsRegistered 引用完整性（真文件）：
// applies_to_slots / applies_to_buckets 里的每个 ID 都必须真实注册。
func TestRuleRegistry_RealRepoSlotsAndBucketsRegistered(t *testing.T) {
	reg, buckets, rules := loadAll(t)

	for _, x := range rules.Rules() {
		for _, s := range x.AppliesToSlots {
			if _, ok := reg.Slot(s); !ok {
				t.Errorf("规则 %s 引用未注册槽 %q", x.ID, s)
			}
		}
		for _, b := range x.AppliesToBuckets {
			if _, ok := buckets.Bucket(b); !ok {
				t.Errorf("规则 %s 引用未注册桶 %q", x.ID, b)
			}
		}
	}
}

// TestRuleRegistry_RealRepoIsReadFromDisk ★ 规则注册表必须真的从磁盘读。
//
// 这条断言的价值：一旦有人把 rules/ 目录删空（「反正没人读」），本条与
// 加载期的「空目录报错」都会拦住。
func TestRuleRegistry_RealRepoIsReadFromDisk(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "rules"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			n++
		}
	}
	if n == 0 {
		t.Fatal("★ rules/ 目录下没有 YAML —— 规则集是费率唯一事实源，空目录必须让闸门变红")
	}
	_, _, rules := loadAll(t)
	if len(rules.Rules()) != n {
		t.Errorf("★ 加载到的规则数 %d != 磁盘上的 YAML 数 %d（有文件被静默跳过？）",
			len(rules.Rules()), n)
	}
}

// ───────────────────── 负向：闸门真的会拦 ─────────────────────

// TestRuleRegistry_RejectsGhostSlot 幽灵槽引用必须被拒。
func TestRuleRegistry_RejectsGhostSlot(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	_, err := writeTempRules(t, reg, buckets, map[string]string{
		"ghost.tk.yaml": `
id: rule.ghost.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.ghost]
applies_to_buckets: [pnl_month]
items:
  - id: fee
    name: 幽灵费
    rate: 0.01
`,
	})
	if err == nil {
		t.Fatal("★ 引用未注册槽 slot.ghost 应被拒（否则规则漂移后无法定位受影响槽）")
	}
	if !strings.Contains(err.Error(), "slot.ghost") {
		t.Errorf("错误信息应指出具体幽灵槽，实际: %v", err)
	}
}

// TestRuleRegistry_RejectsGhostBucket 幽灵桶引用必须被拒。
func TestRuleRegistry_RejectsGhostBucket(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	_, err := writeTempRules(t, reg, buckets, map[string]string{
		"ghost.tk.yaml": `
id: rule.ghost.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.revenue]
applies_to_buckets: [no_such_bucket]
items:
  - id: fee
    name: 幽灵费
    rate: 0.01
`,
	})
	if err == nil {
		t.Fatal("★ 引用未注册桶应被拒（否则规则改了没有桶会被重算）")
	}
	if !strings.Contains(err.Error(), "no_such_bucket") {
		t.Errorf("错误信息应指出具体幽灵桶，实际: %v", err)
	}
}

// TestRuleRegistry_RejectsFilenameIDMismatch 文件名与 id 的段集合分叉必须被拒。
//
// 人类按文件名找规则、程序按 id 找规则 —— 两侧对不上且都不报错，是最隐蔽的漂移。
func TestRuleRegistry_RejectsFilenameIDMismatch(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	_, err := writeTempRules(t, reg, buckets, map[string]string{
		// 文件名 cost.gate.yaml ⇒ 段 {cost, gate}；id 却写成 rule.tk.fee ⇒ 段 {tk, fee}
		"cost.gate.yaml": `
id: rule.tk.fee
version: 1
scope: {global: true}
applies_to_slots: [slot.revenue]
items:
  - id: fee
    name: 费
    rate: 0.01
`,
	})
	if err == nil {
		t.Fatal("★ 文件名与 id 段集合不一致应被拒（用文件名找不到 id、用 id 找不到文件）")
	}
	if !strings.Contains(err.Error(), "文件名") {
		t.Errorf("错误信息应说明文件名不匹配，实际: %v", err)
	}
}

// TestRuleRegistry_RejectsIllegalRate 非法费率必须被拒。
//
// ★ 费率是 P&L 的乘数：一个非法费率不会报错，只会把整张损益表算错一个系数。
func TestRuleRegistry_RejectsIllegalRate(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"费率>100%", `
id: rule.bad.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.revenue]
items:
  - id: fee
    name: 费
    rate: 1.5
`, "不在 (0,1]"},
		{"既无率也无定额", `
id: rule.bad.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.revenue]
items:
  - id: fee
    name: 费
`, "按 0 计提"},
		{"率与定额同时给出", `
id: rule.bad.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.revenue]
items:
  - id: fee
    name: 费
    rate: 0.01
    flat_per_order: 3
`, "重复计提"},
		{"items 为空", `
id: rule.bad.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.revenue]
items: []
`, "items 为空"},
		{"版本非正", `
id: rule.bad.tk
version: 0
scope: {platform: TK}
applies_to_slots: [slot.revenue]
items:
  - id: fee
    name: 费
    rate: 0.01
`, "非正数"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := writeTempRules(t, reg, buckets, map[string]string{"bad.tk.yaml": c.body})
			if err == nil {
				t.Fatalf("★ %s 应被拒，实际加载成功", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应含 %q，实际: %v", c.want, err)
			}
		})
	}
}

// TestRuleRegistry_RejectsEmptyDir 空目录必须报错（防「没东西可查 ⇒ 永远通过」）。
func TestRuleRegistry_RejectsEmptyDir(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	if _, err := writeTempRules(t, reg, buckets, map[string]string{}); err == nil {
		t.Fatal("★ 空规则目录必须报错 —— 否则删掉 rules/ 后所有引用完整性都『因为没东西可查』而通过")
	}
}

// TestRuleRegistry_NilRegistriesFailClosed nil 注册表必须 fail-closed。
func TestRuleRegistry_NilRegistriesFailClosed(t *testing.T) {
	root := repoRoot(t)
	if _, err := rule.LoadRuleRegistry(filepath.Join(root, "rules"), nil, nil); err == nil {
		t.Fatal("★ nil 槽/桶注册表必须报错 —— 否则引用完整性恒真")
	}
}

// ───────────────── 与迁移种子的一致性（不需要数据库就能拦的漂移）─────────────────

// TestRuleRegistry_SeedVersionMustMatch ★ 版本漂移必须被检出。
//
// 版本是「改了规则要重算哪些桶」的判定依据（G6）：YAML version=6 而库里是 2，
// 则桶永远认为「规则没变」⇒ 费率变了但桶不重算，报表口径错而无人知。
func TestRuleRegistry_SeedVersionMustMatch(t *testing.T) {
	_, _, rules := loadAll(t)

	// 真实种子（与库内容一致）：rule.tk.fee 版本 2，而 YAML 是 6。
	drift := rules.CompareSeed([]rule.SeedRule{
		{ID: "rule.tk.fee", Version: 2},
	})
	if len(drift) == 0 {
		t.Fatal("★ 种子版本 2 vs YAML 版本 6 必须被检出为漂移")
	}
	found := false
	for _, d := range drift {
		if strings.Contains(d, "版本漂移") && strings.Contains(d, "rule.tk.fee") {
			found = true
		}
	}
	if !found {
		t.Errorf("漂移描述应指出 rule.tk.fee 版本漂移，实际 %v", drift)
	}

	// 负向自证：版本一致时不得报漂移（防「永远报警」的假阳性）。
	rules2, err := rule.LoadRuleRegistry(filepath.Join(repoRoot(t), "rules"),
		mustSlots(t), mustBuckets(t))
	if err != nil {
		t.Fatal(err)
	}
	clean := rules2.CompareSeed([]rule.SeedRule{{ID: "rule.tk.fee", Version: 6}})
	for _, d := range clean {
		if strings.Contains(d, "版本漂移") {
			t.Errorf("版本一致时不应报漂移，实际 %v", d)
		}
	}
}

// TestRuleRegistry_SeedMissingRow 种子缺行必须被检出。
func TestRuleRegistry_SeedMissingRow(t *testing.T) {
	_, _, rules := loadAll(t)
	drift := rules.CompareSeed(nil)
	if len(drift) == 0 {
		t.Fatal("★ YAML 里有规则而种子完全没有对应行，必须被检出")
	}
}

// TestParseSeedItems_RealMigration 解析器必须能从真实迁移文件里解出规则。
//
// ★ 解析型闸门必须自带自测（docs/05 §3.3 的教训：一个「不报错但给出错误答案」
// 的解析器比直接崩溃的更危险 —— 前者会让闸门永远绿灯）。
func TestParseSeedItems_RealMigration(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "sql", "migrations", "0004_registry_seed.sql"))
	if err != nil {
		t.Fatal(err)
	}
	seeds := rule.ParseSeedItems(string(raw))
	if len(seeds) == 0 {
		t.Fatal("★ 从 0004 迁移里一条规则都没解析出来 —— 解析器失效（闸门会永远绿灯）")
	}
	byID := map[string]rule.SeedRule{}
	for _, s := range seeds {
		byID[s.ID] = s
	}
	tk, ok := byID["rule.tk.fee"]
	if !ok {
		t.Fatalf("★ 应解出 rule.tk.fee 的两个版本，实际 %v", seeds)
	}
	if tk.Version <= 0 {
		t.Errorf("rule.tk.fee 版本应 > 0，实际 %d", tk.Version)
	}
	// 自证：解析器对不含 registry_rule_set 的文本必须零命中（防「随便什么都算命中」）。
	if got := rule.ParseSeedItems("-- 无关文本\nSELECT 1;\n"); len(got) != 0 {
		t.Errorf("解析器对无关文本应零命中，实际 %v", got)
	}
}

// ───────────────────── 接线：判定函数必须真有生产调用点 ─────────────────────

// TestWiring_RuleGatesHaveProductionCallSite ★ 核心接线断言：
// gate 的三条规则判定函数必须在**非测试**代码里被调用。
//
// 这是本仓「判定函数没有生产调用点」这个病的**回归闸门**（第十个变种）。
func TestWiring_RuleGatesHaveProductionCallSite(t *testing.T) {
	root := repoRoot(t)
	backend := filepath.Join(root, "backend")

	fns := []string{
		"CheckRuleIDsRegistered",
		"CheckRuleIDMatchesFilename",
		"CheckPlatformFeeRule",
		// G4 第十五侧（第二十三个变种）：规则项「契约外键 + 生效期」聚合入口。
		// 它必须在 rule.Registry.Validate 里被调用 —— 否则「写进 YAML 却被
		// yaml 静默丢弃的键」照旧无人发现。
		"CheckRuleItems",
	}
	for _, fn := range fns {
		sites := nonTestCallSites(t, backend, fn)
		if len(sites) == 0 {
			t.Errorf("★ gate.%s 没有任何**非测试**调用点 ⇒ 该闸门恒真", fn)
			continue
		}
		found := false
		for _, s := range sites {
			if strings.Contains(s, "internal/rule/rule.go") {
				found = true
			}
		}
		if !found {
			t.Errorf("★ 期望 gate.%s 的生产调用点在 internal/rule/rule.go，实际 %v", fn, sites)
		}
	}
}

// TestWiring_RulePackageReadsRealFiles 规则包必须真的从磁盘读 YAML。
func TestWiring_RulePackageReadsRealFiles(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "backend", "internal", "rule", "rule.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{"os.ReadFile", "yaml.Unmarshal", "os.ReadDir", "LoadRuleRegistry"} {
		if !strings.Contains(src, want) {
			t.Errorf("rule.go 应包含 %q（真实读盘 + 真解析）", want)
		}
	}
}

// TestWiring_SparkdLoadsSpecs ★ 进程级接线：sparkd 启动时必须真的装载
// slots / algorithms / buckets / rules，并且把装载结果暴露到 /healthz。
//
// 为什么这条尤其重要：本仓的「假闸门」形态有十种变体，其中最常见的一种是
// 「判定函数只在测试里被调用」。仅仅在包内加一个 LoadXxx 还不够 ——
// **必须有进程在真实启动时调用它**，否则那些闸门依然只在 `go test` 里存在，
// 生产部署时一个都不会生效。
func TestWiring_SparkdLoadsSpecs(t *testing.T) {
	root := repoRoot(t)
	wiring, err := os.ReadFile(filepath.Join(root, "backend", "cmd", "sparkd", "wiring.go"))
	if err != nil {
		t.Fatal(err)
	}
	w := string(wiring)
	for _, want := range []string{
		"slot.LoadRegistry(",       // 数据槽 + 算法（G4 分离 / 引用完整性）
		"slot.LoadBucketRegistry(", // 桶 → 算法（G4 反向）
		"rule.LoadRuleRegistry(",   // 规则 → 槽/桶（G4 第三侧）
		"loadSpecs(",               // 启动期调用点本身
	} {
		if !strings.Contains(w, want) {
			t.Errorf("★ sparkd/wiring.go 应包含 %q —— 否则该闸门在生产部署时不存在", want)
		}
	}
	// 装载结果必须可观测（否则「静默降级」会伪装成部署正常 —— 本仓已有真实事故）。
	main, err := os.ReadFile(filepath.Join(root, "backend", "cmd", "sparkd", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"specIssues", "specLoaded"} {
		if !strings.Contains(string(main), want) {
			t.Errorf("★ /healthz 应暴露 %q（规格装载失败必须可观测，不得静默）", want)
		}
	}
}

// ───────────────── 桶 → 规则（G4 第三方向，实测抓出的真缺口）─────────────────

// TestBucketRuleVersions_RealRepoRegistered 真仓库的桶 rule_versions 引用必须真实注册。
//
// ★ 本条补的是一个**实测抓出的漏**：`rule_versions` 在
// `gate.CheckBucketRuleVersionsRegistered` 出现之前**没有任何关把点** ——
// `bucket.Validate` 只校验了版本号为正。于是往桶的 rule_versions 里塞一个
// 幽灵规则名不会让任何断言变红 ⇒ G6 的**规则**漂移检测对该桶恒不生效：
// 费率改了、桶不重算、报表口径错，而全程不报错。
func TestBucketRuleVersions_RealRepoRegistered(t *testing.T) {
	_, buckets, rules := loadAll(t)

	// 真实注册表里的桶必须先能过规则一侧的校验（sparkd 启动时也走这条路）。
	if err := buckets.ValidateWithRules(mustSlotsRegistry(t), rules.RegisteredRuleIDs()); err != nil {
		t.Fatalf("★ 真仓库的桶 rule_versions 引用校验失败: %v", err)
	}

	// 且 pnl_month 必须真的声明了 rule_versions（否则 G6 规则漂移无事可做）。
	b, ok := buckets.Bucket("pnl_month")
	if !ok {
		t.Fatal("期望 pnl_month 存在")
	}
	if len(b.RuleVersions) == 0 {
		t.Fatal("★ pnl_month 的 rule_versions 为空 —— 规则（费率）变更后该桶不会被重算")
	}
	// 规则侧版本必须与 rules/*.yaml 的事实源一致（防两处版本分叉）。
	if r, ok := rules.Rule("rule.tk.fee"); ok {
		if got := b.RuleVersions["rule.tk.fee"]; got != r.Version {
			t.Errorf("★ 桶记录的 rule.tk.fee 版本=%d，而规则事实源是 %d（G6 会误判规则未变更）",
				got, r.Version)
		}
	}
}

// TestBucketRuleVersions_RejectsGhostRule 幽灵规则名必须被拒（回归闸门）。
func TestBucketRuleVersions_RejectsGhostRule(t *testing.T) {
	reg, _, rules := loadAll(t)

	// 直接对一个含幽灵规则名的桶注册表走校验（临时目录 + 真实入口）。
	//
	// ★ 桶 ID 取 `pnl_month`（真桶 ID）：因为 `LoadBucketRegistry` 会按**算法注册表**
	//   校验 `writes_bucket` 引用完整性（G4 第六侧附）—— 真算法声明的是
	//   `writes_bucket: pnl_month`，夹具若用别的 ID 会先在这里被拦下，
	//   从而测不到本用例真正要测的 `rule_versions` 幽灵规则。
	dir := t.TempDir()
	body := `
id: pnl_month
grain: [month]
produced_by: [algo.gp]
algo_versions: {algo.gp: 3}
rule_versions: {rule.ghost.fee: 9}
refresh: monthly_incremental
`
	if err := os.WriteFile(filepath.Join(dir, "ghost_bucket.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	buckets, err := slot.LoadBucketRegistry(dir, reg)
	if err != nil {
		t.Fatalf("首次装载（未接规则）应通过：%v", err)
	}
	// ★ 关键：首次装载时 rules 尚不存在，故 rule_versions 未被校验 ——
	//   这正是「装载顺序造成的静默窗口」，必须由回填校验补上。
	if err := buckets.ValidateWithRules(reg, rules.RegisteredRuleIDs()); err == nil {
		t.Fatal("★ rule_versions 含幽灵规则 rule.ghost.fee 必须被拒")
	} else if !strings.Contains(err.Error(), "rule.ghost.fee") {
		t.Errorf("错误信息应指出具体幽灵规则，实际: %v", err)
	}
	// 反向自证：不接规则注册表时**允许**通过 —— 说明这条断言真的由回填校验提供，
	// 而不是别的什么在偶然拦截（防「恒真型夹具」）。
	if err := buckets.ValidateWithRules(reg, nil); err != nil {
		t.Errorf("不接规则注册表时应跳过规则一侧（便于分步装载），实际: %v", err)
	}
}

// TestWiring_BucketRuleVersionsGateHasProductionCallSite ★ 接线：
// `gate.CheckBucketRuleVersionsRegistered` 必须真有非测试调用点。
func TestWiring_BucketRuleVersionsGateHasProductionCallSite(t *testing.T) {
	root := repoRoot(t)
	backend := filepath.Join(root, "backend")
	sites := nonTestCallSites(t, backend, "CheckBucketRuleVersionsRegistered")
	if len(sites) == 0 {
		t.Fatal("★ gate.CheckBucketRuleVersionsRegistered 没有任何**非测试**调用点 ⇒ 该闸门恒真")
	}
	found := false
	for _, s := range sites {
		if strings.Contains(s, "internal/slot/bucket.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("★ 期望生产调用点在 internal/slot/bucket.go，实际 %v", sites)
	}
	// 且 sparkd 必须真的走回填校验（否则该调用点在生产部署时不会被执行）。
	wiring, err := os.ReadFile(filepath.Join(backend, "cmd", "sparkd", "wiring.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wiring), "ValidateWithRules(") {
		t.Error("★ sparkd 应调用 ValidateWithRules 回填校验 rule_versions（装载顺序造成的静默窗口）")
	}
}

// nonTestCallSites 扫描 backend 下所有非 _test.go，返回调用 fn( 的文件（相对路径）。
func nonTestCallSites(t *testing.T, backend, fn string) []string {
	t.Helper()
	var sites []string
	def := "func " + fn + "("
	err := filepath.WalkDir(backend, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(backend, path)
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, def) {
				continue
			}
			if strings.Contains(line, fn+"(") {
				sites = append(sites, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites
}

// ───────────────────── 自证：断言覆盖强度 ─────────────────────

// TestRuleGate_FixtureCanDistinguishRealAndGhost 自证夹具能区分真注册对象与幽灵对象 ——
// 防止夹具太弱（本仓第四类缺陷：恒真型夹具）。
func TestRuleGate_FixtureCanDistinguishRealAndGhost(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	if _, ok := reg.Slot("slot.platform_fee"); !ok {
		t.Fatal("夹具前提失败：slot.platform_fee 应存在")
	}
	if _, ok := buckets.Bucket("pnl_month"); !ok {
		t.Fatal("夹具前提失败：pnl_month 应存在")
	}
	// 负向自证：给闸门两个**必须被拒**的幽灵对象，确认它真的会响。
	if err := writeExpectErr(t, reg, buckets, "ghost_a.yaml", "rule.ghost.a",
		"slot.ghost"); err == nil {
		t.Error("幽灵槽未被拒 —— 夹具无效，本文件所有引用完整性断言都可能恒真")
	}
	if err := writeExpectErr(t, reg, buckets, "ghost_b.yaml", "rule.ghost.b",
		"bucket.ghost"); err == nil {
		t.Error("幽灵桶未被拒 —— 夹具无效")
	}
}

// TestRuleGate_RealFileCanDistinguishRateViolation 自证「费率为 0 的项」真的会被拦 ——
// 防止 CheckPlatformFeeRule 因为「0 与缺省无法区分」而恒放行。
func TestRuleGate_RealFileCanDistinguishRateViolation(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	body := fmt.Sprintf(`
id: rule.zero.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.platform_fee]
items:
  - id: free_ride
    name: 零费率项
    rate: 0
`)
	_, err := writeTempRules(t, reg, buckets, map[string]string{"zero.tk.yaml": body})
	if err == nil {
		t.Fatal("★ 显式 rate: 0 必须被拒（该项会静默按 0 计提）")
	}
	if !strings.Contains(err.Error(), "按 0 计提") {
		t.Errorf("错误信息应指出按 0 计提，实际: %v", err)
	}
}

func writeExpectErr(t *testing.T, reg *slot.Registry, buckets *slot.BucketRegistry,
	filename, id, ghost string) error {
	t.Helper()
	body := fmt.Sprintf(`
id: %s
version: 1
scope: {platform: TK}
applies_to_slots: [%s]
items:
  - id: fee
    name: 费
    rate: 0.01
`, id, ghost)
	_, err := writeTempRules(t, reg, buckets, map[string]string{filename: body})
	return err
}

func mustSlots(t *testing.T) map[string]bool {
	t.Helper()
	reg, _, _ := loadAll(t)
	return reg.RegisteredSlotIDs()
}

func mustSlotsRegistry(t *testing.T) *slot.Registry {
	t.Helper()
	reg, _, _ := loadAll(t)
	return reg
}

func mustBuckets(t *testing.T) map[string]bool {
	t.Helper()
	_, buckets, _ := loadAll(t)
	return buckets.RegisteredBucketIDs()
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ───────── G4 第十五侧（第二十三个变种）：规则项「契约外键 + 生效期」 ─────────
//
// 链路级（从 `LoadRuleRegistry` 入口进、以「加载成功/失败」出），而不是直接调
// gate 的判定函数看返回值 —— 只有这样才能证明「闸门真的接在生产入口上」。

// TestRuleItem_EffectiveToIsParsedNotDropped ★ 本侧的核心：`effective_to` 必须
// 真的被解析进结构体，而不是被 yaml **静默丢弃**。
//
// 修复前 `rule.Item` 没有 `EffectiveTo` 字段，yaml.v3 对未声明的键不报错 ⇒
// 作者按 docs/02 写的到期声明被悄悄丢掉、费率永不到期、全程零报错。
func TestRuleItem_EffectiveToIsParsedNotDropped(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	body := `
id: rule.window.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.platform_fee]
items:
  - id: promo_rate
    name: 促销费率
    rate: 0.02
    effective_from: 2026-07-01
    effective_to: 2026-12-31
`
	rules, err := writeTempRules(t, reg, buckets, map[string]string{"window.tk.yaml": body})
	if err != nil {
		t.Fatalf("合法的生效期不应被拒: %v", err)
	}
	x, ok := rules.Rule("rule.window.tk")
	if !ok {
		t.Fatal("规则 rule.window.tk 应已注册")
	}
	if len(x.Items) != 1 {
		t.Fatalf("应有 1 个明细项，实际 %d", len(x.Items))
	}
	it := x.Items[0]
	if it.EffectiveFrom != "2026-07-01" {
		t.Errorf("effective_from 未解析进结构体：%q", it.EffectiveFrom)
	}
	if it.EffectiveTo != "2026-12-31" {
		t.Errorf("★ effective_to 被静默丢弃了（结构体里是 %q）—— "+
			"该键会被 yaml 忽略，费率永不到期", it.EffectiveTo)
	}

	// ★ 第二段（实测注入 B 首轮 MISSED 后补）：必须同时钉住
	// `rule.Item → gate.RuleItem` 这一跳。若只钉结构体而漏了 Doc()，
	// 把 EffectiveTo 从 `doc()` 里删掉时测试照绿 —— 而闸门读的正是
	// `gate.RuleDoc.Items`，于是「上界」在闸门侧永远为空、空窗检查静默失效。
	doc, ok := rules.Doc("rule.window.tk")
	if !ok {
		t.Fatal("Doc(rule.window.tk) 应存在")
	}
	if len(doc.Items) != 1 {
		t.Fatalf("gate.RuleDoc 应有 1 个明细项，实际 %d", len(doc.Items))
	}
	if doc.Items[0].EffectiveTo != "2026-12-31" {
		t.Errorf("★ rule.Item → gate.RuleItem 的 effective_to 映射断了（闸门侧拿到 %q）—— "+
			"窗口校验读的是 gate.RuleDoc.Items，该跳丢失会让上界永远为空",
			doc.Items[0].EffectiveTo)
	}
	if doc.Items[0].EffectiveFrom != "2026-07-01" {
		t.Errorf("rule.Item → gate.RuleItem 的 effective_from 映射断了（闸门侧拿到 %q）",
			doc.Items[0].EffectiveFrom)
	}
}

// TestRuleItem_UnknownKeyRejectsLoad 契约外的键 ⇒ 加载即失败（不是静默丢弃）。
func TestRuleItem_UnknownKeyRejectsLoad(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	cases := []struct{ name, key, want string }{
		{"拼写错误", "effective_form", "effective_form"},
		{"未建模字段", "unit", "unit"},
		{"多余字段", "vat_include", "vat_include"},
	}
	for _, c := range cases {
		body := fmt.Sprintf(`
id: rule.typo.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.platform_fee]
items:
  - id: fee
    name: 费
    rate: 0.01
    %s: 2026-07-01
`, c.key)
		_, err := writeTempRules(t, reg, buckets, map[string]string{"typo.tk.yaml": body})
		if err == nil {
			t.Errorf("%s：含契约外的键 %q 必须被拒（否则该键会被 yaml 静默丢弃）", c.name, c.key)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息应点名键 %q，实际: %v", c.name, c.want, err)
		}
	}
}

// TestRuleItem_WindowViolationsRejectLoad 生效期的形态与次序违规必须被拒。
func TestRuleItem_WindowViolationsRejectLoad(t *testing.T) {
	reg, buckets, _ := loadAll(t)
	cases := []struct{ name, from, to, want string }{
		{"非零填充", "2026-7-1", "", "YYYY-MM-DD"},
		{"空窗（from==to）", "2026-06-01", "2026-06-01", "空窗"},
		{"空窗（from>to）", "2026-07-01", "2026-06-01", "空窗"},
	}
	for _, c := range cases {
		body := fmt.Sprintf(`
id: rule.badwin.tk
version: 1
scope: {platform: TK}
applies_to_slots: [slot.platform_fee]
items:
  - id: fee
    name: 费
    rate: 0.01
    effective_from: %q
    effective_to: %q
`, c.from, c.to)
		_, err := writeTempRules(t, reg, buckets, map[string]string{"badwin.tk.yaml": body})
		if err == nil {
			t.Errorf("%s：应被拒，实际加载成功", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息应含 %q，实际: %v", c.name, c.want, err)
		}
	}
}

// TestRuleItem_RealYamlWindowsFlowIntoStruct 真实 `rules/*.yaml` 的生效期
// 必须真的流进结构体（证明该字段在**真数据**上不是死的）。
//
// `rules/platform_fee.tk.yaml` 的 platform_commission / commerce_growth_fee
// 都带 `effective_from`；若解析链路断了，这里会变红。
func TestRuleItem_RealYamlWindowsFlowIntoStruct(t *testing.T) {
	_, _, rules := loadAll(t)
	x, ok := rules.Rule("rule.tk.fee")
	if !ok {
		t.Fatal("rule.tk.fee 应存在")
	}
	var withFrom, withTo int
	for _, it := range x.Items {
		if it.EffectiveFrom != "" {
			withFrom++
		}
		if it.EffectiveTo != "" {
			withTo++
		}
	}
	if withFrom == 0 {
		t.Error("★ rule.tk.fee 的 items 里应至少有一项带 effective_from（真数据），" +
			"实际一个都没解析出来 —— 解析链路断了")
	}
	_ = withTo // 当前真数据没有上界，不强制；本断言只钉「下界真的流进来了」
}

// TestRuleItem_UnknownKeyGateHasProductionCallSite ★ 接线：契约外键闸门必须
// 在**非测试**代码里被调用（`rule.Registry.Validate`）。
//
// 这是本仓「判定函数没有生产调用点 ⇒ 闸门恒真」这个病的**回归闸门**。
func TestRuleItem_UnknownKeyGateHasProductionCallSite(t *testing.T) {
	root := repoRoot(t)
	backend := filepath.Join(root, "backend")
	for _, fn := range []string{"CheckRuleItemUnknownKeys", "CheckRuleItemWindowFields", "RuleItemEffective"} {
		sites := nonTestCallSites(t, backend, fn)
		if len(sites) == 0 {
			t.Errorf("★ gate.%s 没有任何**非测试**调用点 ⇒ 该闸门恒真", fn)
			continue
		}
		found := false
		for _, s := range sites {
			// RuleItemEffective 由同包（gate）的窗口校验调用；其余两个由 rule 包调用。
			if strings.Contains(s, "internal/rule/rule.go") ||
				strings.Contains(s, "internal/gate/g4_effectivity.go") {
				found = true
			}
		}
		if !found {
			t.Errorf("★ 期望 gate.%s 的生产调用点在 rule.go / g4_effectivity.go，实际 %v", fn, sites)
		}
	}
}
