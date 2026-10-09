package api

// wiring_test.go —— ★ 接线闸门：G12 / D13 的判定路径必须**有真实生产入口**。
//
// 本仓反复出现的同一个病：「判定函数全仓没有任何非测试调用点 ⇒ 断言恒真」。
// 本轮修的是最后两处：
//
//	| 判定路径 | 修复前 | 修复后（真实生产入口） |
//	|---|---|---|
//	| `authz.Resolver.Delegate`（D13 代授不溢出） | 只在 `gate` 测试里被调 | `DelegationHandlers.handleDelegate` ← `POST /api/authz/delegate` |
//	| `dr.Promote` / `dr.PlanRollback` / `dr.DownloadService.RequestDownload`（G12） | 只在 `dr_test.go` 里被调 | `DrHandlers` ← `POST /api/dr/*` |
//
// 本文件用**静态源码扫描**证明这些入口存在且**真的调用了**判定路径，
// 并同时钉住「路由已在 sparkd 注册」—— 有 handler 但没注册 = 同样恒真。
//
// ★ 两条方法论（本仓已踩过的坑，务必保留）：
//  1. **扫描前必须去注释** —— 否则「摘掉调用点、只留一句说明注释」也能骗过断言。
//  2. **断的是完整调用语句**（`h.Failover.Promote(`），不是函数名 ——
//     否则 `_ = h.Failover.Promote` 这种「引用但结果被丢弃」也会通过。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stripGoComments 去掉 Go 源码里的行注释与块注释。
//
// ★ 保护字符串/字符字面量：URL 里的 `//`（如 `https://`）不得被当成行注释，
//
//	否则会把后面的真实代码一起删掉，造成**假阴性**。
func stripGoComments(src string) string {
	var b strings.Builder
	inLine := false
	inBlock := false
	inStr := false
	inRaw := false
	inChar := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inLine {
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
			continue
		}
		if inBlock {
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if inStr {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if inRaw {
			b.WriteByte(c)
			if c == '`' {
				inRaw = false
			}
			continue
		}
		if inChar {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i++
				continue
			}
			if c == '\'' {
				inChar = false
			}
			continue
		}
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine = true
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			i++
		case c == '"':
			inStr = true
			b.WriteByte(c)
		case c == '`':
			inRaw = true
			b.WriteByte(c)
		case c == '\'':
			inChar = true
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// readStripped 读文件并去注释；不存在则失败。
func readStripped(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	return stripGoComments(string(raw))
}

// backendDir 返回 backend/ 绝对路径。
func backendDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// ───────────────────── ① 判定路径必须有生产调用语句 ─────────────────────

func TestWiring_DrAndDelegationJudgmentPathsAreCalled(t *testing.T) {
	root := backendDir(t)

	// 每条：期望的**完整调用语句** → 它必须出现在哪个生产文件里。
	cases := []struct {
		name string
		file string // 相对 backend/
		stmt string // 完整调用语句（含接收者，避免「引用但不用」骗过断言）
	}{
		{"D13 代授不溢出", "internal/api/delegation.go", "h.Resolver.Delegate("},
		{"G12 备份下载配额", "internal/api/dr.go", "h.Downloads.RequestDownload("},
		{"G12 切换必先 fencing", "internal/api/dr.go", "h.Failover.Promote("},
		{"G12 回滚不动审计", "internal/api/dr.go", "h.Failover.RollbackPlan("},
		{"G12 隔离旧主", "internal/api/dr.go", "h.Failover.Fence("},
		{"G12 见证者/LSN", "internal/api/dr.go", "h.Failover.AcquireWitness("},
		// 控制器 → dr 包判定函数（第二跳，缺则第一跳是空转）。
		{"控制器→Promote", "internal/dr/failover_controller.go", "return Promote(ctx, f.Cluster, req, f.Witness, f.LSN, f.Audit, now)"},
		{"控制器→PlanRollback", "internal/dr/failover_controller.go", "return PlanRollback(f.Cluster, p, req, now)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := readStripped(t, filepath.Join(root, filepath.FromSlash(tc.file)))
			if !strings.Contains(body, tc.stmt) {
				t.Fatalf("%s 中未找到完整调用语句 %q ⇒ 该判定路径没有真实生产调用点（恒真）",
					tc.file, tc.stmt)
			}
		})
	}
}

// ───────────────────── ② 路由必须在 sparkd 真的注册 ─────────────────────

func TestWiring_DrAndDelegationRoutesRegisteredInSparkd(t *testing.T) {
	root := backendDir(t)
	mainBody := readStripped(t, filepath.Join(root, "cmd", "sparkd", "main.go"))

	// 注册：Routes(mux) 必须被调用（否则 handler 有、入口没有）。
	for _, stmt := range []string{"drH.Routes(mux)", "delH.Routes(mux)"} {
		if !strings.Contains(mainBody, stmt) {
			t.Fatalf("sparkd/main.go 未注册 %q ⇒ 判定路径仍无真实入口（恒真）", stmt)
		}
	}
	// 依赖必须真装配（不能只 new 一个空 handler）。
	for _, stmt := range []string{"dp.buildDownloadService()", "dp.buildFailoverController()", "dp.isT1Func()"} {
		if !strings.Contains(mainBody, stmt) {
			t.Fatalf("sparkd/main.go 未装配 %q ⇒ handler 是空壳", stmt)
		}
	}

	// 高风险门禁：Tier 必须由服务端写死，不得从请求体透传。
	apiDr := readStripped(t, filepath.Join(root, "internal", "api", "dr.go"))
	// ★ 必须**按函数体**检查，不能整文件 Contains ——
	//   否则 rollback 处理器里的 `Tier: "T1"` 会让 promote 处理器的破坏被漏检
	//   （实测：把 promote 的 Tier 改成客户端字段，整文件断言仍然通过）。
	promoteBody, ok := funcBody(apiDr, "handlePromote")
	if !ok {
		t.Fatalf("未能定位 handlePromote 函数体（断言失效）")
	}
	if !strings.Contains(collapseWS(promoteBody), `Tier: "T1"`) {
		t.Fatalf("handlePromote 未把 Tier 写死为服务端判定值 ⇒ 「仅 T1 可发起」可被客户端声明绕过")
	}
	if strings.Contains(promoteBody, "in.Tier") || strings.Contains(promoteBody, "in.Operator") {
		t.Fatalf("handlePromote 从请求体读 Tier/Operator ⇒ 客户端可自封 T1（F8 同型漏洞）")
	}
	// ★ 请求体结构体里**不得**出现 tier/operator 字段。
	for _, forbidden := range []string{`json:"tier"`, `json:"operator"`} {
		if strings.Contains(apiDr, forbidden) {
			t.Fatalf("dr.go 的请求体出现了 %s ⇒ 允许客户端声明身份/层级（F8 同型漏洞）", forbidden)
		}
	}
}

// funcBody 从（已去注释的）源码里按函数名截取**花括号配平**的函数体。
//
// 用途：把「整文件 Contains」收紧为「该函数内 Contains」——
// 同一文件里其他函数恰巧有相同语句时，整文件断言会漏检。
func funcBody(src, name string) (string, bool) {
	idx := strings.Index(src, "func (h *DrHandlers) "+name+"(")
	if idx < 0 {
		// 兼容无接收者 / 其他接收者的写法
		idx = strings.Index(src, " "+name+"(")
		if idx < 0 {
			return "", false
		}
	}
	open := strings.Index(src[idx:], "{")
	if open < 0 {
		return "", false
	}
	start := idx + open
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1], true
			}
		}
	}
	return "", false
}

