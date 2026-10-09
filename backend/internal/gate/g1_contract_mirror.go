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
//
// ★ 第二轮补齐（2026-10-09）：对平范围从 16 对扩到 27 对，并补两件事 ——
//
//  1. **「声明即须对平」的覆盖闸门**：首轮只覆盖了「已经进表」的 16 对，
//     而**没进表**的契约照旧无人对平。实测发现 `template` / `pnl` / `strategy`
//     三个包各自在 `_test.go` 里用**硬编码字面量**断言「字段与 contracts/X.ts 一致」
//     （例：`template_test.go` 的 `required := []string{"id","name",...}`）——
//     测试的注释声称读契约，实际**从不打开那个文件** ⇒ 真源加一个字段，
//     它照样绿。这正是本仓反复踩的「假闸门」。现 `UncoveredContractFiles`
//     要求 `contracts/` 下**每个 `.ts`** 要么有字段级镜像对，要么在
//     `UnmirroredContracts()` 里显式声明理由（且声明不得腐烂）。
//  2. **有意的差异必须显式声明**：对平是双向的，Go 多一个字段同样算漂移，
//     但有些扩展是有意的（`template.Template` 的 `v` / `shares`、
//     `strategy.Choice` 把契约的 `current` 对象收成 `currentKey`）。
//     故 `ExtraGoFields` / `FieldAliases` 必须逐条登记，且**登记了就必须真的存在**
//     （防白名单腐烂）；未登记的多余字段照旧报漂移。
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
	"github.com/miccchwang/spark-cicada/backend/internal/pnl"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
	"github.com/miccchwang/spark-cicada/backend/internal/strategy"
	"github.com/miccchwang/spark-cicada/backend/internal/template"
	"github.com/miccchwang/spark-cicada/backend/internal/tenant"
)

// ContractMirrorPair 一对「TS 真源接口 ↔ Go 镜像结构体」。
//
// 只有**包注释里明写「与 contracts/X.ts 对齐」**的 Go 结构体才进这张表 ——
// 它是一份显式清单，不是靠命名猜测。要新增/移除对平范围，必须改本表并同步 docs/05。
type ContractMirrorPair struct {
	File      string       // 真源文件名，相对 contracts/（如 "query-state.ts"）
	Interface string       // TS 接口名
	GoType    reflect.Type // Go 镜像结构体类型

	// ExtraGoFields 是**有意**比真源多的 Go 字段（json 名）。
	//
	// 为什么必须显式声明：对平是**双向**的，Go 多一个字段同样算漂移；
	// 而有些扩展是有意的（`template.Template` 比契约多 `v`（版本位）与
	// `shares`（团队档可见对象），两者都有明确生产者/消费者）。
	// 声明后仍会被校验「真的存在」，故它是一份**不得腐烂**的白名单。
	ExtraGoFields []string

	// FieldAliases 是**有意**改名的字段映射：真源字段名 → Go json 字段名。
	//
	// 例：契约 `StrategyChoice.current` 是整个选项对象，Go 侧只存
	// `currentKey`（选项 key）—— 存两份必然漂移。改名是有意的，但必须登记，
	// 且映射**两侧都要真实存在**（否则是死映射，同样报错）。
	FieldAliases map[string]string
}

// mirrorPair 构造一对「字段应完全一致」的镜像。
func mirrorPair(file, iface string, t reflect.Type) ContractMirrorPair {
	return ContractMirrorPair{File: file, Interface: iface, GoType: t}
}

// mirrorPairExt 构造一对**有已声明差异**的镜像（见 ExtraGoFields / FieldAliases）。
func mirrorPairExt(file, iface string, t reflect.Type, extra []string, aliases map[string]string) ContractMirrorPair {
	return ContractMirrorPair{
		File: file, Interface: iface, GoType: t,
		ExtraGoFields: extra, FieldAliases: aliases,
	}
}

