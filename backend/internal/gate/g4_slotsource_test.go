// g4_slotsource_test.go —— G4 第六侧「槽的数据来源必须可解析」的判定测试。
//
// 本文件承担三件事（与前五侧同构）：
//  1. **负向**：种类未知 / ref 缺失 / ref 占位符 / derived 幽灵引用 ⇒ 必须报错；
//  2. **夹具自证**：夹具必须能区分「合法来源」与「违规来源」——
//     防止夹具太弱（本仓第四类缺陷：恒真型夹具）；
//  3. **接线**：判定函数必须有**非测试**生产调用点（本仓反复出现的病）。
package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ───────────────────── 正向：正常来源一律放行（防误伤） ─────────────────────

func TestSlotSource_AcceptsKnownKinds(t *testing.T) {
	docs := []SlotSourceDoc{
		{SlotID: "slot.a", Kind: "master_table", Ref: "product_master"},
		{SlotID: "slot.b", Kind: "api", Ref: "tiktok.finance.ads"},
		{SlotID: "slot.c", Kind: "mcp", Ref: "mcp.crm"},
		{SlotID: "slot.d", Kind: "skill", Ref: "skill.shopee"},
		{SlotID: "slot.e", Kind: "upload", Ref: "upload.2026-10"},
		// derived 且**不填** ref ⇒ 合法（上游产出，天然无外部引用）。
		{SlotID: "slot.f", Kind: "derived", Ref: ""},
		// derived 填了 ref 且**已核对存在** ⇒ 合法。
		{SlotID: "slot.g", Kind: "derived", Ref: "pnl_sku_month",
			DerivedChecked: true, DerivedResolved: true},
	}
	if v := CheckSlotSourceResolvable(docs); len(v) != 0 {
		t.Fatalf("合法来源不应报错，实际: %v", v)
	}
}

// ───────────────────── 负向①：source_kind 未知 / 缺失 ─────────────────────

func TestSlotSource_RejectsUnknownKind(t *testing.T) {
	cases := []struct {
		name string
		doc  SlotSourceDoc
	}{
		{"自由文本", SlotSourceDoc{SlotID: "slot.a", Kind: "随便写", Ref: "x"}},
		{"老口径 db", SlotSourceDoc{SlotID: "slot.b", Kind: "db", Ref: "pg://x"}},
		{"老口径 file", SlotSourceDoc{SlotID: "slot.c", Kind: "file", Ref: "s3://x"}},
		{"拼写错 table", SlotSourceDoc{SlotID: "slot.d", Kind: "table", Ref: "x"}},
		{"缺失", SlotSourceDoc{SlotID: "slot.e", Kind: "", Ref: "x"}},
		{"仅空白", SlotSourceDoc{SlotID: "slot.f", Kind: "   ", Ref: "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := CheckSlotSourceResolvable([]SlotSourceDoc{c.doc})
			if len(v) == 0 {
				t.Fatalf("source_kind=%q 应被拒", c.doc.Kind)
			}
			if !strings.Contains(v[0], "source_kind") {
				t.Errorf("原因应指向 source_kind，实际: %v", v)
			}
		})
	}
}

// ───────────────────── 负向②：外部来源缺 ref / ref 是占位符 ─────────────────────

func TestSlotSource_RejectsMissingOrPlaceholderRef(t *testing.T) {
	// 缺少 ref
	for _, kind := range []string{"api", "mcp", "skill", "master_table", "upload"} {
		t.Run("缺ref/"+kind, func(t *testing.T) {
			v := CheckSlotSourceResolvable([]SlotSourceDoc{{SlotID: "slot.a", Kind: kind}})
			if len(v) == 0 {
				t.Fatalf("kind=%s 缺 source_ref 应被拒", kind)
			}
			if !strings.Contains(v[0], "source_ref") {
				t.Errorf("原因应指向 source_ref，实际: %v", v)
			}
		})
	}

	// 占位符（**大小写/空格变体**都要拦 —— 这正是本仓 freshness 那轮的教训）
	for _, ref := range []string{"x", "X", " x ", "TODO", "tbd", "TBD", "TBA", "-", "?", "N/A", "n/a", "none", "unknown", "placeholder", "待定", "无"} {
		t.Run("占位符/"+ref, func(t *testing.T) {
			v := CheckSlotSourceResolvable([]SlotSourceDoc{{SlotID: "slot.a", Kind: "api", Ref: ref}})
			if len(v) == 0 {
				t.Fatalf("占位符 ref=%q 应被拒（看似填了、实则未填）", ref)
			}
		})
	}

	// 反向：**相似但合法**的 ref 不得被误伤（防过度拦击）。
	for _, ref := range []string{"x_api", "xray.ads", "tbd_report", "n/a-x", "todo_master", "xxx.api"} {
		t.Run("不误伤/"+ref, func(t *testing.T) {
			v := CheckSlotSourceResolvable([]SlotSourceDoc{{SlotID: "slot.a", Kind: "api", Ref: ref}})
			if len(v) != 0 {
				t.Fatalf("合法 ref=%q 被误伤: %v", ref, v)
			}
		})
	}
}

