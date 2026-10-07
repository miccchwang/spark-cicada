// G4 第七侧：槽的**匹配键策略**必须可解析 —— 承袭本仓「字段被读进来、写进 DB，
// 却没有任何判定消费」这个病的**第十五个变种**。
//
// 缺口（2026-10-07 实测）：
//
//	`slots/*.yaml` 的 `key_strategy` 在此之前是**被读进来、写进 DB
//	（registry_slot.key_strategy）、但没有任何判定消费**的一个字段。
//	全链路只有三类用法 —— 解析（yaml.Unmarshal 到 Slot.KeyStrategy）、
//	赋值（admin.Plane 传输结构）、写库（store/admin.go 的 INSERT/UPDATE/SELECT 列）——
//	**没有第四类**（比较、判定、阈值）。也就是说：
//	  * 16 个槽全部声明了 key_strategy，但 `store_sku` 与 `想写什么写什么`
//	    在行为上完全等价（都能加载成功、都能写库、都不影响任何结果）；
//	  * docs/03 §2.2 的槽表里有一列「匹配键」，它与 YAML 的 `key_strategy`
//	    本该一一对应，但两处**谁也没被对方校过** ⇒ 表格可以写 `store+sku`
//	    而 YAML 写 `spu`，两条定义永不报错却指向不同的对齐口径。
//
//	后果不是「少一条断言」，而是**跨源对齐口径失去判据**：
//	`key_strategy` 决定采集/预计算时多条来源（渠道流水、成本主数据、库存主表…）
//	按什么键对齐成一行。写错它，桶照样算出来，但**合并后的每一行都可能张冠李戴**
//	（把 A 店的成本配到 B 店的收入上），且全程零报错。静默的错数，比缺数危险得多。
//
// 与 G4 其余六侧的关系（各侧互补，缺一即漏）：
//
//	1 CheckAlgorithmNoDataSource       算法里**不得**混入数据源（分离）
//	2 CheckSlotsRegistered             算法 → 槽 的引用完整性
//	3 CheckFormulaVariablesBound       公式自由变量的**值绑定**
//	4 CheckBucketProducersRegistered   桶 → 算法 的反向引用完整性
//	5 CheckRefreshDeclared             桶的**刷新节律**必须可解析
//	6 CheckSlotSourceResolvable        槽的**数据来源**必须可解析
//	7 CheckKeyStrategyDeclared         槽的**匹配键策略**必须可解析（本文件）
//
// ★ 诚实说明：本侧只断言「键策略**声明**是已知的、可解析的、与来源种类相称的」，
//
//	它**不**验证键在真实数据里真的能对齐（那需要真库/真数据）。同 G2/G3/G10
//	的出站守卫：在正常数据上恒放行 —— 价值是把「键策略写错 / 漏写 / 写成自由文本」
//	变成一条**可被拒绝的加载**，并让这条判定函数拥有真实生产调用点（`slot.LoadRegistry`）。
package gate

import (
	"fmt"
	"sort"
	"strings"
)

// keyStrategySpec 描述一种匹配键策略的**语义与适用来源**。
//
//   - Keys 是「参与对齐的键列」的**规范序列**（如 store+sku 对齐到 store_sku）；
//   - RequiresSourceKinds 为空表示不限定来源种类；非空表示**只有**这些来源
//     种类才允许用它（fail-closed：来源与键不相称 ⇒ 拒绝加载）。
type keyStrategySpec struct {
	// Keys 规范键序列（docs/03 §2.2「匹配键」列的机器可读形式）。
	Keys []string
	// RequiresSourceKinds 限定来源种类；空 = 不限。
	RequiresSourceKinds []string
	// Desc 人类可读说明（进报错信息）。
	Desc string
}

// keyStrategies 是允许的匹配键策略（**唯一权威表**）。
//
// ★ 权威依据：docs/03 §2.2 槽表的「匹配键」列 + §2.3 示例注释。
// 声明写法（YAML `key_strategy`）与文档写法（`store+sku`、`barcode→sku`）不同形，
//
//	故本表同时给出规范键序列，供「文档 ↔ 定义」双向漂移检测使用。
//
// ★ 不接受任意自由文本：`key_strategy: 智能匹配` 看起来合理，但解析器拿不到
//
//	键，最终必然退化成「不影响任何事」—— 正是本文件要消灭的形态。
var keyStrategies = map[string]keyStrategySpec{
	// 主数据槽：整行直达，键即行本身（product_master 的每一行就是一条产品）。
	"direct": {Keys: []string{"row"}, Desc: "整行直达（主数据行即键，不做跨源合并）"},

	// 明细/事实槽：按维度组合对齐。
	"store_sku":   {Keys: []string{"store", "sku"}, Desc: "店铺 + SKU（渠道流水与成本的默认对齐口径）"},
	"sku":         {Keys: []string{"sku"}, Desc: "仅 SKU（库存/物流等以 SKU 为唯一粒度的来源）"},
	"spu":         {Keys: []string{"spu"}, Desc: "仅 SPU（达人佣金/市场费用按 SPU 计提）"},
	"channel_day": {Keys: []string{"channel", "day"}, Desc: "渠道 + 日期（广告花费按渠道逐日对齐）"},
	"shop_txn":    {Keys: []string{"shop", "txn"}, Desc: "店铺 + 交易号（平台费用按交易逐笔对齐）"},
	"date_pair":   {Keys: []string{"date", "pair"}, Desc: "日期 + 货币对（汇率按日、按货币对对齐）"},

	// 特殊：先按条码精确匹配，未命中再退到品牌+归一化 SKU 名。
	"barcode_then_sku": {Keys: []string{"barcode", "brand", "sku"},
		Desc: "完整条码精确匹配优先，无条码用品牌+精确归一化 SKU 名（成本主数据专用）"},
}

