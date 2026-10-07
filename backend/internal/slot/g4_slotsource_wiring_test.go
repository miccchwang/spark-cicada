// g4_slotsource_wiring_test.go —— G4 第六侧在 **slot 包（生产侧）** 的链路测试。
//
// 与 gate 包的 g4_slotsource_test.go 分工：
//
//	gate 侧   —— 判定函数**本身**的行为（负向、边界、夹具自证）；
//	slot 侧（本文件）—— 判定函数在**真实加载链路**上的行为：
//	                     `LoadRegistry` 读真 YAML → 过闸门；
//	                     `ValidateWithBuckets` 在桶到位后收口 derived 来源。
package slot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// ───────────────────── 真仓库：16 个槽的来源全部可解析 ─────────────────────

// TestSlotSource_RealRepoAllResolvable 钉住当前仓库事实：
// `slots/*.yaml` 的 source_kind 全部已知、source_ref 全部非占位符。
//
// ★ 这是本闸门的**首个真实调用点证据** —— 判定对象是磁盘上的真文件。
func TestSlotSource_RealRepoAllResolvable(t *testing.T) {
	slotsDir, algosDir := repoDirs(t)
	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatalf("加载真仓库槽定义失败（G4 第六侧违规？）：%v", err)
	}
	if n := len(r.Slots()); n == 0 {
		t.Fatal("槽注册表为空")
	}
	// 逐条自证：每个槽都必须有已知 kind。
	for _, s := range r.Slots() {
		if strings.TrimSpace(s.SourceKind) == "" {
			t.Errorf("槽 %s 无 source_kind", s.ID)
		}
	}
}

// TestSlotSource_RealRepoDerivedRefsResolve 钉住当前仓库事实：
// **每个** derived 槽的 source_ref 都必须解析到真实上游（事实表或物化桶）。
//
// ★ 本断言上线首刻即抓到真缺陷：`slot.gross_profit` 的 `source_ref: pnl_sku_month`
//
//	在 `buckets/` 下**并不存在**（真桶 ID 是 `pnl_month`）—— 幽灵引用，
//	与 `pnl_month.yaml` 引 4 个幽灵算法属同一类缺陷，此前无任何代码读过它。
func TestSlotSource_RealRepoDerivedRefsResolve(t *testing.T) {
	root := repoRoot(t)
	slotsDir := filepath.Join(root, "slots")
	algosDir := filepath.Join(root, "algorithms")
	bucketsDir := filepath.Join(root, "buckets")

	reg, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}
	buckets, err := slot.LoadBucketRegistry(bucketsDir, reg)
	if err != nil {
		t.Fatal(err)
	}
	// ★ 这是 derived 来源校验的**真实生产调用路径**（sparkd 用它收口加载顺序）。
	if err := reg.ValidateWithBuckets(buckets); err != nil {
		t.Fatalf("derived 来源校验失败（G4 第六侧）：%v", err)
	}

	// 至少有一个 derived 槽（否则本断言没测到东西 —— 防恒真）。
	nDerived, nChecked := 0, 0
	for _, s := range reg.Slots() {
		if strings.EqualFold(s.SourceKind, "derived") {
			nDerived++
			if strings.TrimSpace(s.SourceRef) != "" {
				nChecked++
				if !reg.ResolveDerived(s.SourceRef) {
					t.Errorf("槽 %s 的 derived source_ref=%q 未解析到真实上游", s.ID, s.SourceRef)
				}
			}
		}
	}
	if nDerived == 0 {
		t.Fatal("★ 仓库里没有任何 derived 槽 ⇒ 本断言无法测到 derived 解析路径（夹具失效）")
	}
	if nChecked == 0 {
		t.Fatal("★ 所有 derived 槽的 ref 都为空 ⇒ 解析路径从未被真正走到（夹具失效）")
	}
}

// ───────────────────── 负向：真加载链路上必须拦下违规 ─────────────────────

// TestSlotSource_RejectsUnknownKindInLoad 未知 source_kind 必须让**加载失败**。
func TestSlotSource_RejectsUnknownKindInLoad(t *testing.T) {
	for _, kind := range []string{"db", "file", "随便写", "table"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
				"id: slot.a\nname: A\nsource_kind: "+kind+"\nsource_ref: some.ref\n"+
					"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
			writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
				"id: algo.a\nname: A\nformula: a\nunit: THB\ndepends_on_slots: [slot.a]\n")

			_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
			if err == nil {
				t.Fatalf("source_kind=%q 应让加载失败", kind)
			}
			if !strings.Contains(err.Error(), "source_kind") {
				t.Errorf("错误信息应指向 source_kind，实际: %v", err)
			}
		})
	}
}

// TestSlotSource_RejectsPlaceholderRefInLoad 占位符 source_ref 必须让加载失败。
func TestSlotSource_RejectsPlaceholderRefInLoad(t *testing.T) {
	for _, ref := range []string{"x", "TODO", "tbd", "-"} {
		t.Run(ref, func(t *testing.T) {
			dir := t.TempDir()
			writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
				"id: slot.a\nname: A\nsource_kind: api\nsource_ref: \""+ref+"\"\n"+
					"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
			writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
				"id: algo.a\nname: A\nformula: a\nunit: THB\ndepends_on_slots: [slot.a]\n")

			_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
			if err == nil {
				t.Fatalf("占位符 source_ref=%q 应让加载失败（看似填了、实则未填）", ref)
			}
		})
	}
}

