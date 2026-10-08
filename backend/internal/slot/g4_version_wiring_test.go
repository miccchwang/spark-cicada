package slot_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// 本自动化此前判「闸门是不是装饰」靠的是**手工 grep**；本文件把它固化成断言。
//
// ★ 为什么必须是**静态源码扫描**而不是「调一次 Validate 看返回」：
//
//	后者只能证明「某个判定函数**此刻**返回了空切片」—— 摘掉一行生产调用点
//	它照样返回空切片，用例依然是绿的。这正是本仓反复出现的
//	「断言测的是判定函数的返回值，而不是链路」形态（2026-10-07 实测）。
//	唯一能分辨「有调用点」与「有定义没消费」的办法，是**去看源码里有没有那一行**。
//
// ★ 为什么 needle 必须**运行时拼接**：
//
//	若把 `gate.CheckUnitDeclared` 以字面量写进本文件的 required 列表，
//	本文件自身就包含了这个串 ⇒ `strings.Contains(body, needle)` 会命中**自己**，
//	断言变成恒真（测试自指）。这是 2026-10-07 实测踩到的坑，故此处
//	把 needle 拆成两半用 `+` 拼接 —— 扫描的是被扫文件，不是本文件。
//
// ★ 已知局限（如实标注，不假装更严）：
//
//	本断言只证明「该函数名出现在被扫源码的方法体里」，
//	**不**证明它真的被用于判定（理论上可写 `_ = gate.CheckX`）。
//	配合「注入破坏实测」（摘掉调用点 ⇒ 本用例必须红）已足够：
//	任何**有效的**破坏都会让这一行消失。
//
// ★ 为什么分两组扫（2026-10-08 修正）：
//
//	本仓的 G4 各侧**不是并排写在同一个方法里**，而是按依赖层次分工：
//	  * `slot.go: Registry.Validate`   —— 槽/算法自身的声明层（无桶依赖）；
//	  * `slot/bucket.go: validate`     —— 桶到位后才能判的四条
//	    （公式值绑定要按桶的 produced_by 串行链、桶→算法/桶→规则引用、
//	     桶的刷新节律、桶的索引声明）。
//	初版把各侧全指向 `Validate`，结果对三条**误报「装饰」** ——
//	这正是本仓反复强调的「先分辨是闸门漏检还是**断言本身错了**」。
//	故此处按**真实所在文件**分组扫，并在最终断言里要求两组都命中。

// g4SidesInRegistryValidate 是写在 `slot.go: Validate` 里的七侧。
func g4SidesInRegistryValidate() []struct{ Name, Why string } {
	return []struct{ Name, Why string }{
		{"CheckAlgorithmNoDataSource", "算法里不得混入数据源（分离）"},
		{"CheckSlotsRegistered", "算法 → 槽 的引用完整性"},
		{"CheckSlotSourceResolvable", "槽的数据来源必须可解析"},
		{"CheckKeyStrategyDeclared", "槽的匹配键策略必须可解析"},
		{"CheckPermissionDeclared", "槽/算法的密级必须可解析"},
		{"CheckUnitDeclared", "算法的计量单位必须可解析"},
		{"CheckAlgorithmVersionDeclared", "算法的版本号必须可解析"},
	}
}

// g4SidesInBucketValidate 是写在 `slot/bucket.go: validate` 里的四侧
// （它们**必须**在桶到位之后才能判）。
func g4SidesInBucketValidate() []struct{ Name, Why string } {
	return []struct{ Name, Why string }{
		{"CheckFormulaVariablesBound", "公式自由变量的值绑定（逐桶串行链）"},
		{"CheckBucketProducersRegistered", "桶 → 算法 的反向引用完整性"},
		{"CheckRefreshDeclared", "桶的刷新节律必须可解析"},
		{"CheckBucketIndexesDeclared", "桶的索引声明必须可解析（G4 第十一侧）"},
		{"CheckBucketGrainDeclared", "桶的粒度声明必须可解析（G4 第十二侧）"},
	}
}

// needleFor 把判定函数名拼成**运行时** needle（防本文件自指恒真）。
func needleFor(fn string) string {
	return "gate." + fn // 名字在运行时拼接，本文件不出现完整字面量
}

