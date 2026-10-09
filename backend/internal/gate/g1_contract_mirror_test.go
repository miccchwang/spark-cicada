// g1_contract_mirror_test.go —— G1 契约镜像闸门的编排、自测与接线断言。
//
// 本文件回答三个问题（缺一即假闸门）：
//  1. **真源与 Go 镜像现在一致吗？** —— TestG1ContractMirror_RealContractsMatchGo
//  2. **这条闸门真的会响吗？** —— 解析器自测 + 双向漂移负向自测 + 白名单腐烂自测
//  3. **它真的在 CI 里跑吗？** —— 接线断言（不是注释里的承诺）
package gate_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// contractDir 返回仓库根下的 contracts/ 目录（测试工作目录 = backend/internal/gate）。
func contractDir() string {
	return filepath.Join("..", "..", "..", "contracts")
}

// ───────────────────────────── 1. 真源 ↔ Go 镜像 ─────────────────────────────

func TestG1ContractMirror_RealContractsMatchGo(t *testing.T) {
	diffs, errs, err := gate.CheckContractMirrorDir(contractDir(), gate.ContractMirrorPairs())
	if err != nil {
		t.Fatalf("读取 contracts/ 失败: %v", err)
	}
	if len(errs) > 0 {
		t.Fatalf("契约镜像结构性错误：\n  %s", strings.Join(errs, "\n  "))
	}
	// 夹具自证：一条差异都没算出来，说明闸门根本没跑到（例如 pair 表被清空）。
	if len(diffs) == 0 {
		t.Fatal("没有对平出任何一对镜像 —— 闸门等于没跑")
	}

	if bad := gate.UnexplainedContractDrifts(diffs, gate.KnownContractDrifts()); len(bad) > 0 {
		t.Fatalf("真源与 Go 镜像字段漂移（未登记）：\n  %s\n\n"+
			"修法：改 Go 镜像结构体（backend/internal/...）或改真源 contracts/*.ts；"+
			"确需保留分叉的，必须写进 gate.KnownContractDrifts 并在 docs/06 登记决策项。",
			strings.Join(bad, "\n  "))
	}
	if stale := gate.StaleKnownContractDrifts(diffs, gate.KnownContractDrifts()); len(stale) > 0 {
		t.Fatalf("已知漂移表已过期（登记了但当前并不复现）：\n  %s\n\n"+
			"修法：漂移已修复 ⇒ 从 gate.KnownContractDrifts 删掉该条，并更新 docs/06。",
			strings.Join(stale, "\n  "))
	}
}

// TestG1ContractMirror_PairsTableIsWellFormed 钉住 pair 表本身没被写坏。
func TestG1ContractMirror_PairsTableIsWellFormed(t *testing.T) {
	pairs := gate.ContractMirrorPairs()
	// 2026-10-09 第二轮：16 → 27 对。数字本身是「有人删了对平范围」的兜底告警，
	// 真正的守卫是 TestG1ContractMirror_AllContractFilesAccountedFor（每个真源都要有归宿）。
	if len(pairs) < 27 {
		t.Fatalf("镜像对只有 %d 条（期望 ≥27）—— 有人删了对平范围？"+
			"删范围必须先改 docs/05 并说明理由", len(pairs))
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		if p.File == "" || p.Interface == "" {
			t.Errorf("镜像对缺少文件名或接口名: %+v", p)
		}
		if !strings.HasSuffix(p.File, ".ts") {
			t.Errorf("真源文件名 %q 不是 .ts", p.File)
		}
		if p.GoType == nil || p.GoType.Kind() != reflect.Struct {
			t.Errorf("%s:%s 的 GoType 不是结构体: %v", p.File, p.Interface, p.GoType)
		}
		key := p.File + ":" + p.Interface
		if seen[key] {
			t.Errorf("镜像对重复登记: %s", key)
		}
		seen[key] = true
	}
}

// ───────────────────────────── 2. 解析器自测 ─────────────────────────────

