// G1 · 契约镜像闸门（Go 镜像 ↔ contracts/*.ts 真源逐字段对平）。
//
// 动机（★ 真实缺口，同一个病的新形态）：
//
//	contracts/*.ts 是「唯一真源」；Go 侧有若干包注释里明写「与 contracts/X.ts 对齐」
//	的镜像结构体。但**此前没有任何东西把两侧的字段逐一对平**：
//	  * Go 侧只在 3 处硬编码版本号（QueryStateVersion / DataContractVersion /
//	    TenantContractVersion），而 tenant_test.go 的「版本一致性」比的是**字面量
//	    "1.0"**（不是真源文件）⇒ 真源升了版本，它照样绿。
//	  * web/test/contracts.test.mjs 只比「TS 真源 ↔ TS 镜像」（web/src/contracts），
//	    与 Go 侧无关。
//	⇒ 真源加一个字段、Go 镜像没跟（或反之），两侧静默错位、全程零报错。
//	  本闸门上线首刻即抓到两处真实漂移（见 KnownContractDrifts）。
//
// 实现方式：TS 侧做**文本级 interface 解析**（不是完整 TS 解析器），
// Go 侧用 reflect 取 json 标签名。取舍与 g1_layering.go 同：闸门要秒级跑完、
// 零外部依赖；漏报由 code review 兜底。
//
// 本闸门是**静态 / CI 闸门**（与 G1 分层依赖、SQL 静态闸门同性质）：
// 它没有运行时调用点 —— contracts/*.ts 不随二进制发布，运行时无从读取。
// 「有没有真的接上 CI」由 g1_contract_mirror_test.go 的接线断言钉住，
// 而不是靠一句注释里的承诺。
package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
)

// ContractMirrorPair 一对「TS 真源接口 ↔ Go 镜像结构体」。
//
// 只有**包注释里明写「与 contracts/X.ts 对齐」**的 Go 结构体才进这张表 ——
// 它是一份显式清单，不是靠命名猜测。要新增/移除对平范围，必须改本表并同步 docs/05。
type ContractMirrorPair struct {
	File      string       // 真源文件名，相对 contracts/（如 "query-state.ts"）
	Interface string       // TS 接口名
	GoType    reflect.Type // Go 镜像结构体类型
}

// ContractMirrorPairs 返回参与对平的镜像对（唯一权威表）。
func ContractMirrorPairs() []ContractMirrorPair {
	return []ContractMirrorPair{
		// ── query-state.ts：M-FILTER 的唯一产出（Go 镜像：backend/internal/contracts）
		{"query-state.ts", "QueryState", reflect.TypeOf(contracts.QueryState{})},
		{"query-state.ts", "TimeRange", reflect.TypeOf(contracts.TimeRange{})},
		{"query-state.ts", "FilterClause", reflect.TypeOf(contracts.FilterClause{})},
		{"query-state.ts", "DimSelection", reflect.TypeOf(contracts.DimSelection{})},
		{"query-state.ts", "OrderClause", reflect.TypeOf(contracts.OrderClause{})},
		{"query-state.ts", "PageClause", reflect.TypeOf(contracts.PageClause{})},
		{"query-state.ts", "PrecomputeHint", reflect.TypeOf(contracts.PrecomputeHint{})},

		// ── data-contract.ts：M-RENDER 的唯一入参
		{"data-contract.ts", "DataContract", reflect.TypeOf(contracts.DataContract{})},
		{"data-contract.ts", "ColumnDef", reflect.TypeOf(contracts.ColumnDef{})},
		{"data-contract.ts", "LevelSummary", reflect.TypeOf(contracts.LevelSummary{})},
		{"data-contract.ts", "AlgoTrace", reflect.TypeOf(contracts.AlgoTrace{})},
		{"data-contract.ts", "DataGap", reflect.TypeOf(contracts.DataGap{})},

		// ── entitlement.ts：授权求值结果（Go 镜像：backend/internal/authz）
		{"entitlement.ts", "Entitlement", reflect.TypeOf(authz.Entitlement{})},
		{"entitlement.ts", "GrantRecord", reflect.TypeOf(authz.GrantRecord{})},
		{"entitlement.ts", "EntitlementView", reflect.TypeOf(authz.EntitlementView{})},

		// ── permission-request.ts：抄送记录（Go 镜像：backend/internal/chain）
		{"permission-request.ts", "CcRecord", reflect.TypeOf(chain.CcRecord{})},
	}
}