// ContractMirrorPairs 返回参与对平的镜像对（唯一权威表）。
func ContractMirrorPairs() []ContractMirrorPair {
	return []ContractMirrorPair{
		// ── query-state.ts：M-FILTER 的唯一产出（Go 镜像：backend/internal/contracts）
		mirrorPair("query-state.ts", "QueryState", reflect.TypeOf(contracts.QueryState{})),
		mirrorPair("query-state.ts", "TimeRange", reflect.TypeOf(contracts.TimeRange{})),
		mirrorPair("query-state.ts", "FilterClause", reflect.TypeOf(contracts.FilterClause{})),
		mirrorPair("query-state.ts", "DimSelection", reflect.TypeOf(contracts.DimSelection{})),
		mirrorPair("query-state.ts", "OrderClause", reflect.TypeOf(contracts.OrderClause{})),
		mirrorPair("query-state.ts", "PageClause", reflect.TypeOf(contracts.PageClause{})),
		mirrorPair("query-state.ts", "PrecomputeHint", reflect.TypeOf(contracts.PrecomputeHint{})),

		// ── data-contract.ts：M-RENDER 的唯一入参
		mirrorPair("data-contract.ts", "DataContract", reflect.TypeOf(contracts.DataContract{})),
		mirrorPair("data-contract.ts", "ColumnDef", reflect.TypeOf(contracts.ColumnDef{})),
		mirrorPair("data-contract.ts", "LevelSummary", reflect.TypeOf(contracts.LevelSummary{})),
		mirrorPair("data-contract.ts", "AlgoTrace", reflect.TypeOf(contracts.AlgoTrace{})),
		mirrorPair("data-contract.ts", "DataGap", reflect.TypeOf(contracts.DataGap{})),

		// ── entitlement.ts：授权求值结果（Go 镜像：backend/internal/authz）
		mirrorPair("entitlement.ts", "Entitlement", reflect.TypeOf(authz.Entitlement{})),
		mirrorPair("entitlement.ts", "GrantRecord", reflect.TypeOf(authz.GrantRecord{})),
		mirrorPair("entitlement.ts", "EntitlementView", reflect.TypeOf(authz.EntitlementView{})),

		// ── permission-request.ts：抄送记录与审批步骤
		//    （Go 镜像：backend/internal/chain 与 backend/internal/req）
		mirrorPair("permission-request.ts", "CcRecord", reflect.TypeOf(chain.CcRecord{})),
		// ApprovalStep 会被序列化进 fact_permission_request.approvals 这个 jsonb 列，
		// 前端按契约 camelCase 读取；req.go 的注释已明写「JSON tag 必须逐字一致」，
		// 但此前**没有任何东西真的去读契约**（只有一句注释）。
		mirrorPair("permission-request.ts", "ApprovalStep", reflect.TypeOf(req.ApprovalStep{})),

		// ── view-template.ts：M-TEMPLATE（Go 镜像：backend/internal/template）
		//
		// ★ 本契约此前由 template_test.go 用**硬编码字段名列表**断言「与契约一致」
		//   （测试从不打开契约文件）⇒ 真源加字段不会变红。现由本表真读真对平。
		mirrorPairExt("view-template.ts", "ViewTemplate", reflect.TypeOf(template.Template{}),
			// Go 侧有意多出的两个字段（均有明确生产者/消费者，非死字段）：
			//   * `v`      —— 契约版本位（template.Version），随模板一并下发；
			//   * `shares` —— 团队档的显式可见对象（契约里未建模，属本仓扩展）。
			[]string{"v", "shares"}, nil),
		mirrorPair("view-template.ts", "ColumnPref", reflect.TypeOf(template.ColumnPref{})),
		mirrorPair("view-template.ts", "LayoutPref", reflect.TypeOf(template.LayoutPref{})),

		// ── strategy-choice.ts：M-STRATEGY 选型卡（Go 镜像：backend/internal/strategy）
		mirrorPairExt("strategy-choice.ts", "StrategyChoice", reflect.TypeOf(strategy.Choice{}),
			nil,
			// 契约的 `current` 是**整个选项对象**；Go 侧只存 `currentKey`（选项 key），
			// 由 key 推出对象 —— 存两份必然漂移（改了 options 忘了改 current）。
			map[string]string{"current": "currentKey"}),
		mirrorPair("strategy-choice.ts", "ChoiceOption", reflect.TypeOf(strategy.Option{})),
		mirrorPair("strategy-choice.ts", "ImpactPreview", reflect.TypeOf(strategy.ImpactPreview{})),

		// ── pnl.ts：口径元数据（Go 镜像：backend/internal/pnl）
		//    此前 pnl_test.go 用字面量比对 `sellerDiscountRole` 等值，同样不读真源。
		mirrorPair("pnl.ts", "CaliberMeta", reflect.TypeOf(pnl.CaliberMeta{})),

		// ── tenant.ts：多租户（Go 镜像：backend/internal/tenant）
		//    此前只有版本号对齐（tenant_test.go 比的是字面量 "1.0"），字段从未对平。
		mirrorPair("tenant.ts", "Tenant", reflect.TypeOf(tenant.Tenant{})),
		mirrorPair("tenant.ts", "TenantQuota", reflect.TypeOf(tenant.TenantQuota{})),
		mirrorPair("tenant.ts", "TenantContext", reflect.TypeOf(tenant.TenantContext{})),
	}
}