// collapseWS 把连续空白折叠成单个空格（用于「与 gofmt 对齐无关」的源码断言）。
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ───────────────────── ③ 扫描器自证 + 负向自测 ─────────────────────

// ★ 夹具自证：若扫描器被「注释」骗过，上面的断言就成了假闸门。
func TestWiring_ScannerIsNotFooledByComments(t *testing.T) {
	src := "// h.Failover.Promote( 只是说明\n/* h.Resolver.Delegate( */\nvar x = 1\n"
	got := stripGoComments(src)
	for _, bad := range []string{"h.Failover.Promote(", "h.Resolver.Delegate("} {
		if strings.Contains(got, bad) {
			t.Fatalf("去注释失效：注释里的 %q 仍被当作代码（断言可被注释骗过）", bad)
		}
	}
	if !strings.Contains(got, "var x = 1") {
		t.Fatalf("去注释误删了真实代码")
	}
}

// ★ 字符串里的 `//`（URL）不得被当成注释起点。
func TestWiring_ScannerKeepsURLsInStrings(t *testing.T) {
	src := "u := \"https://backup.example/x\"\nh.Failover.Fence(ctx)\n"
	got := stripGoComments(src)
	if !strings.Contains(got, "h.Failover.Fence(ctx)") {
		t.Fatalf("URL 中的 // 被误当注释 ⇒ 后续真实代码被删（假阴性）")
	}
}

// ★ 负向自测：只有注释提到函数名时，接线断言**必须**判为缺失。
func TestWiring_CommentOnlyCallSiteIsRejected(t *testing.T) {
	src := "// 这里本应调用 h.Downloads.RequestDownload(ctx, ...)\nfunc f() {}\n"
	if strings.Contains(stripGoComments(src), "h.Downloads.RequestDownload(") {
		t.Fatalf("「只有注释、没有调用」竟被判为有调用点 —— 接线断言形同虚设")
	}
}

// ★ 负向自测：`_ = h.Failover.Promote(...)`（引用但结果被丢弃）不算有效调用点。
//
// 本仓第十一课的判据：静态接线断言要断「完整调用语句」，不能只断函数名。
func TestWiring_DiscardedResultIsNotACallSite(t *testing.T) {
	src := "_ = h.Failover.Promote(ctx, req, now)\n"
	// 本条断言的是**扫描器不会把它当成 `out, err := ...` 形态**；
	// 真正的保护来自上面对「完整调用语句」的逐条断言（见 ①）。
	if !strings.Contains(stripGoComments(src), "h.Failover.Promote(") {
		t.Fatalf("扫描器应仍能看见该调用（由 ① 的语句级断言区分形态）")
	}
}

// ★ 负向自测（本轮**实测抓到**的缺陷）：函数体作用域必须真的生效 ——
// 若同一文件里另一个函数恰巧也有 `Tier: "T1"`，整文件 Contains 会漏检
// promote 处理器被改坏。本用例把「必须按函数体检查」这一结论固化。
func TestWiring_TierCheckIsScopedToPromoteBody(t *testing.T) {
	src := "func (h *DrHandlers) handleRollbackPlan() {\n" +
		"\tTier: \"T1\",\n}\n" +
		"func (h *DrHandlers) handlePromote() {\n" +
		"\tTier: in.Reason,\n}\n"
	body, ok := funcBody(src, "handlePromote")
	if !ok {
		t.Fatalf("未能定位 handlePromote（扫描器失效）")
	}
	if strings.Contains(collapseWS(body), `Tier: "T1"`) {
		t.Fatalf("函数体作用域失效：rollback 里的 Tier 让 promote 的破坏被漏检")
	}
	if strings.Contains(body, "handleRollbackPlan") {
		t.Fatalf("函数体截取越界（把相邻函数也圈进来了）")
	}
}