// KnownContractDrift 已知、且**已登记**的字段漂移。
//
// 只允许放「已确认存在、但修法需业务拍板」的项；每条必须引用 docs/06 的待办编号。
// 新出现的漂移**不得**塞进这里 —— 那正是本闸门要拦的东西。
//
// 白名单最大的风险是「腐烂」：漂移被修好了，条目还留着，于是闸门对同类问题
// 永远闭嘴。故 StaleKnownContractDrifts 会反向核对「登记了但已不复现」的条目。
type KnownContractDrift struct {
	File        string
	Interface   string
	MissingInGo []string // 真源有、Go 镜像没有
	MissingInTS []string // Go 镜像有、真源没有
	DecisionID  string   // docs/06 的待办编号（如 "F17"）
	Reason      string   // 为什么不能直接改（而不是「懒得改」）
}

// KnownContractDrifts 返回已登记的已知漂移。
func KnownContractDrifts() []KnownContractDrift {
	return []KnownContractDrift{
		{
			File:        "entitlement.ts",
			Interface:   "Entitlement",
			MissingInGo: []string{"v"},
			DecisionID:  "F17",
			Reason: "真源把 `v: typeof ENTITLEMENT_VERSION` 声明为**必填**（契约版本位），" +
				"Go 镜像 authz.Entitlement 没有对应字段。修法有两条且结论相反：" +
				"① 给 Go 侧补 `V` 字段 —— 但全仓没有任何地方给 authz.Entitlement 赋版本值" +
				"（该结构只作服务端求值入参、不经 HTTP 出线），补了就是一个**结构性死字段**" +
				"（本仓反复踩的坑）；② 从真源删掉 `v` —— 那要先把契约版本位的语义定下来" +
				"（Entitlement 到底出不出线、出线时版本位谁负责填）。⇒ 交用户拍板。",
		},
		{
			File:        "permission-request.ts",
			Interface:   "CcRecord",
			MissingInGo: []string{"readAt"},
			DecisionID:  "F17",
			Reason: "真源声明 `readAt?`（抄送人已读时间），Go 镜像 chain.CcRecord 无此字段。" +
				"全仓（.go/.ts/.mjs/.sql）除该契约外**零引用** ⇒ 既无生产者也无消费者；" +
				"而同结构里的 `decidedAt` 恰恰相反 —— 它有明确的生产者（会签表态）与消费者" +
				"（0006 的 v_cosign_pending 视图），注释里还专门解释过为什么**不能** omitempty。" +
				"⇒ 补齐 `readAt` 会造出一个结构性死字段；删除它又要先确认产品是否需要" +
				"「已读未表态」的待办提醒（与 decidedAt 的语义边界）。交用户拍板。",
		},
	}
}

// ContractMirrorDiff 一对镜像的字段差异（结构化，便于与已知漂移表比对）。
type ContractMirrorDiff struct {
	File        string
	Interface   string
	GoTypeName  string
	MissingInGo []string // 真源有、Go 镜像没有
	MissingInTS []string // Go 镜像有、真源没有
}

// IsClean 判断该对镜像是否字段完全一致。
func (d ContractMirrorDiff) IsClean() bool {
	return len(d.MissingInGo) == 0 && len(d.MissingInTS) == 0
}

// String 人类可读的差异说明（空差异返回「一致」）。
func (d ContractMirrorDiff) String() string {
	if d.IsClean() {
		return fmt.Sprintf("%s:%s ↔ %s 一致", d.File, d.Interface, d.GoTypeName)
	}
	var parts []string
	if len(d.MissingInGo) > 0 {
		parts = append(parts, "真源有而 Go 缺 ["+strings.Join(d.MissingInGo, ", ")+"]")
	}
	if len(d.MissingInTS) > 0 {
		parts = append(parts, "Go 有而真源缺 ["+strings.Join(d.MissingInTS, ", ")+"]")
	}
	return fmt.Sprintf("%s:%s ↔ %s 漂移：%s", d.File, d.Interface, d.GoTypeName, strings.Join(parts, "；"))
}

// ───────────────────────────── TS 侧：文本级 interface 解析 ─────────────────────────────

