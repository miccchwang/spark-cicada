// Command spark-perf —— G8 性能闸门的**执行入口**。
//
// 职责：把实测数字（可来自 `--in` 报告文件，和/或本地真实测量）交给
// `perf.Judge` → `gate.CheckPerf` 判定，并以退出码表达结果。
//
//	退出码 0：通过
//	退出码 1：未通过（有已测指标超标，或 strict 下有未测指标）
//	退出码 2：用法/解析/测量错误
//
// ★ 为什么要有这个命令：在它出现之前，`gate.CheckPerf` 全仓**没有任何
// 非测试调用点** —— G8 的四条阈值一条都没在把关。CI 的 `perf` 作业调用本命令，
// 使「阈值」第一次成为真正的红线（详见 docs/05 G8 尾注、docs/11 G8 收口）。
//
// 目前能**真实**测出的只有「首屏产物 gzip」（`--measure-bundle`，确定性、
// 无需浏览器）。TTI / 筛选 P95 需要无头浏览器 / 真库，尚不能在本机或
// 当前 CI 作业中产出 ⇒ 它们**如实标记为未测**（`--strict` 会让未测判失败，
// CI 因此暂用非严格模式，并把未测项打进日志）。
package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/perf"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是可测试的入口（不直接读 os.Args / os.Exit）。
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spark-perf", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		inPath       = fs.String("in", "", "实测报告 JSON 路径（'-' 表示 stdin）")
		bundleDir    = fs.String("measure-bundle", "", "本地真实测量首屏产物 gzip 的目录（如 web/dist）")
		strict       = fs.Bool("strict", false, "fail-closed：有任一未测指标即判失败")
		asJSON       = fs.Bool("json", false, "以 JSON 输出判定结果")
		budgetTTIMs  = fs.Int("budget-tti-ms", 0, "覆盖首屏 TTI 预算（ms）；0 = 用默认")
		budgetHitMs  = fs.Int("budget-hit-ms", 0, "覆盖筛选 P95（命中）预算（ms）；0 = 用默认")
		budgetMissMs = fs.Int("budget-miss-ms", 0, "覆盖筛选 P95（未命中）预算（ms）；0 = 用默认")
		budgetGzipKB = fs.Int("budget-gzip-kb", 0, "覆盖首屏产物 gzip 预算（KB）；0 = 用默认")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *inPath == "" && *bundleDir == "" {
		fmt.Fprintln(stderr, "用法：spark-perf [--in 报告.json] [--measure-bundle web/dist] [--strict] [--json]")
		return 2
	}

	budget := gate.DefaultPerfBudget()
	if *budgetTTIMs > 0 {
		budget.TTIMs = *budgetTTIMs
	}
	if *budgetHitMs > 0 {
		budget.FilterP95HitMs = *budgetHitMs
	}
	if *budgetMissMs > 0 {
		budget.FilterP95MissMs = *budgetMissMs
	}
	if *budgetGzipKB > 0 {
		budget.BundleGzipBytes = *budgetGzipKB * 1024
	}

	report := perf.Report{}
	if *inPath != "" {
		raw, err := readInput(*inPath)
		if err != nil {
			fmt.Fprintf(stderr, "读取报告失败: %v\n", err)
			return 2
		}
		report, err = perf.ParseReport(raw)
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 2
		}
	}

	if *bundleDir != "" {
		bytes, files, err := measureBundleGzip(*bundleDir)
		if err != nil {
			fmt.Fprintf(stderr, "测量首屏产物 gzip 失败: %v\n", err)
			return 2
		}
		report.BundleGzipBytes = bytes
		report.Measured = addMeasured(report.Measured, string(perf.MetricBundleGzip))
		fmt.Fprintf(stdout, "实测首屏产物 gzip：%d 字节（%.1f KB），文件 %v\n",
			bytes, float64(bytes)/1024.0, files)
	}

	verdict, err := perf.Judge(report, budget, *strict)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(verdict); err != nil {
			fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
			return 2
		}
	} else {
		printVerdict(stdout, verdict, budget)
	}

	if verdict.Passed {
		return 0
	}
	return 1
}

// readInput 读取报告：'-' 走 stdin，否则读文件。
func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// measureBundleGzip 真实测量首屏产物的 gzip 体积。
//
// 口径（与 docs/05 G8「首屏产物 gzip < 200 KB」对应）：
//   - 只计**首屏会加载**的静态资源：.js / .css / .html；
//   - 排除 .map（sourcemap 不随首屏下发）；
//   - 逐文件 gzip 后**求和** —— 真实 HTTP 是每个文件各自压缩后下发的，
//     把多个文件拼起来再压会低估体积。
//
// 返回（总字节数, 参与统计的文件名, error）。目录内无任何匹配文件时报错：
// 静默返回 0 会让闸门「因为没东西可测」而永远通过。
func measureBundleGzip(dir string) (int, []string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".js", ".css", ".html":
		default:
			return nil // 含 .map 在内的一切非首屏资源不计
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if len(files) == 0 {
		return 0, nil, fmt.Errorf("目录 %s 下没有 .js/.css/.html（构建产物缺失？）", dir)
	}
	sort.Strings(files)

	total := 0
	names := make([]string, 0, len(files))
	for _, f := range files {
		n, err := gzipSize(f)
		if err != nil {
			return 0, nil, err
		}
		total += n
		names = append(names, filepath.Base(f))
	}
	return total, names, nil
}

// gzipSize 返回单个文件 gzip 后的字节数（默认压缩级别，结果稳定）。
func gzipSize(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var buf countingWriter
	zw := gzip.NewWriter(&buf)
	if _, err := io.Copy(zw, f); err != nil {
		return 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	return buf.n, nil
}

type countingWriter struct{ n int }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += len(p)
	return len(p), nil
}

// addMeasured 去重地加入一个已测指标名。
func addMeasured(list []string, name string) []string {
	for _, x := range list {
		if x == name {
			return list
		}
	}
	return append(list, name)
}

// printVerdict 打印人类可读结论。
func printVerdict(w io.Writer, v perf.Verdict, b gate.PerfBudget) {
	fmt.Fprintf(w, "预算：TTI<%dms  P95命中<%dms  P95未命中<%dms  gzip<%dKB\n",
		b.TTIMs, b.FilterP95HitMs, b.FilterP95MissMs, b.BundleGzipBytes/1024)
	fmt.Fprintf(w, "已测：%s\n", strings.Join(v.Measured, ", "))
	for _, viol := range v.Violations {
		fmt.Fprintf(w, "  ✗ %s\n", viol)
	}
	if len(v.Unmeasured) > 0 {
		fmt.Fprintf(w, "未测（不计入通过，也不判为超标）：%s\n", strings.Join(v.Unmeasured, ", "))
	}
	fmt.Fprintf(w, "%s\n", v.Summary)
}