// TestSlotSource_RejectsMissingRefInLoad 外部来源缺 ref 必须让加载失败。
func TestSlotSource_RejectsMissingRefInLoad(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: api\n"+
			"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nformula: a\nunit: THB\ndepends_on_slots: [slot.a]\n")

	_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
	if err == nil {
		t.Fatal("api 槽缺 source_ref 应让加载失败")
	}
	if !strings.Contains(err.Error(), "source_ref") {
		t.Errorf("错误信息应指向 source_ref，实际: %v", err)
	}
}

// TestSlotSource_RejectsGhostDerivedRefAfterBuckets derived 槽引幽灵桶
// 必须在**桶到位后被拦下** —— 这是加载顺序静默窗口的收口断言。
func TestSlotSource_RejectsGhostDerivedRefAfterBuckets(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	writeYAML(t, slotsDir, "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: derived\nsource_ref: bucket.ghost\n"+
			"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nformula: a\nunit: THB\ndepends_on_slots: [slot.a]\nwrites_bucket: bucket.real\n")
	writeYAML(t, filepath.Join(dir, "buckets"), "bucket.real.yaml",
		"id: bucket.real\nname: 真桶\ngrain: [month]\nrefresh: monthly\n"+
			"produced_by: [algo.a]\nalgo_versions: {algo.a: 1}\nrule_versions: {}\nindexes: [[month]]\n")

	reg, err := slot.LoadRegistry(slotsDir, filepath.Join(dir, "algorithms"))
	if err != nil {
		t.Fatalf("derived 的 ref 在校验桶之前不应被拦（ref 可空是本轮设计）：%v", err)
	}
	buckets, err := slot.LoadBucketRegistry(filepath.Join(dir, "buckets"), reg)
	if err != nil {
		t.Fatal(err)
	}
	// ★ 幽灵桶：bucket.ghost 不在桶注册表里 ⇒ 必须被拦下。
	err = reg.ValidateWithBuckets(buckets)
	if err == nil {
		t.Fatal("derived 槽引幽灵桶（bucket.ghost）必须在校验后被拦下")
	}
	if !strings.Contains(err.Error(), "幽灵") {
		t.Errorf("错误信息应指出幽灵引用，实际: %v", err)
	}
}

// TestSlotSource_AcceptsResolvableDerivedRef 反向：derived 指向**真桶**必须放行。
func TestSlotSource_AcceptsResolvableDerivedRef(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	writeYAML(t, slotsDir, "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: derived\nsource_ref: bucket.real\n"+
			"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nformula: a\nunit: THB\ndepends_on_slots: [slot.a]\nwrites_bucket: bucket.real\n")
	writeYAML(t, filepath.Join(dir, "buckets"), "bucket.real.yaml",
		"id: bucket.real\nname: 真桶\ngrain: [month]\nrefresh: monthly\n"+
			"produced_by: [algo.a]\nalgo_versions: {algo.a: 1}\nrule_versions: {}\nindexes: [[month]]\n")

	reg, err := slot.LoadRegistry(slotsDir, filepath.Join(dir, "algorithms"))
	if err != nil {
		t.Fatal(err)
	}
	buckets, err := slot.LoadBucketRegistry(filepath.Join(dir, "buckets"), reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.ValidateWithBuckets(buckets); err != nil {
		t.Fatalf("derived 指向真桶应放行，实际: %v", err)
	}
}

// TestSlotSource_ValidateWithBucketsNilFailsClosed nil 桶注册表必须拒判（fail-closed）。
func TestSlotSource_ValidateWithBucketsNilFailsClosed(t *testing.T) {
	slotsDir, algosDir := repoDirs(t)
	reg, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.ValidateWithBuckets(nil); err == nil {
		t.Fatal("nil 桶注册表必须拒判（不能假装核对过 derived 来源）")
	}
}

// ───────────────────── 夹具自证 ─────────────────────

// TestSlotSource_LoadFixtureCanDistinguish 自证加载链路的夹具能区分合法/违规。
func TestSlotSource_LoadFixtureCanDistinguish(t *testing.T) {
	build := func(kind, ref string) (string, string, error) {
		dir := t.TempDir()
		writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
			"id: slot.a\nname: A\nsource_kind: "+kind+"\nsource_ref: "+ref+"\n"+
				"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
		writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
			"id: algo.a\nname: A\nformula: a\nunit: THB\ndepends_on_slots: [slot.a]\n")
		_, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"))
		return filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms"), err
	}

	if _, _, err := build("api", "tiktok.finance.ads"); err != nil {
		t.Fatalf("合法样例自身不通过 ⇒ 夹具无效：%v", err)
	}
	if _, _, err := build("api", "x"); err == nil {
		t.Fatal("占位符样例未被检出 ⇒ 夹具无法区分")
	}
	if _, _, err := build("db", "pg.x"); err == nil {
		t.Fatal("未知种类样例未被检出 ⇒ 夹具无法区分")
	}
}