func TestG1ContractMirror_Parser(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		iface string
		want  []string
	}{
		{
			name:  "顶层字段与可选标记",
			src:   "export interface Foo {\n  a: string;\n  b?: number;\n}\n",
			iface: "Foo",
			want:  []string{"a", "b"},
		},
		{
			name: "内联对象类型里的字段不算顶层字段",
			src: "export interface CcPolicy {\n" +
				"  defaultMode: \"notify\" | \"cosign\";\n" +
				"  cosignTriggers: {\n" +
				"    level?: \"L4\";\n" +
				"    crossDept?: boolean;\n" +
				"  };\n" +
				"  emptyCcBehavior: \"notify\" | \"skip\";\n" +
				"}\n",
			iface: "CcPolicy",
			want:  []string{"cosignTriggers", "defaultMode", "emptyCcBehavior"},
		},
		{
			name: "注释里的字段名不算字段（含行尾注释）",
			src: "export interface Foo {\n" +
				"  // hidden: string;\n" +
				"  /* alsoHidden: number; */\n" +
				"  a: string; // trailing: string;\n" +
				"}\n",
			iface: "Foo",
			want:  []string{"a"},
		},
		{
			name: "注释里的花括号不影响配平（去注释必须真的生效）",
			src: "export interface Foo {\n" +
				"  a: string; // note: { unbalanced\n" +
				"  b: string; /* } */\n" +
				"  c: string;\n" +
				"}\n",
			iface: "Foo",
			want:  []string{"a", "b", "c"},
		},
		{
			name: "联合类型续行不被误收",
			src: "export interface Foo {\n" +
				"  preset?:\n" +
				"    | \"today\"\n" +
				"    | \"last7d\";\n" +
				"  grain: string;\n" +
				"}\n",
			iface: "Foo",
			want:  []string{"grain", "preset"},
		},
		{
			name: "泛型参数与字符串字面量里的花括号",
			src: "export interface DataContract<T = Row> {\n" +
				"  v: typeof DATA_CONTRACT_VERSION;\n" +
				"  note: \"}\";\n" +
				"  rows: T[];\n" +
				"}\n",
			iface: "DataContract",
			want:  []string{"note", "rows", "v"},
		},
		{
			name:  "解析不到 export interface 时返回空（不猜）",
			src:   "export type X = { a: string };\n",
			iface: "X",
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gate.ParseTSInterfaceFields(tc.src)[tc.iface]
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("解析结果不符\n  期望: %v\n  实际: %v", tc.want, got)
			}
		})
	}
}

// mirrorFixture 用于验证 Go 侧取名的边界行为（无 tag / `-` / 未导出）。
type mirrorFixture struct {
	Keep   string `json:"keep"`
	Dash   string `json:"-"`
	NoTag  string
	hidden string //nolint:unused // 未导出字段不应出现在 json 字段名里
}

func TestG1ContractMirror_GoJSONFieldNames(t *testing.T) {
	got := gate.GoJSONFieldNames(reflect.TypeOf(mirrorFixture{}))
	// `-` 与未导出字段必须被排除；无 tag 时用字段名原文（与 encoding/json 一致）。
	// 故 `Keep` 出线为 `keep`（有 tag），`NoTag` 出线为 `NoTag`（无 tag）。
	want := []string{"NoTag", "keep"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("GoJSONFieldNames 不符\n  期望: %v\n  实际: %v", want, got)
	}
}

// ───────────────────────────── 2b. 双向漂移负向自测 ─────────────────────────────

func TestG1ContractMirror_DetectsDriftBothDirections(t *testing.T) {
	// contracts.OrderClause 的 Go json 字段为 field / dir。
	pair := gate.ContractMirrorPair{
		File: "x.ts", Interface: "X", GoType: reflect.TypeOf(contracts.OrderClause{}),
	}

	// 夹具自证：一致的输入**必须**被判为一致 —— 否则闸门成了「永远报错」。
	if d := gate.DiffContractMirrorPair(pair, []string{"field", "dir"}); !d.IsClean() {
		t.Fatalf("一致用例被判为漂移（夹具/实现有误）：%s", d)
	}

	// 方向一：真源多一个字段 ⇒ 必须报「真源有而 Go 缺」。
	d := gate.DiffContractMirrorPair(pair, []string{"field", "dir", "extra"})
	if strings.Join(d.MissingInGo, ",") != "extra" || len(d.MissingInTS) != 0 {
		t.Fatalf("真源侧漂移未被正确报出: %+v", d)
	}

	// 方向二：Go 多一个字段 ⇒ 必须报「Go 有而真源缺」。
	d = gate.DiffContractMirrorPair(pair, []string{"field"})
	if strings.Join(d.MissingInTS, ",") != "dir" || len(d.MissingInGo) != 0 {
		t.Fatalf("Go 侧漂移未被正确报出: %+v", d)
	}
}

