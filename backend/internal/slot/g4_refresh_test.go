// g4_refresh_test.go —— ★ 链路级闸门：桶的刷新节律（refresh）从「不影响任何事」到真判定。
//
// 承袭本仓「假闸门」形态在桶侧的实例（第十三个变种：**字段被读进来、写进数据库，
// 却没有任何判定消费它**）：
//
//	`Bucket.Refresh` 的链路是
//	  buckets/*.yaml → Bucket.Refresh → gate.BucketDoc.Refresh → registry_bucket.refresh 列 → 结束。
//	唯一对它的校验是 `strings.TrimSpace(...) == ""`（只查非空），此外全仓无人读它：
//	  * 没有解析器 ⇒ `monthly_incremental` 只是一个裸字符串；
//	  * 没有到期判定 ⇒ 「这个桶该刷了吗」无从回答；
//	  * 没有与 docs/04 §3.1 刷新任务表的对齐校验。
//
//	后果与 freshness 完全同型：`refresh: monthly_incremental` 与
//	`refresh: 想写什么写什么` 行为**完全等价**。最贵的代价是**静默的陈旧** ——
//	桶永远不会被判「该刷了」，没人知道 pnl_month 已经三个月没重算。
//
// 本文件从 `LoadBucketRegistry` **入口**进（真读磁盘 YAML）、以「加载成功/失败」**出**，
// 而不是直接给判定函数喂内存结构。
package slot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// ───────────────────── 正向：真仓库的刷新声明必须可解析 ─────────────────────

// TestRefreshGate_RealRepoDeclaresKnownModes 真仓库每个桶都必须声明**已知**刷新节律。
func TestRefreshGate_RealRepoDeclaresKnownModes(t *testing.T) {
	_, buckets := loadAll(t)
	all := buckets.Buckets()
	if len(all) == 0 {
		t.Fatal("★ 桶注册表为空 —— 空注册表会让本闸门『因为没东西可查』永远通过")
	}
	for _, b := range all {
		if _, ok := gate.ParseRefresh(b.Refresh); !ok {
			t.Errorf("★ 桶 %s 的 refresh=%q 不是已知节律 —— 刷新到期判定对它恒不生效", b.ID, b.Refresh)
		}
	}
}

// TestRefreshGate_RealRepoGivesPnlMonthAnInterval pnl_month 必须带**固定节律**
// （docs/04 §3.1：每日 + 月终结转），否则它永远不会被判到期。
func TestRefreshGate_RealRepoGivesPnlMonthAnInterval(t *testing.T) {
	_, buckets := loadAll(t)
	b, ok := buckets.Bucket("pnl_month")
	if !ok {
		t.Fatal("夹具前提失败：期望桶 pnl_month 存在")
	}
	if !gate.RefreshHasInterval(b.Refresh) {
		t.Fatalf("★ pnl_month 的 refresh=%q 不带固定节律 ⇒ 永远不会被判到期（docs/04 §3.1 要求每日+月终结转）", b.Refresh)
	}
	if got := b.RefreshIntervalMinutes(); got <= 0 {
		t.Fatalf("★ pnl_month 的刷新上限应为正数，实际 %d", got)
	}
}

// TestRefreshGate_ModesMatchDocsRequirement 契约钉住：
// docs/04 §3.1 用到的说法（daily / monthly_incremental）必须都在允许集合里。
//
// 这是防「文档写了某节律、解析器不认」的两侧漂移。
func TestRefreshGate_ModesMatchDocsRequirement(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docs", "04-运维方案.md"))
	if err != nil {
		t.Fatal(err)
	}
	modes := gate.RefreshModes()
	// docs/04 §3.1 表格里出现的桶频率说法。
	for _, want := range []string{"daily", "monthly_incremental"} {
		if !modes[want] {
			t.Errorf("★ 允许节律集合缺少 %q —— docs/04 §3.1 用到它，会导致真仓库定义加载失败", want)
		}
	}
	if !strings.Contains(string(raw), "monthly_incremental") {
		t.Error("docs/04 §3.1 应写明 monthly_incremental（本断言防两侧漂移）")
	}
}

// ───────────────────── 负向：闸门真的会拦 ─────────────────────