// TestG4Wiring_AllSidesHaveProductionCallSites 断言 G4 各侧判定函数
// **全部**在生产路径里被调用（槽/算法侧在 `Registry.Validate`，
// 桶相关的四侧在 `bucket.go: validate`）。
//
// 这是「闸门不是装饰」的机器可校验证据。
func TestG4Wiring_AllSidesHaveProductionCallSites(t *testing.T) {
	regBody := readMethodBody(t, "slot.go", "func (r *Registry) Validate() error {")
	// ★ 阈值 600（而非早前的 2000）：`readMethodBody` 现在**去掉了注释**，
	//   而本仓方法体绝大部分是注释 ⇒ 去注释后字节数大幅下降（Validate ≈1.5KB）。
	//   阈值的作用只是「证明夹具真读到了源码、没盯错文件」。
	if len(regBody) < 600 {
		t.Fatalf("Validate 方法体只读到 %d 字节（去注释后）—— 夹具没读到真源码，本用例是假的", len(regBody))
	}
	bucketBody := readMethodBody(t, "bucket.go", "func (r *BucketRegistry) validate(reg *Registry, registeredRules map[string]bool) error {")
	if len(bucketBody) < 600 {
		t.Fatalf("bucket.validate 方法体只读到 %d 字节（去注释后）—— 夹具没读到真源码，本用例是假的", len(bucketBody))
	}

	for _, side := range g4SidesInRegistryValidate() {
		if !strings.Contains(regBody, needleFor(side.Name)) {
			t.Errorf("★ G4 判定函数 %s（%s）在 slot.Registry.Validate 里**没有真实的"+
				"生产调用点** ⇒ 该侧闸门是装饰（有定义、没消费），docs/05 标「已实现」也不成立",
				side.Name, side.Why)
		}
	}
	for _, side := range g4SidesInBucketValidate() {
		if !strings.Contains(bucketBody, needleFor(side.Name)) {
			t.Errorf("★ G4 判定函数 %s（%s）在 bucket.validate 里**没有真实的生产调用点**"+
				" ⇒ 该侧闸门是装饰", side.Name, side.Why)
		}
	}
}

// TestG4Wiring_ScannerCanDetectRemoval 是**夹具自证**：把 needle 换成
// 一个绝不存在的名字，断言扫描器立刻报「找不到」。
//
// 没有这一条，上面那条用例在「扫描器永远返回 true / 扫错文件」时也是绿的。
func TestG4Wiring_ScannerCanDetectRemoval(t *testing.T) {
	regBody := readMethodBody(t, "slot.go", "func (r *Registry) Validate() error {")
	ghost := "gate.CheckThisFunctionDoesNotExistAtAll"
	if strings.Contains(regBody, ghost) {
		t.Fatalf("夹具无效：不存在的名字 %q 竟然被扫到了", ghost)
	}
	// 且真名字**必须**能扫到（证明扫描器不是恒返 false）。
	if !strings.Contains(regBody, needleFor("CheckUnitDeclared")) {
		t.Fatal("夹具无效：连已确认存在的 CheckUnitDeclared 都扫不到 ⇒ 扫描器盯错文件")
	}
}

// TestG4Wiring_RealRepoLoadsThroughAllSides 用**真仓库**的 slots/algorithms
// 跑一遍加载 —— 各侧闸门的第一天就会把「以前写得不完整的定义」全部照出来。
func TestG4Wiring_RealRepoLoadsThroughAllSides(t *testing.T) {
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	if err != nil {
		t.Fatalf("真仓库定义未能通过 G4 各侧闸门：%v", err)
	}
	// 夹具自证：确实读到了真实定义（不是空目录「因为没东西可查」而通过）。
	if len(reg.Slots()) < 5 || len(reg.Algorithms()) < 2 {
		t.Fatalf("真仓库只有 %d 槽 / %d 算法 —— 夹具没读到真定义",
			len(reg.Slots()), len(reg.Algorithms()))
	}
	// 版本号旁路：真仓库每个算法的原文都必须是可解析的正整数。
	for _, d := range reg.AlgorithmVersionDocs() {
		if strings.TrimSpace(d.Raw) == "" {
			t.Errorf("算法 %s 的 version 原文未被旁路保留（Raw 为空）—— "+
				"声明层闸门将只能看到 0，报错信息会误导人", d.AlgoID)
		}
	}
}