func TestG1ContractMirror_UnexplainedAndStale(t *testing.T) {
	diffs := []gate.ContractMirrorDiff{
		{File: "a.ts", Interface: "A", GoTypeName: "A", MissingInGo: []string{"x"}},
		{File: "b.ts", Interface: "B", GoTypeName: "B"}, // 一致
	}
	// 未登记 ⇒ 报为未解释。
	if got := gate.UnexplainedContractDrifts(diffs, nil); len(got) != 1 {
		t.Fatalf("未登记的漂移应被报出，得到 %v", got)
	}
	// 登记且完全匹配 ⇒ 放行。
	ok := []gate.KnownContractDrift{{File: "a.ts", Interface: "A", MissingInGo: []string{"x"}, DecisionID: "F99"}}
	if got := gate.UnexplainedContractDrifts(diffs, ok); len(got) != 0 {
		t.Fatalf("已登记的漂移应放行，得到 %v", got)
	}
	if got := gate.StaleKnownContractDrifts(diffs, ok); len(got) != 0 {
		t.Fatalf("登记项与实际一致时不应报过期，得到 %v", got)
	}
	// 登记项与实际不符 ⇒ 报过期（防白名单腐烂）。
	driftChanged := []gate.KnownContractDrift{{File: "a.ts", Interface: "A", MissingInGo: []string{"z"}, DecisionID: "F99"}}
	if got := gate.StaleKnownContractDrifts(diffs, driftChanged); len(got) != 1 {
		t.Fatalf("登记项与实际不符时应报过期，得到 %v", got)
	}
	// 登记了根本不在对平表里的镜像对 ⇒ 报过期。
	ghost := []gate.KnownContractDrift{{File: "zzz.ts", Interface: "Z", MissingInGo: []string{"x"}, DecisionID: "F99"}}
	if got := gate.StaleKnownContractDrifts(diffs, ghost); len(got) != 1 {
		t.Fatalf("幽灵登记项应报过期，得到 %v", got)
	}
}

// ───────────────────────────── 2c. 已知漂移必须已登记在 docs/06 ─────────────────────────────

func TestG1ContractMirror_KnownDriftsRegisteredInDocs(t *testing.T) {
	decisions := repoFile(t, "docs/06-决策清单.md")
	for _, k := range gate.KnownContractDrifts() {
		if k.DecisionID == "" {
			t.Errorf("%s:%s 已知漂移缺少 DecisionID（必须指向 docs/06 的待办项）", k.File, k.Interface)
		}
		if !strings.Contains(decisions, k.DecisionID) {
			t.Errorf("%s:%s 的 DecisionID %q 在 docs/06 里找不到 —— "+
				"已知漂移必须登记成一条待拍板项，不能只写在代码白名单里", k.File, k.Interface, k.DecisionID)
		}
		if strings.TrimSpace(k.Reason) == "" {
			t.Errorf("%s:%s 已知漂移缺少 Reason —— 白名单必须有可审计的理由", k.File, k.Interface)
		}
		if len(k.MissingInGo) == 0 && len(k.MissingInTS) == 0 {
			t.Errorf("%s:%s 已知漂移没有登记任何差异字段", k.File, k.Interface)
		}
	}
}

// ───────────── 2d. 「声明即须对平」：每个真源都要有归宿（2026-10-09 第二轮） ─────────────

// TestG1ContractMirror_AllContractFilesAccountedFor 断言 contracts/ 下**每个** .ts
// 要么有字段级镜像对、要么被显式声明为「有意无镜像」。
//
// ★ 为什么这条断言才是真守卫：首轮只对平「已经进表」的 16 对，
// 而**没进表**的契约照旧无人对平 —— 实测 template / pnl / strategy 三处
// 都在 `_test.go` 里用**硬编码字面量**冒充「与契约一致」（测试从不打开契约文件）。
// 只把已知的补进表，下次新加一个契约文件仍然会静默无人管。
func TestG1ContractMirror_AllContractFilesAccountedFor(t *testing.T) {
	uncovered, stale, err := gate.UncoveredContractFiles(
		contractDir(), gate.ContractMirrorPairs(), gate.UnmirroredContracts())
	if err != nil {
		t.Fatalf("枚举 contracts/ 失败: %v", err)
	}
	if len(uncovered) > 0 {
		t.Fatalf("以下契约文件无人对平（既无镜像对、也未声明）：\n  %s\n\n"+
			"修法：① 加进 gate.ContractMirrorPairs()（推荐，真读真对平）；"+
			"② 确无字段级镜像可做时，加进 gate.UnmirroredContracts() 并写明理由。",
			strings.Join(uncovered, "\n  "))
	}
	if len(stale) > 0 {
		t.Fatalf("「无镜像」声明已过期：\n  %s", strings.Join(stale, "\n  "))
	}
}

