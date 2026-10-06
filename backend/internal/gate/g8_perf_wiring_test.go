// g8_perf_wiring_test.go —— G8 性能闸门的**接线闸门**。
//
// 动机（★ 真实缺口，同一个病的第七个变种）：
//
//	`gate.CheckPerf` 此前全仓没有任何非测试调用点 —— 唯一「调用」它的
//	就是它自己的 `_test.go`。于是 G8 的四条阈值**一条都没在把关**，
//	而 docs/05 标着「✅ 已实现」，尾注还写着「实测由 CI 的 gate-perf 阶段
//	产出后喂给 CheckPerf」—— 可 CI 里根本没有 gate-perf 作业。
//
// 前六轮已分别在 authz/req/dr/audit 上补过「判定函数没有生产调用点」的洞
// （见 docs/05 / docs/11 的收口记录）。G8 的洞靠两个新东西补上：
//
//  1. `backend/cmd/spark-perf` —— 判定器的**执行入口**（调 `perf.Judge` →
//     `gate.CheckPerf`）；CI 的 `perf` 作业调用它。
//  2. 本文件 —— 钉住「CI 必须跑这个入口」与「文档阈值必须等于代码阈值」。
//
// ★ 为什么「接线」也要有闸门：一个存在但没人调用的判定器，和一个不存在的
// 判定器，在「能不能拦住坏事」这件事上**完全等价**。所以「有没有接上」
// 必须是一条可失败的断言，而不是一句注释里的承诺。
package gate_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// TestG8_CIWiresPerfJudge 断言 CI 真的会运行 spark-perf 判定器。
//
// 若有人删掉 perf 作业、或把它从汇总闸门的 needs 里摘掉，
// G8 就退回「阈值写在文档里、没人执行」的状态 —— 本断言会失败。
func TestG8_CIWiresPerfJudge(t *testing.T) {
	yml := repoFile(t, ".github/workflows/ci.yml")

	perfJob, ok := yamlJobBlock(yml, "perf")
	if !ok {
		t.Fatal("CI 没有 `perf` 作业 —— G8 判定器不会被任何生产/CI 路径调用，" +
			"四条阈值退回纸面（见本文件顶部事故记录）")
	}
	if !strings.Contains(perfJob, "spark-perf") {
		t.Errorf("`perf` 作业没有调用 spark-perf 判定器。\n作业内容：\n%s", perfJob)
	}
	// 必须真的测量首屏产物，否则判定器只拿到一份「什么都没测」的报告。
	if !strings.Contains(perfJob, "measure-bundle") {
		t.Errorf("`perf` 作业没有测量首屏产物（缺 --measure-bundle）—— "+
			"判定器会拿到空报告而非真实数字。\n作业内容：\n%s", perfJob)
	}

	// 汇总闸门必须依赖 perf，否则 perf 挂了也拦不住合并。
	gateJob, ok := yamlJobBlock(yml, "gate")
	if !ok {
		t.Fatal("CI 缺少汇总作业 `gate`")
	}
	if !yamlNeedsIncludes(gateJob, "perf") {
		t.Errorf("汇总作业 `gate` 的 needs 未包含 perf —— perf 失败不会阻断合并。\n作业内容：\n%s", gateJob)
	}
}

// TestG8_BudgetDocMatchesCode 断言 docs/05 的 G8 阈值 == gate.DefaultPerfBudget()。
//
// 阈值只允许有一处真值。文档与代码分叉时（例如文档写 200KB、代码写 300KB），
// 「文档说达标、代码却放行」或反之，会让人对闸门失去信任。
func TestG8_BudgetDocMatchesCode(t *testing.T) {
	doc := repoFile(t, "docs/05-验收闸门.md")
	got, err := parseG8Budget(doc)
	if err != nil {
		t.Fatalf("解析 docs/05 G8 阈值失败: %v", err)
	}
	want := gate.DefaultPerfBudget()

	checks := []struct {
		label string
		got   int
		want  int
	}{
		{"首屏 TTI", got.tti, want.TTIMs},
		{"筛选 P95（命中预计算）", got.hit, want.FilterP95HitMs},
		{"筛选 P95（未命中）", got.miss, want.FilterP95MissMs},
		{"首屏产物 gzip", got.gzipBytes, want.BundleGzipBytes},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("阈值漂移：docs/05 %q = %d，而 gate.DefaultPerfBudget = %d",
				c.label, c.got, c.want)
		}
	}
}

