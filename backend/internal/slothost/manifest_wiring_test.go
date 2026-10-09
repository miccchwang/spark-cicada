// manifest_wiring_test.go —— 把「sparkd 真的装载并挂载了清单」固化成断言。
//
// ★ 为什么必须是**静态源码扫描**而不是「调一次 LoadManifest 看返回」：
//
//	后者只能证明「判定函数**此刻**返回了空切片」—— 摘掉 wiring.go 里那一行
//	生产调用点，用例照样是绿的（那正是本 automation 反复抓到的
//	「判定函数没有生产调用点」形态）。唯一能分辨「有调用点」与「有定义没消费」
//	的办法，是**去看源码里有没有那一行**。
//
// ★ 两条纪律（均来自本仓实测教训）：
//
//	① 扫描前必须**去注释** —— 说明注释里会写出函数名，把真正的调用点摘掉、
//	   只留注释也能骗过 `strings.Contains`（R-20261008-04 实测漏检）。
//	② 断言必须断**完整调用语句**，不能只断「函数名」—— 把
//	   `man, err := slothost.LoadManifest(...)` 换成 `_ = slothost.LoadManifest(...)`
//	   （引用但结果被丢弃）会骗过只断函数名的断言（R-20261009-02 实测漏检）。
package slothost_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// slothostWiringFile 是被扫的生产装配文件（相对本包测试目录）。
const slothostWiringFile = "../../cmd/sparkd/wiring.go"

// stripGoComments 去掉 Go 源码里的注释（保护字符串/字符字面量，
// 防 URL 里的 `//` 被误删制造假红）。
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"' || c == '`' || c == '\'':
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
				i += j
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

// readLoadSpecsBody 读出 wiring.go 里 `loadSpecs` 的方法体。
//
// 返回 (原始体, 去注释体)：两者都要用 —— 原始体用于证明「去注释确实起了作用」，
// 去注释体用于真正的调用点断言。
func readLoadSpecsBody(t *testing.T) (raw, stripped string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, slothostWiringFile))
	if err != nil {
		t.Fatalf("读 %s 失败（本用例必须扫真源码）：%v", slothostWiringFile, err)
	}
	// 本仓既有 Go 文件 100% CRLF：先归一再扫，否则与手工 grep 的结果不一致。
	src := strings.ReplaceAll(string(b), "\r\n", "\n")
	const sig = "func (p *dataPlane) loadSpecs(specDir string) {"
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("%s 里没找到 %q —— 签名变了，请同步本扫描器", slothostWiringFile, sig)
	}
	rest := src[start:]
	if m := regexp.MustCompile(`\nfunc `).FindStringIndex(rest); m != nil {
		rest = rest[:m[0]]
	}
	return rest, stripGoComments(rest)
}

// TestG9ManifestWiring_SparkdLoadsAndMounts 断言 sparkd 的装配路径真的：
//
//	① 用**真实槽注册表**重设能力集；
//	② 装载 contracts/slot-manifest.yaml；
//	③ 把清单里的模块真的挂载。
//
// ★ needle 运行时拼接（本仓纪律）：避免「把要查的串以字面量写进断言自己」——
// 不过本用例扫的是 wiring.go 而非本文件，仍按惯例拆开写，并在下面用
// selfProofToken 自证「读到的确实是 wiring.go」。
func TestG9ManifestWiring_SparkdLoadsAndMounts(t *testing.T) {
	raw, body := readLoadSpecsBody(t)

	// ★ 扫描器自证 ①：本文件里存在、而 wiring.go 里不存在的哨兵 —— 若它出现在 body 里，
	//   说明读错了文件（断言自指）。
	const selfProofToken = "slothostWiringSelfProofToken"
	if strings.Contains(body, selfProofToken) {
		t.Fatalf("★ 扫描器读错了文件（读到本测试文件）：body 里出现了哨兵 %q", selfProofToken)
	}

	// ★ 扫描器自证 ②：去注释确实生效 —— 说明注释里的解释文字必须已被剥掉。
	//   （否则「摘掉调用点、只留注释」就能骗过下面的断言。）
	const commentOnly = "两个必须同源的事实源"
	if !strings.Contains(raw, commentOnly) {
		t.Fatalf("★ 说明注释消失了？%s 的 loadSpecs 里找不到 %q —— 请同步本用例", slothostWiringFile, commentOnly)
	}
	if strings.Contains(body, commentOnly) {
		t.Fatalf("★ 去注释未生效：body 里仍能看到注释文字 %q ⇒ 调用点断言可能被注释骗过", commentOnly)
	}

	// ① 能力集同源（完整调用语句）
	wantAvailable := "p.registry." + "SetAvailable(reg." + "RegisteredSlotIDs())"
	if !strings.Contains(body, wantAvailable) {
		t.Errorf("★ 恒真风险：%s 的 loadSpecs 里没有把能力集接到真实槽注册表 ——\n  期望完整语句：%s",
			slothostWiringFile, wantAvailable)
	}

	// ② 装载清单（完整调用语句，且结果被赋给变量 —— 防 `_ = ...` 式的假调用）
	wantLoad := "man, err := " + "slothost." + "LoadManifest(manifestPath, reg." + "RegisteredSlotIDs())"
	if !strings.Contains(body, wantLoad) {
		t.Errorf("★ 恒真风险：%s 的 loadSpecs 里没有真正装载 SlotManifest ——\n  期望完整语句：%s",
			slothostWiringFile, wantLoad)
	}

	// ③ 真的挂载（完整调用语句，且错误被检查）
	wantMount := "} else if err := " + "man." + "MountAll(p.registry); err != nil {"
	if !strings.Contains(body, wantMount) {
		t.Errorf("★ 恒真风险：%s 的 loadSpecs 里没有真正挂载模块 ——\n  期望完整语句：%s",
			slothostWiringFile, wantMount)
	}

	// ④ 失败必须进 specIssues（静默降级比启动失败危险 —— 本仓既有纪律）
	if !strings.Contains(body, `append(p.specIssues, "slot manifest`) {
		t.Errorf("★ 清单装载/挂载失败必须进 specIssues（/healthz 可观测），%s 里没有",
			slothostWiringFile)
	}
}

// TestG9ManifestWiring_ScannerIsDiscriminating 是扫描器自身的**判别性自证**：
// 把一个 needle 从 body 里抹掉后，同一套判定必须变红。
//
// 若不做这一步，上面的断言可能是「无论 body 是什么都通过」的恒真断言。
func TestG9ManifestWiring_ScannerIsDiscriminating(t *testing.T) {
	_, body := readLoadSpecsBody(t)
	needle := "man." + "MountAll(p.registry)"
	if !strings.Contains(body, needle) {
		t.Fatalf("夹具前提不成立：body 里本应有 %q", needle)
	}
	sabotaged := strings.Replace(body, needle, "manMountAllRemoved", 1)
	if strings.Contains(sabotaged, needle) {
		t.Fatal("★ 判别性自证失败：抹掉后仍能命中 ⇒ 断言恒真")
	}
}
