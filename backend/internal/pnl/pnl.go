// Package pnl —— M-PNL 损益口径（calc caliber）的**结构**决策层。
//
// 职责边界（docs/03 §4.3、docs/02 M-PNL）：
//   - 回答「用哪个口径」「该口径下折扣归到哪一行」「模块默认口径是什么」；
//   - 把口径切换做成**可审计的决策**（谁在什么时候把口径从 A 换成 B）。
//
// ★★ 红线纪律：本包**不实现任何业务公式**。
//
//	「rev − cogs」「gp / rev」「Σ(qty × unit_price)」一律由 compute 内核
//	（Rust）产出。本包只做**结构**层面的判断：折扣在报表上的归属位置。
//	（把折扣从「费用侧」搬到「收入侧」是记账归属，不是重算 ——
//	 两侧的数字都来自内核。）
//
// ★ 为什么口径需要 Go 侧这一层而不是「前端随便传一个字符串」：
//
//	口径会**完全改变经营解读**（docs/03 §4.3 的 KODP 实测：同一份数据，
//	口径 A 显示 −152,567.19、口径 B 显示 +123.17）。这类「看法」如果只由
//	前端 URL 参数决定，就没人能回答「昨天这张报表是用什么口径出的」。
//	所以口径必须在服务端被解析、被校验、被记录。
package pnl

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Caliber 折扣口径。
//
//	CaliberA —— 参考口径：Seller Discount 计入**营销费用**。
//	            营销费用虚高，容易被误读为「费用失控」。
//	CaliberB —— Finance 口径：Seller Discount 作 **contra-revenue（收入抵减）**。
//	            揭示真实问题是「折扣压缩毛利」。
type Caliber string

const (
	CaliberA Caliber = "A"
	CaliberB Caliber = "B"
)

// Calibers 全部合法口径（有序）。
var Calibers = []Caliber{CaliberA, CaliberB}

// ErrInvalidCaliber 非法口径。
var ErrInvalidCaliber = errors.New("pnl: invalid caliber")

// ParseCaliber 严格解析口径。
//
// ★ 非法值**返回错误而非回退**：这是刻意的。
//
//	「静默按 A 展示」的风险是：财务同事拿着 A 的数字去开会，
//	而他以为自己在看 B。宁可 400 让人当场发现参数写错。
func ParseCaliber(s string) (Caliber, error) {
	switch Caliber(strings.TrimSpace(s)) {
	case CaliberA:
		return CaliberA, nil
	case CaliberB:
		return CaliberB, nil
	}
	return "", fmt.Errorf("%w: %q（合法值 %s）", ErrInvalidCaliber, s, joinCalibers())
}

// IsValidCaliber 判断口径是否合法。
func IsValidCaliber(c Caliber) bool {
	return c == CaliberA || c == CaliberB
}

func joinCalibers() string {
	parts := make([]string, 0, len(Calibers))
	for _, c := range Calibers {
		parts = append(parts, string(c))
	}
	return strings.Join(parts, " / ")
}

// ───────────────────────────── 折扣归属 ─────────────────────────────

// TargetLine 折扣在报表上的归属行。
type TargetLine string

const (
	// LineMarketing 营销费用（费用侧）。
	LineMarketing TargetLine = "marketing"
	// LineSellerDiscount 卖家折扣（收入抵减侧）。
	LineSellerDiscount TargetLine = "seller_discount"
)

// DiscountTarget 口径 → 折扣归属行。
//
// ★ 这是两口径**唯一**的结构性差异。做成显式映射（而不是散在各处 if）
// 是为了：「折扣搬到哪一行」这件事可以被断言、可以被审计、可以被文档引用。
func DiscountTarget(c Caliber) TargetLine {
	if c == CaliberA {
		return LineMarketing
	}
	return LineSellerDiscount
}

// SellerDiscountRole 折扣的角色（与前端契约 CaliberMeta.sellerDiscountRole 对齐）。
func SellerDiscountRole(c Caliber) string {
	if c == CaliberA {
		return "marketing_expense"
	}
	return "contra_revenue"
}

// MisreadingRisk 该口径最容易被误读成什么（供 UI/导出文案直接使用）。
//
// ★ 这不是「友好提示」而是**必需信息**：KODP 的教训就是同一份数据
// 两个口径给出符号相反的结论，而两者都没错。不告诉用户该口径的陷阱，
// 就等于制造一个「数据看起来在骗人」的现场。
func MisreadingRisk(c Caliber) string {
	if c == CaliberA {
		return "营销费用会虚高，容易被误读为「费用失控」——实际问题可能是折扣过深"
	}
	return "收入侧被压低，容易被误读为「卖不动」——实际问题可能是毛利率被折扣压缩"
}

// ───────────────────────────── 模块默认口径（D10） ─────────────────────────────

// 模块 ID（与 authz 的 ModuleGrant.Enabled 取值一致）。
const (
	ModuleReport = "module.report" // 经营报表：运营视角
	ModulePnL    = "module.pnl"    // P&L：财务视角
)

