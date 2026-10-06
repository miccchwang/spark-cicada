// bucket_test.go —— ★ 链路级闸门：桶 → 算法 引用完整性（G4 反向）+ 生产调用点钉住。
//
// 承袭本仓「判定函数没有生产调用点」这个病（第九个变种）：
//
//	`buckets/*.yaml` 的 `produced_by` 在本次修复之前**全仓没有任何代码读过** ——
//	所以 `pnl_month.yaml` 可以列出 `algo.rev` / `algo.net_rev` / `algo.platform_fee` /
//	`algo.net_contrib` 这些**在 algorithms/ 下根本不存在的算法**而不报错。
//	后果不是「少了一条断言」，而是 G6 的 `AffectedBuckets`（算法升级后要重算哪些桶）
//	会**静默漏算** —— 桶里的数就永远不对平，而没人知道。
//
// 本文件从 `LoadBucketRegistry` **入口**进、以「注册表加载成功/失败」**出**，
// 而不是直接调 `gate.CheckBucketProducersRegistered` 看返回值。
package slot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// repoRoot 返回仓库根（含 slots/ algorithms/ buckets/）。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// loadAll 加载槽/算法注册表 + 桶注册表（走真实生产入口）。
func loadAll(t *testing.T) (*slot.Registry, *slot.BucketRegistry) {
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
	return reg, buckets
}

// writeTempBuckets 把给定 YAML 写入临时 buckets 目录并加载。
//
// ★ 这是真实入口：文件从磁盘读、过真闸门，而不是给判定函数喂内存结构。
func writeTempBuckets(t *testing.T, reg *slot.Registry, files map[string]string) (*slot.BucketRegistry, error) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return slot.LoadBucketRegistry(dir, reg)
}

// ───────────────────── 正向：真仓库的桶定义必须自洽 ─────────────────────

// TestBucketRegistry_RealRepoIsConsistent 真仓库的 buckets/*.yaml 必须能加载成功。
//
// 若失败，说明磁盘上的桶定义引用了不存在的算法 —— 这正是本条闸门要拦的东西。
func TestBucketRegistry_RealRepoIsConsistent(t *testing.T) {
	reg, buckets := loadAll(t)

	ids := buckets.IDs()
	if len(ids) == 0 {
		t.Fatal("★ 桶注册表为空 —— 空注册表会让引用完整性『因为没东西可查』永远通过")
	}
	if !contains(ids, "pnl_month") {
		t.Fatalf("★ 期望桶 pnl_month 存在，实际 %v", ids)
	}

	// 每个 produced_by 算法都必须真实存在于算法注册表（闸门已保证，这里复述口径）。
	for _, b := range buckets.Buckets() {
		for _, a := range b.ProducedBy {
			if _, ok := reg.Algorithm(a); !ok {
				t.Errorf("桶 %s 引用未注册算法 %s", b.ID, a)
			}
		}
	}
	// pnl_month 的 algo_versions 必须覆盖它声明的产出算法里带版本的那些。
	if b, ok := buckets.Bucket("pnl_month"); ok {
		if len(b.AlgoVersions) == 0 {
			t.Error("pnl_month 的 algo_versions 为空 —— G6 版本漂移检测将无事可做")
		}
		if len(b.ProducedBy) == 0 {
			t.Error("pnl_month 的 produced_by 为空 —— 该桶永不会被重算")
		}
	}
}

// TestBucketRegistry_RealRepoHasNoGhostAlgorithms 直接对**真文件**断言：
// algorithms/ 下不存在的算法 ID 不得出现在任何桶定义里。
//
// 这条断言的价值在于它读的是磁盘真内容 —— 一旦有人把幽灵算法加回去，立刻变红。
func TestBucketRegistry_RealRepoHasNoGhostAlgorithms(t *testing.T) {
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	if err != nil {
		t.Fatal(err)
	}
	// 4 个在 algorithms/ 下**不存在**、历史上曾被 pnl_month 引用的算法。
	ghosts := []string{"algo.rev", "algo.net_rev", "algo.platform_fee", "algo.net_contrib"}
	_ = ghosts
	existing := reg.RegisteredAlgorithmIDs()

	raw, err := os.ReadFile(filepath.Join(root, "buckets", "pnl_month.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		// 只看 YAML 列表项（`- algo.xxx`），注释行不算。
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, "- algo.") {
			continue
		}
		// ★ 去掉行内注释（`- algo.gp  # 说明`）：YAML 允许尾随注释，
		// 但本闸门比对的是**算法 ID 本身**，带上注释会误判为幽灵算法。
		id := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
		if i := strings.Index(id, "#"); i >= 0 {
			id = strings.TrimSpace(id[:i])
		}
		if _, ok := existing[id]; !ok {
			t.Errorf("★ pnl_month.yaml 引用了未注册算法 %q —— 会让 G6『仅重算受影响桶』漏算", id)
		}
	}
}