// TestG8_CIWiring_NegativeSelfCheck 是接线闸门的负向自测。
//
// 文本闸门最大的失效模式是「写了却永远为真」。这里喂进**故意残缺**的
// workflow，断言解析器真的会判「没接上」——而不是因为 Contains 写错而假绿。
func TestG8_CIWiring_NegativeSelfCheck(t *testing.T) {
	// 1) 有 perf 作业，但汇总闸门没依赖它 ⇒ 必须报「未接上」。
	noNeeds := `
jobs:
  perf:
    runs-on: ubuntu-latest
    steps:
      - run: go run ./cmd/spark-perf --measure-bundle ../web/dist
  gate:
    needs: [go, rust]
    steps:
      - run: echo ok
`
	gateJob, ok := yamlJobBlock(noNeeds, "gate")
	if !ok {
		t.Fatal("应能解析出 gate 作业")
	}
	if yamlNeedsIncludes(gateJob, "perf") {
		t.Error("gate.needs 未含 perf，却判为已包含 —— 负向自测失败（闸门永远为真）")
	}

	// 2) perf 作业存在但没有调用判定器 ⇒ 必须能被检出。
	noCall := `
jobs:
  perf:
    runs-on: ubuntu-latest
    steps:
      - run: echo "假装在测性能"
`
	perfJob, ok := yamlJobBlock(noCall, "perf")
	if !ok {
		t.Fatal("应能解析出 perf 作业")
	}
	if strings.Contains(perfJob, "spark-perf") {
		t.Error("perf 作业没有 spark-perf，却判为有 —— 负向自测失败")
	}
}

// TestG8_BudgetDoc_NegativeSelfCheck 是阈值解析的负向自测：
// 篡改一个数字后必须被检出（证明解析真的读到了那个数，而非恒等）。
func TestG8_BudgetDoc_NegativeSelfCheck(t *testing.T) {
	doc := repoFile(t, "docs/05-验收闸门.md")
	tampered := strings.Replace(doc, "| 首屏 TTI | < 1000 ms |", "| 首屏 TTI | < 9999 ms |", 1)
	if tampered == doc {
		t.Fatal("未能在文档中找到 TTI 阈值行 —— 解析器锚点已漂移")
	}
	got, err := parseG8Budget(tampered)
	if err != nil {
		t.Fatalf("解析篡改后的文档失败: %v", err)
	}
	if got.tti == gate.DefaultPerfBudget().TTIMs {
		t.Fatal("文档被篡改为 9999ms，解析结果却仍是代码阈值 —— 解析器没有真的读到文档")
	}
	if got.tti != 9999 {
		t.Fatalf("应解析出篡改值 9999，得到 %d", got.tti)
	}
}

// ───────────────────────────── 解析辅助 ─────────────────────────────

// yamlJobBlock 抽出 jobs 下某个作业的原始文本块。
//
// 作业键形如 `  perf:`（缩进 2 空格）；块在下一个同级作业键或文件结尾处结束。
// 只按缩进与键名判断，不引入 YAML 依赖。
func yamlJobBlock(doc, name string) (string, bool) {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, line := range lines {
		if line == "  "+name+":" { // 恰好 2 空格缩进的顶层作业键
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	var b strings.Builder
	for i := start; i < len(lines); i++ {
		if i > start && isJobHeaderLine(lines[i]) {
			break
		}
		b.WriteString(lines[i])
		b.WriteString("\n")
	}
	return b.String(), true
}

// isJobHeaderLine 判断一行是否是「另一个顶层作业键」（2 空格缩进 + 名字 + 冒号）。
func isJobHeaderLine(line string) bool {
	if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
		return false
	}
	rest := line[2:]
	if rest == "" || strings.HasPrefix(rest, "#") {
		return false
	}
	return strings.HasSuffix(rest, ":") && !strings.Contains(rest, " ")
}

// yamlNeedsIncludes 判断作业块里的 `needs:` 是否包含某作业名。
//
// 支持两种形态：`needs: [a, b]` 与 `needs: a`。
func yamlNeedsIncludes(jobBlock, name string) bool {
	for _, line := range strings.Split(jobBlock, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "needs:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "needs:"))
		rest = strings.Trim(rest, "[]")
		for _, item := range strings.Split(rest, ",") {
			if strings.TrimSpace(item) == name {
				return true
			}
		}
	}
	return false
}

// g8Budget 是从 docs/05 解析出的 G8 阈值。
type g8Budget struct {
	tti       int
	hit       int
	miss      int
	gzipBytes int
}

// parseG8Budget 解析 docs/05 的 G8 表格行。
//
// 行形态：`| 首屏 TTI | < 1000 ms |`（值单元格里的第一个整数 + 单位）。
func parseG8Budget(doc string) (g8Budget, error) {
	var out g8Budget
	numRe := regexp.MustCompile(`(\d+)`)
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		label := strings.TrimSpace(cells[1])
		value := strings.TrimSpace(cells[2])
		m := numRe.FindStringSubmatch(value)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		switch {
		case label == "首屏 TTI":
			out.tti = n
		case strings.HasPrefix(label, "筛选 P95（命中"):
			out.hit = n
		case strings.HasPrefix(label, "筛选 P95（未命中"):
			out.miss = n
		case label == "首屏产物 gzip":
			if strings.Contains(value, "KB") {
				out.gzipBytes = n * 1024
			} else {
				out.gzipBytes = n
			}
		}
	}
	return out, nil
}