// TestG4Wiring_CommentIsNotACallSite 是**夹具自证**（2026-10-08 新增）：
// 断言 `stripGoComments` 真的把注释去掉 —— 否则「调用点存在」可以被一段
// **说明注释**骗过（注入实测 B 就是这么漏检的）。
func TestG4Wiring_CommentIsNotACallSite(t *testing.T) {
	line := "func f() {\n\t// gate.CheckOnlyInComment(x)\n\t_ = 1\n}\n"
	if strings.Contains(stripGoComments(line), "CheckOnlyInComment") {
		t.Fatal("★ stripGoComments 没去掉行注释 ⇒ 接线断言可被注释骗过（漏检）")
	}
	block := "func f() {\n\t/* gate.CheckOnlyInBlock(x) */\n\t_ = 1\n}\n"
	if strings.Contains(stripGoComments(block), "CheckOnlyInBlock") {
		t.Fatal("★ stripGoComments 没去掉块注释 ⇒ 接线断言可被注释骗过（漏检）")
	}
	// 反向：字符串字面量里的 `//` 不得被误删（否则 URL 被截断、制造假红）。
	lit := "var u = \"http://example.com\"\n"
	if !strings.Contains(stripGoComments(lit), "http://example.com") {
		t.Fatal("★ stripGoComments 误删了字符串字面量里的 // ⇒ 会制造假红")
	}
	// 反向：真调用点必须保留。
	real := "func f() {\n\tgate.CheckReal(x)\n}\n"
	if !strings.Contains(stripGoComments(real), "gate.CheckReal") {
		t.Fatal("★ stripGoComments 误删了真调用点")
	}
}

// ─────────────────────────── 辅助 ───────────────────────────

// stripGoComments 去掉 Go 源码里的注释（行注释 `//` 与块注释 `/* */`），
// 但**保护字符串 / 字符字面量**（`"…"` / “ `…` “ / `'…'`）—— 否则 URL 里的 `//`
// 会被误删，让「调用点存在」的判定产生假阴性（假红）。
//
// ★ 为什么必须去注释（2026-10-08 注入实测逼出，第六类缺陷的新实例）：
//
//	G4 各侧的接线说明注释里**会写出判定函数名**（例如「现把
//	gate.CheckBucketGrainDeclared 接成真实生产调用点」）。若扫描前不去注释，
//	把真正的调用点摘掉、只留那段说明注释，扫描器**照样命中** ⇒ 接线断言被
//	注释骗过（漏检）。这正是本仓反复强调的「断言覆盖强度不足」形态 ——
//	断言看着在测「有没有调用点」，实际测的是「文件里出现过这个字符串」。
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"' || c == '`' || c == '\'':
			// 字符串 / 字符字面量：整段原样保留（含其中的 // 与 /*）。
			q := c
			b.WriteByte(c)
			i++
			for i < len(src) {
				if src[i] == '\\' && q != '`' && i+1 < len(src) {
					b.WriteByte(src[i])
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				b.WriteByte(src[i])
				if src[i] == q {
					i++
					break
				}
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				i = len(src)
			} else {
				i += j // 保留换行本身（下一轮作为普通字节写出）
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				i = len(src)
			} else {
				i += 2 + j + 2
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// readMethodBody 读出 file 里 `sig` 到下一个顶格 func 之间的方法体源码
// （先把 CRLF 归一为 LF —— 本仓 Go 文件 100% CRLF，不归一会让扫描器
// 与手工 grep 的结果不一致；再**去掉注释** —— 见 stripGoComments）。
func readMethodBody(t *testing.T, file, sig string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatalf("读 %s 失败（本用例必须扫真源码）：%v", file, err)
	}
	src := stripGoComments(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("%s 里没找到方法签名 %q —— 签名变了，请同步本扫描器", file, sig)
	}
	rest := src[start:]
	re := regexp.MustCompile(`\nfunc `)
	if m := re.FindStringIndex(rest); m != nil {
		rest = rest[:m[0]]
	}
	return rest
}