// TestSlotSource_RealRepoKindCountSane 钉住真仓库来源种类的分布（防「全是同一个种类」）。
func TestSlotSource_RealRepoKindCountSane(t *testing.T) {
	slotsDir, algosDir := repoDirs(t)
	reg, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, s := range reg.Slots() {
		kinds[s.SourceKind]++
	}
	if len(kinds) < 2 {
		t.Fatalf("★ 真仓库槽的来源种类只有 %d 种 ⇒ 闸门几乎测不到种类判定：%v", len(kinds), kinds)
	}
	_ = os.Getenv
}

// ───────────────────── 接线（本包内，与 gate 包互为冗余覆盖） ─────────────────────

// TestWiring_SlotSourceGateCalledFromThisPackage ★ 在 **slot 包内**再钉一次
// 「`gate.CheckSlotSourceResolvable` 被本包的生产代码调用」。
//
// 为什么要在两个包各钉一次：gate 包的接线断言在 gate 包内运行
// （`go test ./internal/gate/...`），而**实现**在 slot 包。若只钉 gate 包，
// 摘掉 slot 包里的调用点时 `go test ./internal/slot/...` **不会变红**
// （本轮注入 F 实测正是这种「MISSED」——经复核不是闸门漏检，
//
//	而是接线断言与实现不在同一个测试目标里）。两个包各钉一次即可闭合。
func TestWiring_SlotSourceGateCalledFromThisPackage(t *testing.T) {
	root := repoRoot(t)
	backend := filepath.Join(root, "backend")
	sites := nonTestCallSitesIn(t, backend, "CheckSlotSourceResolvable")
	found := false
	for _, s := range sites {
		if strings.Contains(s, "internal/slot/slot.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("★ internal/slot/slot.go 未调用 gate.CheckSlotSourceResolvable ⇒ 该闸门在生产侧恒真，实际调用点: %v", sites)
	}
}

// TestWiring_ResolveDerivedCalledFromThisPackage 同理钉住 derived 解析路径。
func TestWiring_ResolveDerivedCalledFromThisPackage(t *testing.T) {
	root := repoRoot(t)
	backend := filepath.Join(root, "backend")
	sites := nonTestCallSitesIn(t, backend, "ResolveDerived")
	if len(sites) == 0 {
		t.Fatal("★ ResolveDerived 没有任何非测试调用点 ⇒ derived ref 解析路径不存在")
	}
}

// TestWiring_SlotSourceValidatorIsBoundInLoadRegistry ★ 闸门在**真实加载入口**
// 被调用：`LoadRegistry` → `Validate` → `CheckSlotSourceResolvable`。
//
// 这条防的是「函数有人调用，但加载入口那条路径被摘掉」——
// 例如把 `Validate()` 里的 append 行注释掉。本用例直接以行为断言：
// 一个含未知 source_kind 的槽目录**必须**让 `LoadRegistry` 失败。
func TestWiring_SlotSourceValidatorIsBoundInLoadRegistry(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "slots"), "slot.a.yaml",
		"id: slot.a\nname: A\nsource_kind: nonsense\nsource_ref: master.x\n"+
			"key_strategy: sku\ncoverage_gate: 0.9\nfreshness: 1d\npermission: L1\nstatus: AVAILABLE\n")
	writeYAML(t, filepath.Join(dir, "algorithms"), "algo.a.yaml",
		"id: algo.a\nname: A\nformula: a\nunit: THB\ndepends_on_slots: [slot.a]\n")
	if _, err := slot.LoadRegistry(filepath.Join(dir, "slots"), filepath.Join(dir, "algorithms")); err == nil {
		t.Fatal("★ LoadRegistry 未把槽来源校验接进校验链 ⇒ 闸门在真实入口失效")
	}
}

// nonTestCallSitesIn 扫描 backend 下非测试 .go 文件中 fn 的调用点（排除定义处）。
//
// ★ 必须同时排除方法定义 `func (r *T) fn(...)` —— 否则方法定义会被误当调用点
// （本仓已踩过「扫描器写错让断言恒真」的坑）。
func nonTestCallSitesIn(t *testing.T, backend, fn string) []string {
	t.Helper()
	var sites []string
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
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if isFuncDeclIn(trimmed, fn) {
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

// isFuncDeclIn 报告该行是否为 fn 的函数/方法定义。
func isFuncDeclIn(trimmed, fn string) bool {
	if !strings.HasPrefix(trimmed, "func ") {
		return false
	}
	rest := strings.TrimPrefix(trimmed, "func ")
	if strings.HasPrefix(rest, "(") {
		idx := strings.Index(rest, ")")
		if idx < 0 {
			return false
		}
		rest = strings.TrimLeft(rest[idx+1:], " \t")
	}
	return strings.HasPrefix(rest, fn+"(")
}