// tsInterfaceRe 匹配 `export interface X {`（允许 `export interface X<T = Row> {`）。
var tsInterfaceRe = regexp.MustCompile(
	`(?m)^[ \t]*export[ \t]+interface[ \t]+([A-Za-z_$][A-Za-z0-9_$]*)[ \t]*(?:<[^>{}]*>)?[ \t]*\{`)

// tsFieldRe 匹配**顶层**字段行：`name:` 或 `name?:`。
var tsFieldRe = regexp.MustCompile(`^[ \t]*([A-Za-z_$][A-Za-z0-9_$]*)[ \t]*\??[ \t]*:`)

// ParseTSInterfaceFields 解析 TS 源里每个 `export interface` 的**顶层**字段名（已排序）。
//
// 规则（刻意保守）：
//   - 先剥注释（`//` 与 `/* */`），且字符串字面量里的 `//` 不算注释；
//   - 只认行首的 `ident` / `ident?` 紧跟 `:`，故联合类型续行（`| "today"`）不会被误收；
//   - 内联对象类型（`cosignTriggers: { ... }`）里的字段**不算**顶层字段（按花括号配平）；
//   - 解析不到 `export interface` 时返回空 map（不 panic、不猜）。
func ParseTSInterfaceFields(src string) map[string][]string {
	clean := stripTSComments(src)
	out := map[string][]string{}
	for _, loc := range tsInterfaceRe.FindAllStringSubmatchIndex(clean, -1) {
		name := clean[loc[2]:loc[3]]
		open := loc[1] - 1 // '{' 的下标
		body, ok := tsBalancedBlock(clean, open)
		if !ok {
			continue // 括号不配平（文件被截断？）—— 宁可漏报，不猜
		}
		out[name] = tsTopLevelFields(body)
	}
	return out
}

// stripTSComments 去掉 `//` 行注释与 `/* */` 块注释，**保留换行**（行号不错位）。
func stripTSComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	var quote byte
	inLine, inBlock := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
				continue
			}
			if c == '\n' {
				b.WriteByte(c)
			}
		case quote != 0:
			b.WriteByte(c)
			if c == '\\' && quote != '`' && i+1 < len(src) {
				i++
				b.WriteByte(src[i])
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine = true
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			i++
		case c == '"' || c == '\'' || c == '`':
			quote = c
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// tsBalancedBlock 返回 s[open] == '{' 起、与之配平的花括号**体内**文本。
func tsBalancedBlock(s string, open int) (string, bool) {
	depth := 0
	var quote byte
	for i := open; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote != '`' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[open+1 : i], true
			}
		}
	}
	return "", false
}

// tsTopLevelFields 逐行取花括号深度为 0 的字段名。
func tsTopLevelFields(body string) []string {
	var fields []string
	depth := 0
	for _, line := range strings.Split(body, "\n") {
		if depth == 0 {
			if m := tsFieldRe.FindStringSubmatch(line); m != nil {
				fields = append(fields, m[1])
			}
		}
		depth += braceDelta(line)
	}
	sort.Strings(fields)
	return fields
}

// braceDelta 统计一行里的净花括号数（忽略字符串字面量里的括号）。
func braceDelta(line string) int {
	d := 0
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == '\\' && quote != '`' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '{':
			d++
		case '}':
			d--
		}
	}
	return d
}

// ───────────────────────────── Go 侧：反射取 json 字段名 ─────────────────────────────

// GoJSONFieldNames 返回结构体的 json 字段名（已排序）。
//
// 规则与 encoding/json 一致：`json:"-"` 跳过；`json:"name,omitempty"` 取 `name`；
// 无 tag 时用字段名原文。未导出字段不参与序列化，故不计入。
func GoJSONFieldNames(t reflect.Type) []string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // 未导出
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ───────────────────────────── 对平 ─────────────────────────────

// DiffContractMirrorPair 计算一对镜像的字段差异（双向）。
func DiffContractMirrorPair(pair ContractMirrorPair, tsFields []string) ContractMirrorDiff {
	goFields := GoJSONFieldNames(pair.GoType)
	onlyTS, onlyGo := diffFieldSets(tsFields, goFields)
	name := pair.GoType.Name()
	return ContractMirrorDiff{
		File:        pair.File,
		Interface:   pair.Interface,
		GoTypeName:  name,
		MissingInGo: onlyTS,
		MissingInTS: onlyGo,
	}
}