// KeyStrategies 暴露允许的键策略集合（供接线闸门比对，防两侧漂移）。
func KeyStrategies() map[string]bool {
	out := make(map[string]bool, len(keyStrategies))
	for k := range keyStrategies {
		out[k] = true
	}
	return out
}

// KeyStrategyKeys 返回某策略的规范键序列（未知名返回 nil,false）。
//
// 供「跨槽口径一致性」判定使用：两个槽若声明了**同一**策略，其键序列必须一致；
// 若声明了**不同**策略却共用一个 source，则是可疑的口径分叉（见 CheckKeyStrategyCohesion）。
func KeyStrategyKeys(name string) ([]string, bool) {
	sp, ok := keyStrategies[strings.TrimSpace(strings.ToLower(name))]
	if !ok {
		return nil, false
	}
	out := make([]string, len(sp.Keys))
	copy(out, sp.Keys)
	return out, true
}

// KeyStrategyDoc 是待判定的一条键策略声明（由 slot 包从 YAML 或 DB 行构造）。
type KeyStrategyDoc struct {
	// SlotID 槽 ID（报错时可定位）。
	SlotID string
	// Raw 声明原文（如 `store_sku`）。
	Raw string
	// SourceKind 该槽的来源种类（用于「键与来源是否相称」判定；可空 = 跳过该判定）。
	SourceKind string
	// SourceRef 该槽的来源引用（用于「共用来源的槽键口径必须一致」判定；可空 = 跳过）。
	SourceRef string
}

// ParseKeyStrategy 解析匹配键策略声明，返回**规范名**（小写去空白）。
//
// 第二个返回值为 false 表示「不是已知的键策略」。注意本函数**不做**任何猜测
// 或模糊匹配 —— 认不出就是 false（fail-closed）。
func ParseKeyStrategy(raw string) (string, bool) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return "", false
	}
	// 归一化：`store+sku` / `store-sku` / `store sku` 都可写成 `store_sku`。
	// 只做分隔符归一，不做语义猜测（`store_sku` 与 `sku_store` 是不同的两串，
	// 后者不认 —— 顺序错就是声明错，不该被静默纠正）。
	norm := strings.NewReplacer("+", "_", "-", "_", " ", "_", "→", "_then_").Replace(s)
	norm = strings.Trim(norm, "_")
	for strings.Contains(norm, "__") {
		norm = strings.ReplaceAll(norm, "__", "_")
	}
	if _, ok := keyStrategies[norm]; !ok {
		return "", false
	}
	return norm, true
}