// ───────────────────── 负向③：derived 幽灵引用 / 未核对即拒判 ─────────────────────

func TestSlotSource_RejectsGhostDerivedRef(t *testing.T) {
	// 已核对但解析不到 ⇒ 幽灵引用（同 pnl_month 引幽灵算法那类缺陷）
	v := CheckSlotSourceResolvable([]SlotSourceDoc{{
		SlotID: "slot.gp", Kind: "derived", Ref: "pnl_sku_month_typo",
		DerivedChecked: true, DerivedResolved: false,
	}})
	if len(v) == 0 {
		t.Fatal("derived 的幽灵引用应被拒")
	}
	if !strings.Contains(v[0], "幽灵") {
		t.Errorf("原因应指出幽灵引用，实际: %v", v)
	}

	// **未核对**时不得放行（不假装核对过）——这是 fail-closed 的关键一条。
	v2 := CheckSlotSourceResolvable([]SlotSourceDoc{{
		SlotID: "slot.gp", Kind: "derived", Ref: "pnl_sku_month",
		DerivedChecked: false,
	}})
	if len(v2) == 0 {
		t.Fatal("derived 填了 ref 却未核对 ⇒ 必须拒判（不能假装核对过）")
	}
	if !strings.Contains(v2[0], "fail-closed") {
		t.Errorf("原因应说明 fail-closed，实际: %v", v2)
	}
}

// ───────────────────── 夹具自证：能区分合法与违规 ─────────────────────

// TestSlotSource_FixtureCanDistinguish 自证夹具的**区分能力** ——
// 防止「夹具太弱 ⇒ 一个错误的实现也能通过」（本仓第四类缺陷）。
func TestSlotSource_FixtureCanDistinguish(t *testing.T) {
	good := SlotSourceDoc{SlotID: "slot.a", Kind: "api", Ref: "tiktok.finance.ads"}
	goodV := CheckSlotSourceResolvable([]SlotSourceDoc{good})
	if len(goodV) != 0 {
		t.Fatalf("夹具的『合法』样例本身不合法: %v", goodV)
	}

	// 三种典型违规各自必须与「合法」可区分。
	badKinds := []SlotSourceDoc{
		{SlotID: "slot.a", Kind: "api2", Ref: "tiktok.finance.ads"}, // 种类错
		{SlotID: "slot.a", Kind: "api", Ref: ""},                    // ref 缺
		{SlotID: "slot.a", Kind: "api", Ref: "x"},                   // ref 占位
	}
	for i, b := range badKinds {
		v := CheckSlotSourceResolvable([]SlotSourceDoc{b})
		if len(v) == 0 {
			t.Fatalf("违规样例 %d 未被检出 ⇒ 夹具无法区分合法/违规", i)
		}
	}
}

// ───────────────────── 单一事实源：种类表与迁移/文档三处一致 ─────────────────────