// TestRefreshGate_RejectsFreeText free text 刷新声明必须被拒 ——
// 这是本条闸门的核心价值：把「看起来合理但解析不了」的写法挡在门外。
func TestRefreshGate_RejectsFreeText(t *testing.T) {
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"weekly-ish", "每天", "1 day", "月终结转时", "sometimes"} {
		_, err := writeTempBuckets(t, reg, map[string]string{
			"b.yaml": "id: pnl_x\ngrain: [month]\nproduced_by: [algo.gp]\nrefresh: \"" + bad + "\"\n",
		})
		if err == nil {
			t.Errorf("★ refresh=%q 必须加载失败（自由文本让到期判定退化为『不影响任何事』）", bad)
			continue
		}
		if !strings.Contains(err.Error(), "refresh") {
			t.Errorf("refresh=%q 的报错应点名 refresh 字段，实际: %v", bad, err)
		}
	}
}

// TestRefreshGate_RejectsMissing 缺 refresh 必须被拒（无判据 ⇒ 桶可能无限期陈旧）。
func TestRefreshGate_RejectsMissing(t *testing.T) {
	root := repoRoot(t)
	reg, _ := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	_, err := writeTempBuckets(t, reg, map[string]string{
		"b.yaml": "id: pnl_x\ngrain: [month]\nproduced_by: [algo.gp]\n",
	})
	if err == nil {
		t.Fatal("★ 缺 refresh 的桶必须被拒（无刷新判据 ⇒ 静默陈旧）")
	}
}

// TestRefreshGate_RejectsWhitespaceOnly 仅空白的 refresh 必须被拒
// （不能靠 TrimSpace 之外的空字符串骗过非空校验）。
func TestRefreshGate_RejectsWhitespaceOnly(t *testing.T) {
	root := repoRoot(t)
	reg, _ := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	_, err := writeTempBuckets(t, reg, map[string]string{
		"b.yaml": "id: pnl_x\ngrain: [month]\nproduced_by: [algo.gp]\nrefresh: \"   \"\n",
	})
	if err == nil {
		t.Fatal("★ 仅空白的 refresh 必须被拒")
	}
}

// ───────────────────── 到期判定（IsDue）：真判定，不是装饰 ─────────────────────

// TestRefreshDue_NeverRefreshedIsDue 从未刷新的桶一律判到期（fail-closed）。
func TestRefreshDue_NeverRefreshedIsDue(t *testing.T) {
	_, buckets := loadAll(t)
	b, _ := buckets.Bucket("pnl_month")
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	due, reason := b.IsDue(nil, now)
	if !due {
		t.Fatalf("★ 从未刷新的桶必须判到期（fail-closed），实际未到期：%s", reason)
	}
	if reason == "" {
		t.Error("到期必须给出人类可读原因（运维要据此行动）")
	}
}

// TestRefreshDue_FreshIsNotDue 刚刷过的桶不应判到期。
func TestRefreshDue_FreshIsNotDue(t *testing.T) {
	_, buckets := loadAll(t)
	b, _ := buckets.Bucket("pnl_month")
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	just := now.Add(-1 * time.Hour)
	if due, reason := b.IsDue(&just, now); due {
		t.Fatalf("★ 1 小时前刷新过的桶不应到期：%s", reason)
	}
}

// TestRefreshDue_StaleIsDue 超过上限即判到期 —— 这是「静默陈旧」被消灭的证据。
func TestRefreshDue_StaleIsDue(t *testing.T) {
	_, buckets := loadAll(t)
	b, _ := buckets.Bucket("pnl_month")
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	// pnl_month = monthly_incremental（上限 31 天）；取 40 天前。
	old := now.Add(-40 * 24 * time.Hour)
	due, reason := b.IsDue(&old, now)
	if !due {
		t.Fatalf("★ 40 天未刷新的 pnl_month 必须判到期（否则静默陈旧无人知）：%s", reason)
	}
	if !strings.Contains(reason, "到期") {
		t.Errorf("原因应含『到期』便于运维检索，实际: %s", reason)
	}
}