// UnmirroredContract 声明「本契约文件**有意**没有字段级 Go 镜像」及其理由。
//
// 为什么需要它：`contracts/` 下的每个 `.ts` 都是「唯一真源」，但并非每个
// 真源都能做**字段级 JSON 对平** —— 有的 Go 对应物是内部领域类型（无 json 标签、
// 不经 HTTP 出线），有的所在包**反向依赖 gate**（import 环使 gate 无法反射它），
// 有的对应物是未导出类型。若不显式声明，「无人对平」与「有意不对平」就分不开，
// 于是「新加一个契约文件却没人管」会再次静默发生。
//
// 纪律：声明必须给理由；且**声明不得腐烂** —— 一旦该文件真的进了镜像表
// （或文件被删除），`UncoveredContractFiles` 会反向报错。
type UnmirroredContract struct {
	File   string
	Reason string
}

// UnmirroredContracts 返回「有意无字段级镜像」的契约文件（唯一权威表）。
func UnmirroredContracts() []UnmirroredContract {
	return []UnmirroredContract{
		{
			File: "backup-quota.ts",
			Reason: "Go 对应物是**内部领域类型且无 json 标签**：`dr.Region` / `dr.Archive` / " +
				"`dr.RollbackPolicy` 都只在进程内流转（出线形态是 `api` 包未导出的 " +
				"`downloadView`，且它把 BackupArchive 与 DownloadCheckResult **压平合并**、" +
				"字段名也不同）。更硬的约束是**包依赖方向**：`internal/dr` 反向 import `gate`" +
				"（它要调 `gate.QuotaKey` / 各判定函数）⇒ gate 无法 import dr，反射对平在" +
				"结构上不可能。故本文件只做**常量级**对平（见 dr.go 的 Reason* 常量与 " +
				"DEFAULT_ROLLBACK_POLICY 注释），字段级对平待「出线视图导出 / 抽独立契约包」后再接。" +
				"（待拍板项见 docs/06 F19）",
		},
		{
			File: "data-chain.ts",
			Reason: "Go 对应物 `chain.OrgNode` / `DataChain` / `DataChainNode` / `ApprovalChain` / " +
				"`CrossDeptPolicy` 是**内部领域类型**：全仓除 `chain.CcRecord` 外，chain 包" +
				"**没有任何 json 标签**（它不经 HTTP 出线，出线形态由 api 层组装）。" +
				"字段级 JSON 对平对它们不适用；且字段口径本身仍受 F9/F10 决策约束。" +
				"（待拍板项见 docs/06 F19）",
		},
		{
			File: "user-group.ts",
			Reason: "Go 对应物 `group.Group` / `group.Grant` **无 json 标签**（内部领域类型）；" +
				"真正的落库形态 `groupstore.groupGrantsJSON` 是**未导出**类型且**有意多一个 " +
				"`isIT` 字段**（0006 的 D7 约束要用它判身份，刻意不用组名猜）。" +
				"未导出类型无法被 gate 反射，故只保留「落库形状 ↔ 契约」的注释级对齐。" +
				"（待拍板项见 docs/06 F19）",
		},
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
//
// 已声明的**有意差异**（ExtraGoFields / FieldAliases）在比较前先归一化，
// 故「有意的扩展」不会被误报为漂移；但**未声明**的多余字段照旧报漂移。
func DiffContractMirrorPair(pair ContractMirrorPair, tsFields []string) ContractMirrorDiff {
	goFields := GoJSONFieldNames(pair.GoType)
	// 改名：真源名 → Go 名（映射本身是否「死」由 ContractMirrorPairIssues 单独报）。
	effTS := aliasTSFields(tsFields, pair.FieldAliases)
	onlyTS, onlyGo := diffFieldSets(effTS, goFields)
	// 剔除已声明的 Go-only 扩展（声明了却不存在的情形同样单独报，不在此静默吞掉）。
	onlyGo = removeDeclaredExtras(onlyGo, pair.ExtraGoFields)
	name := pair.GoType.Name()
	return ContractMirrorDiff{
		File:        pair.File,
		Interface:   pair.Interface,
		GoTypeName:  name,
		MissingInGo: onlyTS,
		MissingInTS: onlyGo,
	}
}

// aliasTSFields 按映射把真源字段名替换为 Go 名（未命中的字段原样保留）。
func aliasTSFields(tsFields []string, aliases map[string]string) []string {
	if len(aliases) == 0 {
		return tsFields
	}
	out := make([]string, 0, len(tsFields))
	for _, f := range tsFields {
		if to, ok := aliases[f]; ok {
			out = append(out, to)
			continue
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// removeDeclaredExtras 从「Go 有而真源缺」里剔除已声明的扩展字段。
func removeDeclaredExtras(onlyGo, declared []string) []string {
	if len(onlyGo) == 0 || len(declared) == 0 {
		return onlyGo
	}
	set := map[string]bool{}
	for _, d := range declared {
		set[d] = true
	}
	out := make([]string, 0, len(onlyGo))
	for _, f := range onlyGo {
		if !set[f] {
			out = append(out, f)
		}
	}
	return out
}

// ContractMirrorPairIssues 校验一对镜像的**已声明差异**本身是否成立。
//
// 存在意义：`ExtraGoFields` / `FieldAliases` 是白名单，白名单最大的风险是**腐烂** ——
//
//	① 映射/扩展登记了，但字段早已改名或删除 ⇒ 白名单还在替一个不存在的差异背书；
//	② 把「真源与 Go 都有的字段」误登记成「Go 独有扩展」⇒ 等于偷偷放宽了对平。
//
// 故：映射两侧都必须真实存在；扩展必须真的只在 Go 侧存在。
func ContractMirrorPairIssues(pair ContractMirrorPair, tsFields []string) []string {
	var out []string
	tsSet := map[string]bool{}
	for _, f := range tsFields {
		tsSet[f] = true
	}
	goSet := map[string]bool{}
	for _, f := range GoJSONFieldNames(pair.GoType) {
		goSet[f] = true
	}
	label := fmt.Sprintf("%s:%s", pair.File, pair.Interface)

	// 字段名排序，保证报错信息稳定（map 迭代无序）。
	keys := make([]string, 0, len(pair.FieldAliases))
	for k := range pair.FieldAliases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, from := range keys {
		to := pair.FieldAliases[from]
		if !tsSet[from] {
			out = append(out, fmt.Sprintf(
				"%s 的字段改名映射源 %q 在真源里不存在（契约已改名/删除？）", label, from))
		}
		if !goSet[to] {
			out = append(out, fmt.Sprintf(
				"%s 的字段改名映射目标 %q 在 Go 镜像里不存在（Go 侧已改名/删除？）", label, to))
		}
	}

	extras := append([]string(nil), pair.ExtraGoFields...)
	sort.Strings(extras)
	for _, e := range extras {
		if !goSet[e] {
			out = append(out, fmt.Sprintf(
				"%s 声明 %q 为 Go 独有扩展，但 Go 镜像里没有该字段（白名单腐烂）", label, e))
		}
		if tsSet[e] {
			out = append(out, fmt.Sprintf(
				"%s 把 %q 登记为 Go 独有扩展，但真源里也有该字段 —— "+
					"这等于偷偷放宽了对平（两侧共有字段不得登记为扩展）", label, e))
		}
	}
	return out
}

// UncoveredContractFiles 检查 `contracts/` 下的 `.ts` 是否**每一个都有人管**。
//
// 返回：uncovered = 既无镜像对、也未声明的文件；stale = 已声明却（被覆盖 / 文件不存在）的声明。
//
// 动机（★ 真实缺口）：首轮 G1 只对平「已经进表」的 16 对，而**没进表**的契约
// 照旧无人对平 —— 实测 `template` / `pnl` / `strategy` 三处都在 `_test.go` 里用
// **硬编码字面量**冒充「与契约一致」。闸门若只覆盖已知范围，「新加一个契约文件
// 却没人管」这件事会永远静默。故把「每个真源都要有归宿」本身变成一条断言。
func UncoveredContractFiles(dir string, pairs []ContractMirrorPair, unmirrored []UnmirroredContract) (uncovered, stale []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 contracts 目录 %s: %w", dir, err)
	}
	covered := map[string]bool{}
	for _, p := range pairs {
		covered[p.File] = true
	}
	declared := map[string]bool{}
	for _, u := range unmirrored {
		declared[u.File] = true
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	for _, f := range files {
		if covered[f] || declared[f] {
			continue
		}
		uncovered = append(uncovered, fmt.Sprintf(
			"%s 既没有字段级镜像对、也没有在 UnmirroredContracts() 里声明 —— "+
				"该契约的真源与实现之间**无人对平**", f))
	}

	for _, u := range unmirrored {
		if _, statErr := os.Stat(filepath.Join(dir, u.File)); statErr != nil {
			stale = append(stale, fmt.Sprintf(
				"%s 被声明为「无字段级镜像」，但该文件在 contracts/ 下不存在（声明腐烂）", u.File))
			continue
		}
		if covered[u.File] {
			stale = append(stale, fmt.Sprintf(
				"%s 被声明为「无字段级镜像」，但它已有镜像对 —— "+
					"请从 UnmirroredContracts() 删掉该声明", u.File))
		}
		if strings.TrimSpace(u.Reason) == "" {
			stale = append(stale, fmt.Sprintf(
				"%s 的「无镜像」声明缺少理由 —— 无理由的声明等于把缺口藏起来", u.File))
		}
	}
	sort.Strings(uncovered)
	sort.Strings(stale)
	return uncovered, stale, nil
}

// CheckContractMirrorDir 读取 dir 下的真源文件，逐对计算差异。
//
// 返回值：diffs = 每对镜像的差异（含「一致」的）；errs = 结构性错误
// （文件读不到 / interface 改名或删除 / 已声明差异本身不成立）；err = 文件读取失败。
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
		errs = append(errs, ContractMirrorPairIssues(p, fields)...)
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