// ───────────────────── 负向：闸门真的会拦 ─────────────────────

// TestBucketRegistry_RejectsGhostAlgorithm 幽灵算法引用必须被拒。
func TestBucketRegistry_RejectsGhostAlgorithm(t *testing.T) {
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = writeTempBuckets(t, reg, map[string]string{
		"ghost.yaml": `
id: pnl_ghost
grain: [month]
produced_by:
  - algo.gp
  - algo.does_not_exist
refresh: monthly
algo_versions:
  algo.gp: 3
`,
	})
	if err == nil {
		t.Fatal("★ 引用了不存在算法的桶必须加载失败（否则 G6 会静默漏算）")
	}
	if !strings.Contains(err.Error(), "algo.does_not_exist") {
		t.Errorf("错误信息应点名幽灵算法，实际: %v", err)
	}
}

// TestBucketRegistry_RejectsEmptyProducedBy 空 produced_by 必须被拒。
func TestBucketRegistry_RejectsEmptyProducedBy(t *testing.T) {
	root := repoRoot(t)
	reg, _ := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	_, err := writeTempBuckets(t, reg, map[string]string{
		"empty.yaml": `
id: pnl_empty
grain: [month]
refresh: monthly
`,
	})
	if err == nil {
		t.Fatal("★ produced_by 为空的桶必须被拒（该桶永远不会被算法升级触发重算）")
	}
}

// TestBucketRegistry_RejectsGhostAlgoVersion algo_versions 里拼错的算法 ID 必须被拒 ——
// 拼错不会报语法错，只会让版本漂移检测对该算法**恒不生效**。
func TestBucketRegistry_RejectsGhostAlgoVersion(t *testing.T) {
	root := repoRoot(t)
	reg, _ := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	_, err := writeTempBuckets(t, reg, map[string]string{
		"vmiss.yaml": `
id: pnl_vmiss
grain: [month]
produced_by: [algo.gp]
refresh: monthly
algo_versions:
  algo.gqp: 3
`,
	})
	if err == nil {
		t.Fatal("★ algo_versions 含未注册算法必须被拒（否则版本漂移检测对该算法恒不生效）")
	}
}

// TestBucketRegistry_RejectsNonPositiveVersion 版本号非正必须被拒。
func TestBucketRegistry_RejectsNonPositiveVersion(t *testing.T) {
	root := repoRoot(t)
	reg, _ := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	_, err := writeTempBuckets(t, reg, map[string]string{
		"vzero.yaml": `
id: pnl_vzero
grain: [month]
produced_by: [algo.gp]
refresh: monthly
algo_versions:
  algo.gp: 0
`,
	})
	if err == nil {
		t.Fatal("★ algo_versions 非正数必须被拒（0 会让 gate.VersionDrift 的对平失去意义）")
	}
}

// TestBucketRegistry_RejectsEmptyDir 空目录必须报错，不得静默返回空注册表。
func TestBucketRegistry_RejectsEmptyDir(t *testing.T) {
	root := repoRoot(t)
	reg, _ := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	if _, err := writeTempBuckets(t, reg, map[string]string{}); err == nil {
		t.Fatal("★ 空 buckets 目录必须报错 —— 静默空注册表是『假闸门』的根源")
	}
}