// CheckContractMirrorDir 读取 dir 下的真源文件，逐对计算差异。
//
// 返回值：diffs = 每对镜像的差异（含「一致」的）；errs = 结构性错误
// （文件读不到 / interface 改名或删除）；err = 文件读取失败。
func CheckContractMirrorDir(dir string, pairs []ContractMirrorPair) ([]ContractMirrorDiff, []string, error) {
	srcs := map[string]string{}
	for _, p := range pairs {
		if _, ok := srcs[p.File]; ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, p.File))
		if err != nil {
			return nil, nil, fmt.Errorf("读取真源 %s: %w", p.File, err)
		}
		srcs[p.File] = string(b)
	}

	var diffs []ContractMirrorDiff
	var errs []string
	for _, p := range pairs {
		fields, ok := ParseTSInterfaceFields(srcs[p.File])[p.Interface]
		if !ok {
			errs = append(errs, fmt.Sprintf(
				"%s:%s 真源里找不到该 interface（改名 / 删除 / 解析失败？）—— "+
					"若是有意移除，请同步从 gate.ContractMirrorPairs() 删掉并更新 docs/05",
				p.File, p.Interface))
			continue
		}
		diffs = append(diffs, DiffContractMirrorPair(p, fields))
	}
	return diffs, errs, nil
}

// UnexplainedContractDrifts 从差异里剔除**已登记**的已知漂移，返回仍未解释的差异说明。
func UnexplainedContractDrifts(diffs []ContractMirrorDiff, known []KnownContractDrift) []string {
	var out []string
	for _, d := range diffs {
		if d.IsClean() {
			continue
		}
		k, ok := findKnownDrift(known, d.File, d.Interface)
		if ok && sameStrings(k.MissingInGo, d.MissingInGo) && sameStrings(k.MissingInTS, d.MissingInTS) {
			continue
		}
		out = append(out, d.String())
	}
	return out
}

// StaleKnownContractDrifts 返回「登记了、但当前已不复现」的已知漂移（防白名单腐烂）。
func StaleKnownContractDrifts(diffs []ContractMirrorDiff, known []KnownContractDrift) []string {
	var out []string
	for _, k := range known {
		var d *ContractMirrorDiff
		for i := range diffs {
			if diffs[i].File == k.File && diffs[i].Interface == k.Interface {
				d = &diffs[i]
				break
			}
		}
		if d == nil {
			out = append(out, fmt.Sprintf(
				"%s:%s 登记为已知漂移，但该镜像对不在 gate.ContractMirrorPairs() 里",
				k.File, k.Interface))
			continue
		}
		if !sameStrings(k.MissingInGo, d.MissingInGo) || !sameStrings(k.MissingInTS, d.MissingInTS) {
			out = append(out, fmt.Sprintf(
				"%s:%s 已知漂移表已过期：登记「真源有而 Go 缺 [%s] / Go 有而真源缺 [%s]」，"+
					"实际「真源有而 Go 缺 [%s] / Go 有而真源缺 [%s]」",
				k.File, k.Interface,
				strings.Join(k.MissingInGo, ", "), strings.Join(k.MissingInTS, ", "),
				strings.Join(d.MissingInGo, ", "), strings.Join(d.MissingInTS, ", ")))
		}
	}
	return out
}

// diffFieldSets 返回只在 a 有 / 只在 b 有的字段（均已排序）。
func diffFieldSets(a, b []string) (onlyA, onlyB []string) {
	bs := map[string]bool{}
	for _, x := range b {
		bs[x] = true
	}
	as := map[string]bool{}
	for _, x := range a {
		as[x] = true
	}
	for _, x := range a {
		if !bs[x] {
			onlyA = append(onlyA, x)
		}
	}
	for _, x := range b {
		if !as[x] {
			onlyB = append(onlyB, x)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return onlyA, onlyB
}

// findKnownDrift 按 (file, interface) 查已知漂移。
func findKnownDrift(known []KnownContractDrift, file, iface string) (KnownContractDrift, bool) {
	for _, k := range known {
		if k.File == file && k.Interface == iface {
			return k, true
		}
	}
	return KnownContractDrift{}, false
}

// sameStrings 判断两个字符串切片集合是否相同（顺序无关）。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