// DefaultByModule 决策项 D10 的落点：「**分模块各自默认**」。
//
//	经营报表要跟运营对话（GMV 为主）⇒ 运营口径 A；
//	P&L 要跟财务对话 ⇒ 财务口径 B。
//
// ★ D10 明确否决了「全局一个默认口径」，因为两个模块的读者不是同一批人：
//
//	给运营看财务口径，他会觉得「收入怎么这么低」；
//	给财务看运营口径，他会觉得「费用怎么这么高」。
//
// 兜底口径取 B（财务口径更保守：不会把费用显示得失控，减少误判恐慌）。
var DefaultByModule = map[string]Caliber{
	ModuleReport: CaliberA,
	ModulePnL:    CaliberB,
}

// FallbackCaliber 模块未登记时的兜底口径。
const FallbackCaliber = CaliberB

// DefaultCaliber 取模块的默认口径（未登记 ⇒ FallbackCaliber）。
func DefaultCaliber(module string) Caliber {
	if c, ok := DefaultByModule[module]; ok {
		return c
	}
	return FallbackCaliber
}

// Resolved 口径解析结果（带来源，便于排障「为什么这里是 B」）。
type Resolved struct {
	Caliber Caliber
	// Source: requested | module_default | fallback
	Source string
	// Downgraded：请求值非法/缺失而走了默认
	Downgraded bool
	// Reason 降级原因（Downgraded=true 时非空）
	Reason string
}

// Resolve 宽松解析：请求值合法即采用；否则按模块默认 → 兜底。
//
// ★ 与 ParseCaliber 的分工（与前端 resolveCaliber 对称）：
//
//	用户**主动切**口径 → ParseCaliber（非法必须报错）；
//	页面**首次加载** → Resolve（URL 可能没带/带脏值，用默认并如实报告）。
//
// 两条路径都必须把「最终用了哪个口径」暴露给调用方，绝不静默。
func Resolve(requested, module string) Resolved {
	trimmed := strings.TrimSpace(requested)
	if c, err := ParseCaliber(trimmed); err == nil {
		return Resolved{Caliber: c, Source: "requested"}
	}
	if c, ok := DefaultByModule[module]; ok {
		reason := fmt.Sprintf("未指定口径，按模块「%s」的默认口径（%s，D10 分模块各自默认）", module, c)
		if trimmed != "" {
			reason = fmt.Sprintf("口径参数非法（%q），已回退到模块「%s」的默认口径（%s）", trimmed, module, c)
		}
		return Resolved{
			Caliber:    c,
			Source:     "module_default",
			Downgraded: trimmed != "",
			Reason:     reason,
		}
	}
	return Resolved{
		Caliber:    FallbackCaliber,
		Source:     "fallback",
		Downgraded: true,
		Reason:     fmt.Sprintf("模块「%s」未登记默认口径，已回退到保守口径（%s）", module, FallbackCaliber),
	}
}

// ───────────────────────────── ★ 不变量 ─────────────────────────────

// InvariantLines 两口径下**必须完全相同**的行。
//
// ★★★ 这是本包最重要的一条契约：
//
//	**净收入（net_revenue）在两口径下完全相同。**
//
//	原因不是巧合，是定义决定的（docs/03 §3.2）：
//	    algo.net_revenue = gross_listing − seller_disc − platform_disc
//	折扣本来就从 net_revenue 里扣掉了。两口径的区别只是把「折扣」在
//	报表上的**呈现位置**从费用侧搬到收入侧 —— 它是展示归属问题，
//	不是计算问题。
//
//	因此：口径切换**不得**改变 net_revenue。若变了，那不是「新口径」，
//	而是有人改错了一处公式 —— 必须当成 bug 拦截，而不是当成新口径放行。
var InvariantLines = []string{"gross_listing", "net_revenue"}

// InvariantViolation 一条不变量违规。
type InvariantViolation struct {
	Line string
	A    *float64
	B    *float64
	Note string
}