// TestG1ContractMirror_AllContractFiles_NegativeSelfCheck 是上一条的负向自测。
//
// 目录闸门最大的失效模式是「写了却永远为真」。这里喂进**故意残缺**的目录，
// 断言它真的会判「无人管」—— 而不是因为实现写错而假绿。
func TestG1ContractMirror_AllContractFiles_NegativeSelfCheck(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("写夹具 %s: %v", name, err)
		}
	}
	write("covered.ts", "export interface A {\n  a: string;\n}\n")
	write("declared.ts", "export interface B {\n  b: string;\n}\n")
	write("orphan.ts", "export interface C {\n  c: string;\n}\n")

	pairs := []gate.ContractMirrorPair{
		{File: "covered.ts", Interface: "A", GoType: reflect.TypeOf(contracts.OrderClause{})},
	}
	// 1) 有镜像对 + 已声明 ⇒ 只有 orphan.ts 该被报出来。
	uncovered, stale, err := gate.UncoveredContractFiles(dir, pairs, []gate.UnmirroredContract{
		{File: "declared.ts", Reason: "夹具：有意无镜像"},
	})
	if err != nil {
		t.Fatalf("枚举失败: %v", err)
	}
	if len(uncovered) != 1 || !strings.Contains(uncovered[0], "orphan.ts") {
		t.Fatalf("应只报出 orphan.ts，得到 %v", uncovered)
	}
	if len(stale) != 0 {
		t.Fatalf("不该报过期声明，得到 %v", stale)
	}

	// 2) 声明了却已有镜像对 ⇒ 必须报过期（防白名单腐烂）。
	_, stale, err = gate.UncoveredContractFiles(dir, pairs, []gate.UnmirroredContract{
		{File: "declared.ts", Reason: "r"},
		{File: "covered.ts", Reason: "r"},
	})
	if err != nil {
		t.Fatalf("枚举失败: %v", err)
	}
	if len(stale) != 1 || !strings.Contains(stale[0], "covered.ts") {
		t.Fatalf("应报出 covered.ts 的过期声明，得到 %v", stale)
	}

	// 3) 声明了文件却不存在 ⇒ 报腐烂；4) 声明没理由 ⇒ 报腐烂。
	_, stale, err = gate.UncoveredContractFiles(dir, pairs, []gate.UnmirroredContract{
		{File: "ghost.ts", Reason: "r"},
		{File: "declared.ts", Reason: "   "},
	})
	if err != nil {
		t.Fatalf("枚举失败: %v", err)
	}
	if len(stale) != 2 {
		t.Fatalf("应报 2 条腐烂声明（文件不存在 / 无理由），得到 %v", stale)
	}
}

// ───────────── 2e. 已声明的有意差异：扩展字段与字段改名（2026-10-09 第二轮） ─────────────

// findMirrorPair 从权威表里取一对镜像（找不到即失败 —— 防「表被删空却测试照绿」）。
func findMirrorPair(t *testing.T, file, iface string) gate.ContractMirrorPair {
	t.Helper()
	for _, p := range gate.ContractMirrorPairs() {
		if p.File == file && p.Interface == iface {
			return p
		}
	}
	t.Fatalf("镜像表里找不到 %s:%s —— 对平范围被删了？", file, iface)
	return gate.ContractMirrorPair{}
}

