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
//	本仓的 G4 十侧**不是并排写在同一个方法里**，而是按依赖层次分工：
//	  * `slot.go: Registry.Validate`   —— 槽/算法自身的声明层（无桶依赖）；
//	  * `slot/bucket.go: validate`     —— 桶到位后才能判的四条
//	    （公式值绑定要按桶的 produced_by 串行链、桶→算法/桶→规则引用、
//	     桶的刷新节律）。
//	初版把十侧全指向 `Validate`，结果对三条**误报「装饰」** ——
//	这正是本仓反复强调的「先分辨是闸门漏检还是**断言本身错了**」。
//	故此处按**真实所在文件**分组扫，并在最终断言里要求两组都命中。

// g4TenSidesInRegistryValidate 是写在 `slot.go: Validate` 里的六侧。
func g4TenSidesInRegistryValidate() []struct{ Name, Why string } {
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

// g4TenSidesInBucketValidate 是写在 `slot/bucket.go: validate` 里的三侧
// （它们**必须**在桶到位之后才能判）。
func g4TenSidesInBucketValidate() []struct{ Name, Why string } {
	return []struct{ Name, Why string }{
		{"CheckFormulaVariablesBound", "公式自由变量的值绑定（逐桶串行链）"},
		{"CheckBucketProducersRegistered", "桶 → 算法 的反向引用完整性"},
		{"CheckRefreshDeclared", "桶的刷新节律必须可解析"},
	}
}

// needleFor 把判定函数名拼成**运行时** needle（防本文件自指恒真）。
func needleFor(fn string) string {
	return "gate." + fn // 名字在运行时拼接，本文件不出现完整字面量
}

// TestG4Wiring_AllTenSidesHaveProductionCallSites 断言 G4 十侧判定函数
// **全部**在生产路径里被调用（槽/算法侧在 `Registry.Validate`，
// 桶相关的三侧在 `bucket.go: validate`）。
//
// 这是「闸门不是装饰」的机器可校验证据。
func TestG4Wiring_AllTenSidesHaveProductionCallSites(t *testing.T) {
	regBody := readMethodBody(t, "slot.go", "func (r *Registry) Validate() error {")
	if len(regBody) < 2000 {
		t.Fatalf("Validate 方法体只读到 %d 字节 —— 夹具没读到真源码，本用例是假的", len(regBody))
	}
	bucketBody := readMethodBody(t, "bucket.go", "func (r *BucketRegistry) validate(reg *Registry, registeredRules map[string]bool) error {")
	if len(bucketBody) < 2000 {
		t.Fatalf("bucket.validate 方法体只读到 %d 字节 —— 夹具没读到真源码，本用例是假的", len(bucketBody))
	}

	for _, side := range g4TenSidesInRegistryValidate() {
		if !strings.Contains(regBody, needleFor(side.Name)) {
			t.Errorf("★ G4 判定函数 %s（%s）在 slot.Registry.Validate 里**没有真实的"+
				"生产调用点** ⇒ 该侧闸门是装饰（有定义、没消费），docs/05 标「已实现」也不成立",
				side.Name, side.Why)
		}
	}
	for _, side := range g4TenSidesInBucketValidate() {
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

// TestG4Wiring_RealRepoLoadsThroughAllTenSides 用**真仓库**的 slots/algorithms
// 跑一遍加载 —— 十侧闸门的第一天就会把「以前写得不完整的定义」全部照出来。
func TestG4Wiring_RealRepoLoadsThroughAllTenSides(t *testing.T) {
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	if err != nil {
		t.Fatalf("真仓库定义未能通过 G4 十侧闸门：%v", err)
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

// ─────────────────────────── 辅助 ───────────────────────────

// readMethodBody 读出 file 里 `sig` 到下一个顶格 func 之间的方法体源码
// （先把 CRLF 归一为 LF —— 本仓 Go 文件 100% CRLF，不归一会让扫描器
// 与手工 grep 的结果不一致）。
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
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
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