// KnownKeyStrategies 返回排序后的已知键策略（供报错信息稳定可读）。
func KnownKeyStrategies() []string {
	out := make([]string, 0, len(keyStrategies))
	for k := range keyStrategies {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CheckKeyStrategyDeclared 断言每个槽都声明了**可解析且已知**的匹配键策略
// （G4 / docs/03 §2.2、§2.3）。
//
// 校验口径（与 CheckFreshnessDeclared / CheckRefreshDeclared 同构，有意保守）：
//   - 必须非空（缺声明 ⇒ 无对齐判据，跨源合并的口径不可知）；
//   - 必须是 KeyStrategies 里的已知策略（`store_sku` / `barcode_then_sku` / `direct` …）；
//   - 不认 `store+sku`（应写 `store_sku`）、`智能匹配`、`auto` 等自由文本
//     —— 它们看起来合理，但解析器拿不到键，最终还是会退化成「不影响任何事」；
//   - 若声明了 SourceKind，且该策略**限定**了来源种类，则二者必须相称。
//
// 返回人类可读原因（fail-closed：无法确认键 ⇒ 不视为合规）。
func CheckKeyStrategyDeclared(docs []KeyStrategyDoc) []string {
	var violations []string
	for _, d := range docs {
		raw := strings.TrimSpace(d.Raw)
		if raw == "" {
			violations = append(violations, fmt.Sprintf(
				"槽 %s 未声明 key_strategy —— 无对齐判据，跨源合并口径不可知", d.SlotID))
			continue
		}
		norm, ok := ParseKeyStrategy(raw)
		if !ok {
			violations = append(violations, fmt.Sprintf(
				"槽 %s 的 key_strategy=%q 不是已知匹配键策略（允许：%s；"+
					"自由文本如 `智能匹配`/`auto` 会让跨源对齐退化为「不影响任何事」）",
				d.SlotID, raw, strings.Join(KnownKeyStrategies(), "/")))
			continue
		}
		sp := keyStrategies[norm]
		if len(sp.RequiresSourceKinds) == 0 || strings.TrimSpace(d.SourceKind) == "" {
			continue
		}
		if !containsString(sp.RequiresSourceKinds, strings.TrimSpace(d.SourceKind)) {
			violations = append(violations, fmt.Sprintf(
				"槽 %s 的 key_strategy=%s（%s）与来源种类 %s 不相称（该策略仅适用于：%s）",
				d.SlotID, norm, sp.Desc, d.SourceKind, strings.Join(sp.RequiresSourceKinds, "/")))
		}
	}
	sort.Strings(violations)
	return violations
}

// CheckKeyStrategyCohesion 断言**共用同一来源**的槽之间键策略不冲突。
//
// 为什么单列一条（而不是并入 CheckKeyStrategyDeclared）：
//
//	单个槽的声明可以完全合法（`store_sku` 与 `sku` 都是已知策略），但当两个槽
//	**指向同一个 source_ref** 时，它们必须按**同一个键**对齐 —— 否则同一个来源
//	被读两遍、按不同键合并，结果必然对不上（且两边都不报错）。
//	这正是「逐条校验都过、合起来是错的」那类缺陷，单元断言看不见。
//
// 判定口径：
//   - 仅对**同 source_kind 且同 source_ref** 的槽分组（非空条件下）；
//   - 组内键序列必须一致（按 KeyStrategyKeys 的规范序列比较）；
//   - `direct` 与明细键**允许**共存（主数据整行直达与事实表按维度对齐是两回事，
//     它们可以共用一张主表引用而各自定义读法）。
//
// 返回人类可读原因（fail-closed）。
func CheckKeyStrategyCohesion(docs []KeyStrategyDoc) []string {
	// 分组键 = source_kind + "\x00" + source_ref（两者都必须非空才有对齐语义）。
	type group struct {
		kind, ref string
		members   []KeyStrategyDoc
	}
	groups := map[string]*group{}
	order := []string{}
	for _, d := range docs {
		kind := strings.TrimSpace(d.SourceKind)
		ref := strings.TrimSpace(d.SourceRef)
		if kind == "" || ref == "" {
			continue
		}
		gk := kind + "\x00" + ref
		if _, ok := groups[gk]; !ok {
			groups[gk] = &group{kind: kind, ref: ref}
			order = append(order, gk)
		}
		groups[gk].members = append(groups[gk].members, d)
	}

	var violations []string
	for _, gk := range order {
		g := groups[gk]
		if len(g.members) < 2 {
			continue
		}
		// 逐成员算出规范键序列；未知/非法声明交给 CheckKeyStrategyDeclared 报，
		// 本函数只处理**都合法**却互相冲突的情形（避免重复噪音）。
		type verdict struct {
			doc  KeyStrategyDoc
			name string
			keys []string
		}
		var verdicts []verdict
		for _, m := range g.members {
			norm, ok := ParseKeyStrategy(m.Raw)
			if !ok {
				continue
			}
			keys, ok := KeyStrategyKeys(norm)
			if !ok {
				continue
			}
			verdicts = append(verdicts, verdict{doc: m, name: norm, keys: keys})
		}
		if len(verdicts) < 2 {
			continue
		}

		// 以第一个**非 direct** 的成员为基准（direct 与明细键允许共存）。
		var base *verdict
		for i := range verdicts {
			if verdicts[i].name != "direct" {
				base = &verdicts[i]
				break
			}
		}
		if base == nil {
			continue // 组内全是 direct（同源同读法）⇒ 一致。
		}
		for _, v := range verdicts {
			if v.name == "direct" {
				continue // direct 与明细键允许共存。
			}
			if !equalStrings(v.keys, base.keys) {
				violations = append(violations, fmt.Sprintf(
					"槽 %s 与 %s 共用来源 %s/%s 却按不同键对齐（%s=[%s] vs %s=[%s]）"+
						"⇒ 同一来源被按两种口径合并，结果必然对不上",
					v.doc.SlotID, base.doc.SlotID, g.kind, g.ref,
					v.name, strings.Join(v.keys, "+"),
					base.name, strings.Join(base.keys, "+")))
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// containsString 是极小工具（本包纪律：不引第三方依赖）。
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// equalStrings 按序比较（本包纪律：切片比较不引 reflect，保持显式可读）。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