// TestG1ContractMirror_NewPairsReadTrueSource 断言第二轮新增的镜像对
// **真的在读真源文件**（而不是像它替换掉的那些测试一样比对字面量）。
//
// 判据（判别性，非「看起来像」）：把真源里该 interface 的**第一个字段位置**
// 插入一个探针字段，若该镜像对真的读了这个文件，差异里就必须出现这个探针。
func TestG1ContractMirror_NewPairsReadTrueSource(t *testing.T) {
	cases := []struct{ file, iface string }{
		{"permission-request.ts", "ApprovalStep"},
		{"view-template.ts", "ViewTemplate"},
		{"view-template.ts", "ColumnPref"},
		{"view-template.ts", "LayoutPref"},
		{"strategy-choice.ts", "StrategyChoice"},
		{"strategy-choice.ts", "ChoiceOption"},
		{"strategy-choice.ts", "ImpactPreview"},
		{"pnl.ts", "CaliberMeta"},
		{"tenant.ts", "Tenant"},
		{"tenant.ts", "TenantQuota"},
		{"tenant.ts", "TenantContext"},
	}
	const probe = "gateProbeField"
	for _, c := range cases {
		t.Run(c.file+":"+c.iface, func(t *testing.T) {
			src := repoFile(t, filepath.Join("contracts", c.file))
			pair := findMirrorPair(t, c.file, c.iface)

			// 夹具自证：真源原样 ⇒ 必须判为一致（否则闸门成了「永远报错」）。
			fields, ok := gate.ParseTSInterfaceFields(src)[c.iface]
			if !ok {
				t.Fatalf("真源 %s 里解析不到 interface %s", c.file, c.iface)
			}
			if d := gate.DiffContractMirrorPair(pair, fields); !d.IsClean() {
				t.Fatalf("真源原样却判为漂移（对平表或已声明差异有误）：%s", d)
			}
			if issues := gate.ContractMirrorPairIssues(pair, fields); len(issues) > 0 {
				t.Fatalf("已声明差异本身不成立：%v", issues)
			}

			// 探针：在 interface 体的第一行插入一个新字段。
			anchor := "export interface " + c.iface + " {"
			tampered := strings.Replace(src, anchor, anchor+"\n  "+probe+": string;", 1)
			if tampered == src {
				t.Fatalf("未能在 %s 里定位 %q —— 锚点漂移，本用例已失去判别力", c.file, anchor)
			}
			fields2, ok := gate.ParseTSInterfaceFields(tampered)[c.iface]
			if !ok {
				t.Fatalf("篡改后解析不到 interface %s", c.iface)
			}
			d := gate.DiffContractMirrorPair(pair, fields2)
			if !containsStr(d.MissingInGo, probe) {
				t.Fatalf("真源新增字段 %q 未被报为「真源有而 Go 缺」—— "+
					"说明该镜像对**没有真的读真源文件**（假闸门）。差异：%s", probe, d)
			}
		})
	}
}