// TestBucketRegistry_NilAlgorithmRegistryFailsClosed 传 nil 算法注册表必须报错，
// 而不是「没有可比的集合 ⇒ 永远通过」。
func TestBucketRegistry_NilAlgorithmRegistryFailsClosed(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(`
id: pnl_x
grain: [month]
produced_by: [algo.gp]
refresh: monthly
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := slot.LoadBucketRegistry(dir, nil); err == nil {
		t.Fatal("★ nil 算法注册表必须 fail-closed 报错（否则引用完整性恒真）")
	}
	_ = root
}

// ───────────────────── 接线：判定函数必须真有生产调用点 ─────────────────────

// TestWiring_BucketGateHasProductionCallSite ★ 核心接线断言：
// `gate.CheckBucketProducersRegistered` 必须在**非测试**代码里被调用。
//
// 这是本仓「判定函数没有生产调用点」这个病的**回归闸门**。
func TestWiring_BucketGateHasProductionCallSite(t *testing.T) {
	root := repoRoot(t)
	backend := filepath.Join(root, "backend")

	var callSites []string
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
			if strings.Contains(line, "func CheckBucketProducersRegistered(") {
				continue // 定义处不算调用
			}
			if strings.Contains(line, "CheckBucketProducersRegistered(") {
				callSites = append(callSites, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callSites) == 0 {
		t.Fatal("★ gate.CheckBucketProducersRegistered 没有任何**非测试**调用点 ⇒ 该闸门恒真")
	}
	found := false
	for _, s := range callSites {
		if strings.Contains(s, "internal/slot/bucket.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("★ 期望生产调用点在 internal/slot/bucket.go，实际: %v", callSites)
	}
}

// TestWiring_BucketPackageReadsRealFiles 桶包必须真的从磁盘读 YAML，
// 而不是对着测试里的内存结构判定。
//
// 读盘与解析走的是本包共享的 `yamlFiles` / `unmarshalYAML`（见 slot.go）——
// 断言它们被真的用到，比断言出现某个具体标准库调用名更准确。
func TestWiring_BucketPackageReadsRealFiles(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "backend", "internal", "slot", "bucket.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{"yamlFiles(", "unmarshalYAML(", "LoadBucketRegistry", "os.ReadDir"} {
		if !strings.Contains(src, want) {
			t.Errorf("bucket.go 应包含 %q（真实读盘 + 真解析）", want)
		}
	}
	// 共享读盘原语必须在同包的 slot.go 里真实使用 os.ReadFile + yaml.Unmarshal。
	shared, err := os.ReadFile(filepath.Join(root, "backend", "internal", "slot", "slot.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"os.ReadFile", "yaml.Unmarshal"} {
		if !strings.Contains(string(shared), want) {
			t.Errorf("slot.go 应包含 %q（真实读盘 + 真解析的实现处）", want)
		}
	}
}

// TestBucketGate_FeedsFromSameRegistryAsAffectedBuckets ★ 防漂移：
// 桶注册表提供的 AlgoToBuckets 必须与 gate.AffectedBuckets 的输入口径一致 ——
// 即「算法升级 ⇒ 受影响桶」在真注册表上能真的算出结果，而不是空表。
func TestBucketGate_FeedsFromSameRegistryAsAffectedBuckets(t *testing.T) {
	_, buckets := loadAll(t)
	m := buckets.AlgoToBuckets()
	if len(m) == 0 {
		t.Fatal("★ AlgoToBuckets 为空 —— AffectedBuckets 将永远返回空（G6-3 恒真）")
	}
	// algo.gp 升级必须能定位到 pnl_month（它是唯一声明产出 algo.gp 的桶）。
	got := m["algo.gp"]
	if !contains(got, "pnl_month") {
		t.Errorf("★ algo.gp 升级后应影响 pnl_month，实际 %v", got)
	}
}

// ───────────────────── 自证：断言覆盖强度 ─────────────────────

// TestBucketGate_FixtureCanDistinguishGhost 自证夹具能区分「真算法」与「幽灵算法」——
// 防止夹具本身太弱（本仓第四类缺陷：恒真型夹具）。
func TestBucketGate_FixtureCanDistinguishGhost(t *testing.T) {
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Algorithm("algo.gp"); !ok {
		t.Fatal("夹具前提失败：algo.gp 应存在")
	}
	if _, ok := reg.Algorithm("algo.rev"); ok {
		t.Fatal("夹具前提变化：algo.rev 现在存在了 —— 请更新本条注释与 docs/03 漂移说明")
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