// CheckNetRevenueInvariant 校验「净收入两口径相同」。
//
// valuesA / valuesB 是两口径下各行（已由内核算出）的数值；
// nil 表示该行缺失（缺失语义：**两边都缺 = 一致**，一边缺一边有 = 违规）。
//
// 返回违规清单（空 = 合规）。本函数**不做算术**，只比较既有数值 ——
// 算术是内核的事，这里只判「它们该不该相等」。
func CheckNetRevenueInvariant(valuesA, valuesB map[string]*float64) []InvariantViolation {
	var out []InvariantViolation
	for _, line := range InvariantLines {
		a := lookupOpt(valuesA, line)
		b := lookupOpt(valuesB, line)
		if sameAmount(a, b) {
			continue
		}
		out = append(out, InvariantViolation{
			Line: line,
			A:    a,
			B:    b,
			Note: "该行在两口径下必须完全相同（折扣本就从净收入扣除，口径只改展示归属）" +
				"—— 不一致说明公式被改坏，而非新口径",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// LookupInvariantViolation 是 InvariantViolation 的零值安全取行名（便于测试可读）。
func (v InvariantViolation) LookupInvariantViolation() string { return v.Line }

const amountEps = 1e-6

func sameAmount(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		d := *a - *b
		if d < 0 {
			d = -d
		}
		return d <= amountEps
	}
}

func lookupOpt(m map[string]*float64, k string) *float64 {
	if m == nil {
		return nil
	}
	return m[k]
}

// ───────────────────────────── 口径切换审计 ─────────────────────────────

// CaliberChange 一次口径切换（append-only 落审计）。
//
// ★ 为什么要审计一个「展示选项」：
//
//	口径决定了同一份数据被读成「巨亏」还是「微利」。当两个部门拿着
//	两张不一致的截图争吵时，唯一能厘清的就是「谁在什么时候用什么口径出的」。
//	把切换记成审计事件，成本极低，但省掉的扯皮极多。
type CaliberChange struct {
	Account   string
	Module    string
	From      Caliber
	To        Caliber
	Requested string // 原始请求值（可能是脏值 —— 保留它才能解释为何回退）
	Source    string // requested | module_default | fallback
	Dirty     bool   // 请求值是否非法（被回退）
}

// AuditAction 审计动作名（M-ADMIN append-only 日志用）。
const AuditActionCaliberChange = "pnl.caliber.change"

// ChangeSummary 生成审计摘要（不含敏感信息；口径不是敏感信息，可明文）。
func (c CaliberChange) ChangeSummary() string {
	from := string(c.From)
	if from == "" {
		from = "(未设置)"
	}
	suffix := ""
	if c.Dirty {
		suffix = fmt.Sprintf("（请求值 %q 非法，按 %s 回退）", c.Requested, c.Source)
	}
	return fmt.Sprintf("口径切换 %s → %s @ %s%s", from, c.To, c.Module, suffix)
}

// Validate 校验审计记录完整性（缺模块则无法解释为何是默认口径）。
func (c CaliberChange) Validate() error {
	if c.Module == "" {
		return fmt.Errorf("pnl: CaliberChange.Module 不能为空（否则无法解释默认口径来源）")
	}
	if c.To == "" || !IsValidCaliber(c.To) {
		return fmt.Errorf("%w: To=%q", ErrInvalidCaliber, c.To)
	}
	if c.From != "" && !IsValidCaliber(c.From) {
		return fmt.Errorf("%w: From=%q", ErrInvalidCaliber, c.From)
	}
	return nil
}

// ───────────────────────────── 分层顺序 ─────────────────────────────

// LineIDs P&L 分层顺序（docs/02 M-PNL：收入→COGS→GP→费用→CM1→CM2→净贡献）。
//
// 顺序即展示顺序，是契约的一部分：财务看表的肌肉记忆很强，
// 行序变了会让人怀疑数据错了。
var LineIDs = []string{
	"gross_listing", "seller_discount", "platform_discount", "net_revenue",
	"cogs", "gp", "gmp", "marketing", "cm1", "platform_fee", "cm2",
	"overhead_alloc", "net_contrib",
}

// SectionOf 行的分组（收入 / 毛利 / 贡献段）。
func SectionOf(line string) string {
	switch line {
	case "gross_listing", "seller_discount", "platform_discount", "net_revenue":
		return "revenue"
	case "cogs", "gp", "gmp":
		return "gross"
	default:
		return "contribution"
	}
}

// CaliberMeta 口径元数据（供 API 下发，与前端契约 CaliberMeta 对齐）。
type CaliberMeta struct {
	ID                 Caliber `json:"id"`
	Short              string  `json:"short"`
	Label              string  `json:"label"`
	Definition         string  `json:"definition"`
	MisreadingRisk     string  `json:"misreadingRisk"`
	SellerDiscountRole string  `json:"sellerDiscountRole"`
}

// MetaFor 取口径元数据。
func MetaFor(c Caliber) CaliberMeta {
	if c == CaliberA {
		return CaliberMeta{
			ID:                 CaliberA,
			Short:              "口径 A",
			Label:              "A · 参考口径",
			Definition:         "Seller Discount 计入营销费用",
			MisreadingRisk:     MisreadingRisk(CaliberA),
			SellerDiscountRole: SellerDiscountRole(CaliberA),
		}
	}
	return CaliberMeta{
		ID:                 CaliberB,
		Short:              "口径 B",
		Label:              "B · Finance 口径",
		Definition:         "Seller Discount 作 contra-revenue（收入抵减）",
		MisreadingRisk:     MisreadingRisk(CaliberB),
		SellerDiscountRole: SellerDiscountRole(CaliberB),
	}
}

// AllMeta 全部口径元数据（有序：A, B）。
func AllMeta() []CaliberMeta {
	out := make([]CaliberMeta, 0, len(Calibers))
	for _, c := range Calibers {
		out = append(out, MetaFor(c))
	}
	return out
}