// TestG1ContractMirror_DeclaredExtensions 校验「已声明差异」的正反两面：
// 声明得当 ⇒ 不报漂移；声明腐烂 / 偷偷放宽 ⇒ 必须报出来。
func TestG1ContractMirror_DeclaredExtensions(t *testing.T) {
	// 正例一：Go 有意多字段（template.Template 的 v / shares）。
	tpl := findMirrorPair(t, "view-template.ts", "ViewTemplate")
	tplFields := gate.ParseTSInterfaceFields(repoFile(t, filepath.Join("contracts", "view-template.ts")))["ViewTemplate"]
	if len(tpl.ExtraGoFields) == 0 {
		t.Fatal("ViewTemplate 的已声明扩展丢了 —— template.Template 确实比契约多 v / shares")
	}
	if d := gate.DiffContractMirrorPair(tpl, tplFields); !d.IsClean() {
		t.Fatalf("已声明的扩展字段仍被报为漂移：%s", d)
	}
	// 判别性：若**不**登记这些扩展，就必须报出 Go 侧多字段 ——
	// 证明这条声明不是装饰，而是真的在放宽一处**真实存在**的差异。
	noExt := tpl
	noExt.ExtraGoFields = nil
	d := gate.DiffContractMirrorPair(noExt, tplFields)
	if d.IsClean() {
		t.Fatalf("摘掉扩展声明后仍判为一致 —— 该声明形同虚设（本就无差异）")
	}
	if !containsStr(d.MissingInTS, "v") || !containsStr(d.MissingInTS, "shares") {
		t.Fatalf("摘掉扩展声明后应报出 v / shares，实际：%s", d)
	}

	// 负例一：声明了但 Go 里没有该字段 ⇒ 白名单腐烂。
	rotten := tpl
	rotten.ExtraGoFields = append(append([]string(nil), tpl.ExtraGoFields...), "noSuchField")
	if issues := gate.ContractMirrorPairIssues(rotten, tplFields); !anyContains(issues, "noSuchField") {
		t.Fatalf("声明了不存在的扩展字段却未报错：%v", issues)
	}

	// 负例二：把两侧**共有**的字段登记成「Go 独有扩展」⇒ 等于偷偷放宽对平。
	sneaky := tpl
	sneaky.ExtraGoFields = []string{"id"}
	if issues := gate.ContractMirrorPairIssues(sneaky, tplFields); !anyContains(issues, "偷偷放宽") {
		t.Fatalf("把共有字段登记为扩展却未报错：%v", issues)
	}

	// 正例二：字段改名（strategy.Choice 的 current → currentKey）。
	choice := findMirrorPair(t, "strategy-choice.ts", "StrategyChoice")
	choiceFields := gate.ParseTSInterfaceFields(repoFile(t, filepath.Join("contracts", "strategy-choice.ts")))["StrategyChoice"]
	if len(choice.FieldAliases) == 0 {
		t.Fatal("StrategyChoice 的改名映射丢了 —— 契约 current 在 Go 侧叫 currentKey")
	}
	if d := gate.DiffContractMirrorPair(choice, choiceFields); !d.IsClean() {
		t.Fatalf("已声明的字段改名仍被报为漂移：%s", d)
	}
	// 判别性：若**不**应用改名，就必须报出 current / currentKey 的双向差异
	//（证明这条改名不是装饰，而是真的在起作用）。
	noAlias := choice
	noAlias.FieldAliases = nil
	if d := gate.DiffContractMirrorPair(noAlias, choiceFields); d.IsClean() {
		t.Fatalf("摘掉改名映射后仍判为一致 —— 该映射形同虚设：%s", d)
	}

	// 负例三：改名映射指向不存在的 Go 字段 / 源字段不存在 ⇒ 死映射。
	dead := choice
	dead.FieldAliases = map[string]string{"current": "notAField"}
	if issues := gate.ContractMirrorPairIssues(dead, choiceFields); !anyContains(issues, "notAField") {
		t.Fatalf("死映射（目标字段不存在）却未报错：%v", issues)
	}
	dead2 := choice
	dead2.FieldAliases = map[string]string{"ghostField": "currentKey"}
	if issues := gate.ContractMirrorPairIssues(dead2, choiceFields); !anyContains(issues, "ghostField") {
		t.Fatalf("死映射（源字段不存在）却未报错：%v", issues)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func anyContains(xs []string, sub string) bool {
	for _, x := range xs {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

// ───────────────────────────── 3. 接线断言 ─────────────────────────────

// TestG1ContractMirror_CiRunsGate 断言这条闸门真的在 CI 里被执行。
//
// 动机与 g8_perf_wiring_test.go 相同：一个「存在但没人跑」的判定器，
// 与一个不存在的判定器，在「能不能拦住坏事」上完全等价。
// 本闸门是静态/CI 闸门（无运行时调用点，理由见 g1_contract_mirror.go 头注释），
// 故「接上 CI」就是它的全部存在依据。
func TestG1ContractMirror_CiRunsGate(t *testing.T) {
	yml := repoFile(t, ".github/workflows/ci.yml")

	goJob, ok := yamlJobBlock(yml, "go")
	if !ok {
		t.Fatal("CI 缺少 `go` 作业 —— 契约镜像闸门不会被任何路径执行")
	}
	if !strings.Contains(goJob, "ContractMirror") {
		t.Errorf("`go` 作业里没有跑契约镜像闸门（找不到 ContractMirror）。\n"+
			"若删掉该步骤，G1 契约镜像就退回「代码里有、CI 里没有」的状态。\n作业内容：\n%s", goJob)
	}

	gateJob, ok := yamlJobBlock(yml, "gate")
	if !ok {
		t.Fatal("CI 缺少汇总作业 `gate`")
	}
	if !yamlNeedsIncludes(gateJob, "go") {
		t.Errorf("汇总作业 `gate` 的 needs 未包含 go —— go 作业失败不会阻断合并")
	}
}

// TestG1ContractMirror_DocsMentionsGate 钉住文档与代码同步（防「代码有闸门、文档没有」）。
func TestG1ContractMirror_DocsMentionsGate(t *testing.T) {
	doc := repoFile(t, "docs/05-验收闸门.md")
	if !strings.Contains(doc, "契约镜像") {
		t.Error("docs/05 没有「契约镜像」相关条目 —— 闸门清单必须与实现同步（docs/05 §4 纪律）")
	}
}