// TestSlotSourceKind_SingleSourceOfTruth 钉住「闸门允许的 source_kind 集合」
// 与 **0005 迁移的 CHECK 约束**、**docs/02 M-COLLECT 表** 三处一致。
//
// ★ 为什么必须钉：本仓已验证过一次口径分叉的实害（`admin/plane.go` 注释曾写
//
//	`db | file`，而 0005 迁移里根本没有这两个值）—— 分叉会让「加载侧看着合法、
//	写进 DB 违反 CHECK」这种最难查的不一致长期存在。
func TestSlotSourceKind_SingleSourceOfTruth(t *testing.T) {
	root := repoRootOfGate(t)
	raw, err := os.ReadFile(filepath.Join(root, "sql", "migrations", "0005_collect_staging.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	// 迁移的枚举是采集任务的 source_kind（api/mcp/skill）——必须全部被闸门认可。
	for _, k := range []string{"api", "mcp", "skill"} {
		if !strings.Contains(sql, "'"+k+"'") {
			t.Fatalf("0005 迁移里找不到枚举值 %q（迁移是否被改过？）", k)
		}
		if !SlotSourceKinds()[k] {
			t.Errorf("★ 迁移允许 %q 但闸门不认 ⇒ 加载侧与 DB 约束分叉", k)
		}
	}
	// 反向：闸门不得允许 `db` / `file`（迁移的 CHECK 里没有，写了会违反约束）。
	for _, bad := range []string{"db", "file"} {
		if SlotSourceKinds()[bad] {
			t.Errorf("★ 闸门允许 %q，但 0005 迁移的 CHECK 约束里没有它 ⇒ 写库会失败", bad)
		}
	}
}

// ───────────────────── 接线：判定函数必须有非测试生产调用点 ─────────────────────

// TestWiring_SlotSourceGateHasProductionCallSite ★ 核心接线断言：
// `gate.CheckSlotSourceResolvable` 必须在**非测试**代码里被调用。
func TestWiring_SlotSourceGateHasProductionCallSite(t *testing.T) {
	root := repoRootOfGate(t)
	backend := filepath.Join(root, "backend")
	sites := scanCallSites(t, backend, "CheckSlotSourceResolvable(")
	if len(sites) == 0 {
		t.Fatal("★ gate.CheckSlotSourceResolvable 没有任何**非测试**调用点 ⇒ 该闸门恒真")
	}
	found := false
	for _, s := range sites {
		if strings.Contains(s, "internal/slot/slot.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("★ 期望生产调用点在 internal/slot/slot.go，实际: %v", sites)
	}
}

// TestWiring_SlotSourceRevalidatedAfterBuckets 钉住「derived 的 ref 在**桶到位后**
// 被重校验」——否则 `source_ref: 幽灵桶` 永远无人发现（加载顺序静默窗口）。
func TestWiring_SlotSourceRevalidatedAfterBuckets(t *testing.T) {
	root := repoRootOfGate(t)
	backend := filepath.Join(root, "backend")
	if sites := scanCallSites(t, backend, "ValidateWithBuckets("); len(sites) == 0 {
		t.Fatal("★ Registry.ValidateWithBuckets 没有生产调用点 ⇒ derived 来源校验形同虚设")
	}
	if sites := scanCallSites(t, backend, ".ResolveDerived("); len(sites) == 0 {
		t.Fatal("★ ResolveDerived 没有生产调用点 ⇒ derived ref 无解析路径")
	}
}

// TestWiring_SlotSourceScannerSelfProof 自证扫描器**真的**能检出「仅在测试里被调用」。
//
// ★ 若扫描器写错（例如把定义处也算调用），上面两条接线断言会变成恒真 ——
//
//	本仓已踩过这个坑，故必须自证。
func TestWiring_SlotSourceScannerSelfProof(t *testing.T) {
	root := repoRootOfGate(t)
	backend := filepath.Join(root, "backend")

	// 造一个临时包：函数只在 _test.go 里被调用。
	dir := t.TempDir()
	pkg := filepath.Join(dir, "probe")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGo(t, filepath.Join(pkg, "probe.go"), "package probe\n\nfunc OnlyTestCalled() int { return 1 }\n")
	writeGo(t, filepath.Join(pkg, "probe_test.go"), "package probe\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { _ = OnlyTestCalled() }\n")

	if sites := scanCallSites(t, dir, "OnlyTestCalled("); len(sites) != 0 {
		t.Fatalf("★ 扫描器未能检出『只在测试里被调用』⇒ 接线断言不可信，实际: %v", sites)
	}
	// 反向：真生产调用点必须被检出。
	writeGo(t, filepath.Join(pkg, "prod.go"), "package probe\n\nfunc Use() int { return OnlyTestCalled() }\n")
	if sites := scanCallSites(t, dir, "OnlyTestCalled("); len(sites) == 0 {
		t.Fatal("★ 扫描器漏检真实生产调用点")
	}

	// ★ 方法定义（`func (r *T) M(`）不得被算作调用点 ——
	//   早前只判包级函数定义，方法定义会被误当调用点 ⇒ 接线断言恒真。
	methodPkg := filepath.Join(dir, "probe2")
	if err := os.MkdirAll(methodPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGo(t, filepath.Join(methodPkg, "m.go"),
		"package probe2\n\ntype T struct{}\n\nfunc (t *T) MethodOnlyDeclared() int { return 1 }\n")
	if sites := scanCallSites(t, dir, "MethodOnlyDeclared("); len(sites) != 0 {
		t.Fatalf("★ 扫描器把**方法定义**误判为调用点 ⇒ 接线断言恒真，实际: %v", sites)
	}
	// 加一个真实调用后必须被检出。
	writeGo(t, filepath.Join(methodPkg, "use.go"),
		"package probe2\n\nfunc Use2(t *T) int { return t.MethodOnlyDeclared() }\n")
	if sites := scanCallSites(t, dir, "MethodOnlyDeclared("); len(sites) == 0 {
		t.Fatal("★ 扫描器漏检真实方法调用点")
	}
	_ = backend
}

// ───────────────────── 小工具 ─────────────────────

// repoRootOfGate 定位仓库根（backend/internal/gate → ../../..）。
func repoRootOfGate(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// scanCallSites 扫描 dir 下的**非测试** .go 文件，返回含 needle 的文件相对路径。
//
// 排除：定义处（`func <name>(` 与 `func (recv) <name>(`）、行首注释、`_test.go`。
func scanCallSites(t *testing.T, dir, needle string) []string {
	t.Helper()
	name := strings.TrimSuffix(needle, "(")
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
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
		rel, _ := filepath.Rel(dir, path)
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if isFuncDecl(trimmed, name) {
				continue // 定义处不算调用
			}
			if strings.Contains(line, needle) {
				out = append(out, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// isFuncDecl 报告该行是否为 name 的**函数/方法定义**。
//
// ★ 必须同时处理两种形态：
//
//	func CheckSlotSourceResolvable(...)   // 包级函数
//	func (r *Registry) ResolveDerived(...)  // 方法
//
// 早前只判第一种 ⇒ 方法定义被当成「生产调用点」⇒ 接线断言恒真
// （本仓已踩过「扫描器写错让断言恒真」的坑，故此处显式覆盖并自证）。
func isFuncDecl(trimmed, name string) bool {
	if !strings.HasPrefix(trimmed, "func ") {
		return false
	}
	rest := strings.TrimPrefix(trimmed, "func ")
	if strings.HasPrefix(rest, "(") {
		// 方法定义：跳过接收者括号后再看函数名。
		idx := strings.Index(rest, ")")
		if idx < 0 {
			return false
		}
		rest = strings.TrimLeft(rest[idx+1:], " \t")
	}
	return strings.HasPrefix(rest, name+"(")
}

// writeGo 写一个 Go 文件（测试夹具用）。
func writeGo(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ───────────────────── 附：writes_bucket 必须指向真实桶 ─────────────────────

func TestAlgoWritesBucket_AcceptsRegistered(t *testing.T) {
	reg := map[string]bool{"pnl_month": true, "pnl_sku_month": true}
	docs := []AlgoBucketDoc{
		{AlgoID: "algo.gp", WritesBucket: "pnl_month"},
		{AlgoID: "algo.a", WritesBucket: ""},                             // 不落桶 ⇒ 合法
		{AlgoID: "algo.b", WritesBucket: "pnl_month", Status: ""},        // 无状态 ⇒ 校验
		{AlgoID: "algo.c", WritesBucket: "ghost", Status: StatusPending}, // PENDING 跳过
	}
	if v := CheckAlgoWritesBucketRegistered(docs, reg); len(v) != 0 {
		t.Fatalf("合法样例不应报错: %v", v)
	}
}

func TestAlgoWritesBucket_RejectsGhost(t *testing.T) {
	reg := map[string]bool{"pnl_month": true}
	// ★ 复现本闸门上线首刻抓到的真缺陷：写 pnl_sku_month 而真桶是 pnl_month。
	v := CheckAlgoWritesBucketRegistered([]AlgoBucketDoc{
		{AlgoID: "algo.gp", WritesBucket: "pnl_sku_month"},
	}, reg)
	if len(v) == 0 {
		t.Fatal("幽灵桶 ID 必须被拒（否则 G6 对该桶恒不成立且零报错）")
	}
	if !strings.Contains(v[0], "writes_bucket") {
		t.Errorf("原因应指向 writes_bucket，实际: %v", v)
	}

	// 空注册表 ⇒ 一切非空声明都拒（fail-closed，防「没东西可查所以永远通过」）。
	if v := CheckAlgoWritesBucketRegistered([]AlgoBucketDoc{
		{AlgoID: "algo.gp", WritesBucket: "pnl_month"},
	}, map[string]bool{}); len(v) == 0 {
		t.Fatal("空桶注册表时非空 writes_bucket 必须被拒（fail-closed）")
	}
}

func TestAlgoWritesBucket_FixtureCanDistinguish(t *testing.T) {
	reg := map[string]bool{"pnl_month": true}
	good := []AlgoBucketDoc{{AlgoID: "algo.gp", WritesBucket: "pnl_month"}}
	if v := CheckAlgoWritesBucketRegistered(good, reg); len(v) != 0 {
		t.Fatalf("夹具的合法样例自身不通过: %v", v)
	}
	bad := []AlgoBucketDoc{{AlgoID: "algo.gp", WritesBucket: "pnl_sku_month"}}
	if v := CheckAlgoWritesBucketRegistered(bad, reg); len(v) == 0 {
		t.Fatal("夹具无法区分 pnl_month 与 pnl_sku_month ⇒ 断言太弱")
	}
}

// TestWiring_AlgoWritesBucketHasProductionCallSite 接线：该判定必须有非测试调用点。
func TestWiring_AlgoWritesBucketHasProductionCallSite(t *testing.T) {
	root := repoRootOfGate(t)
	sites := scanCallSites(t, filepath.Join(root, "backend"), "CheckAlgoWritesBucketRegistered(")
	if len(sites) == 0 {
		t.Fatal("★ CheckAlgoWritesBucketRegistered 没有非测试调用点 ⇒ 恒真")
	}
	found := false
	for _, s := range sites {
		if strings.Contains(s, "internal/slot/bucket.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("★ 期望生产调用点在 internal/slot/bucket.go，实际: %v", sites)
	}
}

// ───────────────────── 接线断言自身的「存在性」自证 ─────────────────────

// TestWiring_G4SlotSourceInventorySelfGuard 钉住本文件的**关键接线断言仍然存在**。
//
// 为什么需要这条：任何测试套件都无法从自身内部发现「某个测试被删掉了」——
// 把 TestWiring_SlotSourceScannerSelfProof 改名成 DISABLED_* 后，
// `go test ./internal/gate/` 依然会绿（本轮注入 H 实测）。
// 这条 inventory 断言的作用是把「接线断言被静默移除」变成**另一条测试的失败**：
// 它读本文件源码，要求关键断言函数名逐字存在。
//
// 诚实边界：它只能防「改名/误删本文件里的这些名字」，防不了「把整条断言连同本条
// 一起删」——那需要 CI 侧的测试清单校验，超出本文件能力范围。
func TestWiring_G4SlotSourceInventorySelfGuard(t *testing.T) {
	root := repoRootOfGate(t)
	raw, err := os.ReadFile(filepath.Join(root, "backend", "internal", "gate", "g4_slotsource_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	// ★ 只在**函数声明行**里找，且 needle 拆分构造 —— 否则本函数的 required 字面量
	//   会把 needle 自己造出来，使断言恒真（本仓反复踩的坑：断言测的是自己的常量）。
	declNames := map[string]bool{}
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "func Test") {
			declNames[trimmed] = true
		}
	}
	// 这些是本侧闸门的「接线 + 自证」脊梁，丢了它们闸门就会静默退化。
	required := []string{
		"TestWiring_SlotSourceGateHasProductionCallSite",
		"TestWiring_SlotSourceRevalidatedAfterBuckets",
		"TestWiring_SlotSourceScannerSelfProof",
		"TestWiring_AlgoWritesBucketHasProductionCallSite",
		"TestSlotSourceKind_SingleSourceOfTruth",
	}
	for _, name := range required {
		found := false
		for decl := range declNames {
			if strings.HasPrefix(decl, "func "+name+"(") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("★ 关键接线/自证断言缺失: %s ⇒ 本侧闸门的接线保证被静默削弱", name)
		}
	}
}