// TestRefreshDue_ManualNeverAutoDue manual/event 无固定节律 ⇒ 不参与到期判定。
func TestRefreshDue_ManualNeverAutoDue(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, mode := range []string{"manual", "event"} {
		b := slot.Bucket{ID: "b_manual", Refresh: mode}
		old := now.Add(-10000 * time.Hour)
		if due, reason := b.IsDue(&old, now); due {
			t.Errorf("★ refresh=%s 无固定节律，不应判自动到期：%s", mode, reason)
		}
	}
}

// TestRefreshDue_UnknownModeFailsClosed 未知节律 ⇒ 判到期（fail-closed），
// 而不是「解析不了所以放行」。
func TestRefreshDue_UnknownModeFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	b := slot.Bucket{ID: "b_bad", Refresh: "weekly-ish"}
	recent := now.Add(-1 * time.Minute)
	if due, _ := b.IsDue(&recent, now); !due {
		t.Fatal("★ 未知节律必须 fail-closed 判到期，绝不能因『解析不了』而放行")
	}
}

// ───────────────────── 接线：判定函数必须真有生产调用点 ─────────────────────

// TestWiring_RefreshGateHasProductionCallSite ★ 核心接线断言：
// `gate.CheckRefreshDeclared` 必须在**非测试**代码里被调用。
//
// 这是本仓「判定函数没有生产调用点」这个病的**回归闸门**。
func TestWiring_RefreshGateHasProductionCallSite(t *testing.T) {
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
			if strings.Contains(line, "func CheckRefreshDeclared(") {
				continue // 定义处不算调用
			}
			if strings.Contains(line, "CheckRefreshDeclared(") {
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
		t.Fatal("★ gate.CheckRefreshDeclared 没有任何**非测试**调用点 ⇒ 该闸门恒真")
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

// TestWiring_RefreshDueHasProductionCallSite ★ 到期判定也必须有生产调用点 ——
// 否则「桶该刷了吗」依旧无人能答（同 freshness 那轮的教训）。
func TestWiring_RefreshDueHasProductionCallSite(t *testing.T) {
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
			if strings.Contains(line, "func CheckRefreshDue(") || strings.Contains(line, "func (b Bucket) IsDue(") {
				continue
			}
			if strings.Contains(line, "CheckRefreshDue(") || strings.Contains(line, ".IsDue(") {
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
		t.Fatal("★ 刷新到期判定没有任何**非测试**调用点 ⇒ 桶到期判定恒真（装饰）")
	}
}

// ───────────────────── 自证：断言覆盖强度 / 夹具正确性 ─────────────────────

// TestRefreshGate_FixtureCanDistinguishModes 自证夹具能区分「已知节律」与「自由文本」——
// 防止夹具太弱（本仓第四类缺陷：恒真型夹具）。
func TestRefreshGate_FixtureCanDistinguishModes(t *testing.T) {
	if _, ok := gate.ParseRefresh("monthly_incremental"); !ok {
		t.Fatal("夹具前提失败：monthly_incremental 应可解析")
	}
	if _, ok := gate.ParseRefresh("weekly-ish"); ok {
		t.Fatal("夹具前提失败：weekly-ish 不应可解析 —— 否则负向用例全部无意义")
	}
	// 合法但无节律的 manual 必须与「非法」区分开（否则会把 manual 误判为到期）。
	if !gate.RefreshHasInterval("daily") {
		t.Fatal("夹具前提失败：daily 应带固定节律")
	}
	if gate.RefreshHasInterval("manual") {
		t.Fatal("夹具前提失败：manual 不应带固定节律")
	}
}

// TestRefreshGate_UnitsSingleSourceOfTruth 防两侧漂移：
// slot 包不得自行复制一份节律表 —— 必须只引用 gate 的权威实现。
func TestRefreshGate_UnitsSingleSourceOfTruth(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "backend", "internal", "slot", "bucket.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "gate.ParseRefresh(") {
		t.Error("★ bucket.go 应通过 gate.ParseRefresh 解析节律（唯一权威），不得自行实现")
	}
	// 不得在 slot 包里硬编码节律字面量表。
	if strings.Contains(src, `"monthly_incremental":`) && !strings.Contains(src, "gate.ParseRefresh(") {
		t.Error("★ slot 包出现节律字面量表且未引用 gate 权威 —— 两侧会漂移")
	}
}
