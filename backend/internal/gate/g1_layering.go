// G1 · 分层依赖闸门 —— 静态源码扫描。
//
// docs/05 G1 要求把「三段式解耦」固化为可自动失败的断言：
//   * M-FILTER 不含网络调用
//   * M-RENDER 不读 QueryState
//   * M-QUERY 不含业务公式字面量
//   * 无反向依赖（L5 不直接依赖 L2 及以下）
//
// 实现方式：对源码做**文本级 token 扫描**（不是完整 AST 分析）。
// 这是有意的取舍——闸门要在 CI 里秒级跑完且零语言依赖；漏报由 code review 兜底，
// 误报可通过白名单注释 `// gate:allow-<rule>` 消除。
package gate

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// SourceFile 参与静态扫描的源文件。
type SourceFile struct {
	Path    string // 仓库相对路径
	Content string
}

// 业务公式字段名（出现在 SQL / 字符串字面量里即为疑似「查询层写公式」）。
var businessFormulaTokens = []string{
	"gp", "gmp", "cogs", "net_contrib", "cm1", "cm2", "roas", "acos",
}

// networkTokens 网络调用特征。
var networkTokens = []string{
	"fetch(", "axios", "http.Get(", "http.Post(", "XMLHttpRequest", "reqwest::", "requests.get(",
}

// qsTokens QueryState 特征（渲染层不得引用）。
var qsTokens = []string{"QueryState", "query-state", "queryState"}

// GateAllow 注释：`gate:allow-<rule>` 可在同一行豁免某条规则。
var allowRe = regexp.MustCompile(`gate:allow-([a-z0-9_,-]+)`)

func isAllowed(line, rule string) bool {
	m := allowRe.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	for _, r := range strings.Split(m[1], ",") {
		if strings.TrimSpace(r) == rule {
			return true
		}
	}
	return false
}

// CheckFilterNoNetwork 断言 M-FILTER 层不含网络调用（G1）。
func CheckFilterNoNetwork(files []SourceFile) []string {
	var out []string
	for _, f := range files {
		if !underLayer(f.Path, "filter") {
			continue
		}
		for i, line := range strings.Split(f.Content, "\n") {
			low := strings.ToLower(line)
			for _, tok := range networkTokens {
				if strings.Contains(low, strings.ToLower(tok)) && !isAllowed(line, "filter-network") {
					out = append(out, fmt.Sprintf("%s:%d M-FILTER 含网络调用 %q", f.Path, i+1, tok))
				}
			}
		}
	}
	return out
}

// CheckRenderNoQueryState 断言 M-RENDER 层不读 QueryState（G1）。
func CheckRenderNoQueryState(files []SourceFile) []string {
	var out []string
	for _, f := range files {
		if !underLayer(f.Path, "render") {
			continue
		}
		for i, line := range strings.Split(f.Content, "\n") {
			for _, tok := range qsTokens {
				if strings.Contains(line, tok) && !isAllowed(line, "render-qs") {
					out = append(out, fmt.Sprintf("%s:%d M-RENDER 引用 QueryState（%s）", f.Path, i+1, tok))
				}
			}
		}
	}
	return out
}

// CheckQueryNoBusinessFormula 断言 M-QUERY 层不含业务公式字面量（G1）。
//
// 判定：在字符串字面量或 SQL 片段里出现 gp/cogs 等派生字段名 —— 说明查询层在「造数」。
// 例外：列名白名单（key/维度列）不算；`algo.gp` 这类**算法 ID 引用**不算公式。
func CheckQueryNoBusinessFormula(files []SourceFile) []string {
	var out []string
	for _, f := range files {
		if !underLayer(f.Path, "query") {
			continue
		}
		for i, line := range strings.Split(f.Content, "\n") {
			if isAllowed(line, "query-formula") {
				continue
			}
			for _, tok := range businessFormulaTokens {
				// 只查「疑似公式」形态：算术运算符紧邻字段名，或 AS 别名
				if containsFormulaUsage(line, tok) {
					out = append(out, fmt.Sprintf("%s:%d M-QUERY 疑似含业务公式 %q", f.Path, i+1, tok))
				}
			}
		}
	}
	return out
}

// containsFormulaUsage 判断一行里是否为「公式用法」而非普通字段引用。
func containsFormulaUsage(line, tok string) bool {
	// `algo.gp` / `"gp"` 单纯引用不算；`rev - cogs` / `gp / rev` 才算
	ops := []string{" + ", " - ", " * ", " / "}
	for _, op := range ops {
		if strings.Contains(line, op) && strings.Contains(line, tok) {
			return true
		}
	}
	// `AS gp` / `as gp` 别名（在 SQL 里造派生列）
	if strings.Contains(strings.ToLower(line), "as "+tok) {
		return true
	}
	return false
}

// underLayer 判断路径是否属于某个逻辑层目录。
// 约定目录：filter / query / render（大小写不敏感）。
func underLayer(path, layer string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == layer {
			return true
		}
	}
	return false
}
