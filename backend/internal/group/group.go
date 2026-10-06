// Package group —— M-GROUP 用户分组（D12）。
//
// 分组是**批量授权的载体**：把「同一批人的共同权限」抽出来，避免逐个账号重复勾选。
// 与「角色模板」互补 —— 模板定「新账号默认勾什么」，分组定「哪些人共享额外权限」。
//
// ★★ 本包是**纯逻辑**（无 DB、无网络），便于无库环境测试与复用：
//   * 组授权的展开与合并
//   * 与个人勾选的求值（DENY 优先）
//   * 「加入该组后能看到什么」预览
//
// 纪律（contracts/user-group.ts、docs/02 M-GROUP）：
//   1. 一人多组，授权取**并集**；组不含层级（层级由 D13 职权链承担）。
//   2. 组授权与个人勾选冲突 → **DENY 优先**（组内允许不得覆盖个人 DENY）。
//   3. 成员可「退出继承」（InheritsGrants=false）以处理例外；
//      该成员仍在组内（可被管理/统计），但不继承组授权。
//   4. 受限分组（Restricted）不得授出业务数值 —— 与 D7 一致的兜底。
package group

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 授权种类。与 SQL 的 fact_group_grant.kind 一一对应。
const (
	KindModule       = "module"
	KindDimension    = "dimension"
	KindLevel        = "level"
	KindDataUseGroup = "data_use_group"
)

// Level 密级。
type Level string

const (
	L1 Level = "L1"
	L2 Level = "L2"
	L3 Level = "L3"
	L4 Level = "L4"
)

// Rank 返回密级序数（L1=1 … L4=4）。未知返回 0（视为最小）。
func Rank(l Level) int {
	switch l {
	case L1:
		return 1
	case L2:
		return 2
	case L3:
		return 3
	case L4:
		return 4
	}
	return 0
}

// MaxLevel 取较大密级。
func MaxLevel(a, b Level) Level {
	if Rank(a) >= Rank(b) {
		return a
	}
	return b
}

// MinLevel 取较小密级。
func MinLevel(a, b Level) Level {
	if Rank(a) <= Rank(b) {
		return a
	}
	return b
}

// Grant 一条组级授权。Key 的形态由 Kind 决定：
//
//	module          → module.<id>
//	dimension       → dimension.<dim>
//	level           → level.<L1..L4>
//	data_use_group  → data_use_group.<grp.xxx>
type Grant struct {
	Kind               string
	Key                string
	Values             []string
	IncludeDescendants bool
	// Deny=true 表示**显式拒绝**，优先于任何允许。
	Deny bool
}

// Group 分组定义。
type Group struct {
	ID          string
	Name        string
	Description string
	Region      string
	Owners      []string
	// Restricted=true 时不得授出业务数值（兜底 D7）。
	Restricted bool
	Grants     []Grant
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Membership 成员关系。
type Membership struct {
	Account string
	GroupID string
	Role    string // member | owner
	// InheritsGrants=false ⇒ 在组内但不继承组授权（例外处理）。
	InheritsGrants bool
	JoinedAt       time.Time
}

// Personal 个人勾选（D11），来自 dim_account_entitlement。
type Personal struct {
	Account                string
	BaseTemplate           string
	Modules                []string
	Dimensions             map[string][]string
	DataUseGroups          []string
	MaxLevel               Level
	CanViewBusinessValues  bool
	Denies                 []string // kind:key 形态，优先于任何允许
}

// IsIT 判断是否为 IT 模板（D7：IT 恒不可见业务数值）。
//
// 与 authz / req 包口径一致：前缀 tpl.it 即视为 IT（含带版本的 tpl.it.v2 等）。
func IsIT(baseTemplate string) bool {
	return baseTemplate == "tpl.it" || strings.HasPrefix(baseTemplate, "tpl.it.")
}

// ───────────────────────────── 求值 ─────────────────────────────

// Evaluated 分组与个人勾选合并后的**有效权限**。
type Evaluated struct {
	Modules               []string
	Dimensions            map[string][]string
	DataUseGroups         []string
	MaxLevel              Level
	CanViewBusinessValues bool
	// Denies 生效的拒绝项（kind:key），便于审计「为什么看不到」。
	Denies []string
	// Sources 每项权限的来源分组（可追溯：是哪个组给的）。
	Sources map[string][]string
	// GroupIDs 参与求值的分组（仅继承者）。
	GroupIDs []string
}

// denyKey 把「拒绝项」归一为 `kind:key` 形态。
//
// ★ 这里曾有一个真实的键不一致 bug：写入用 `gr.Kind + ":" + gr.Key`（key 是
// 带前缀的 `dimension.channel`），而查找用 `kind + ":" + dim`（dim 是**剥掉前缀**的
// `channel`）—— 两侧永不相等，DENY 静默失效（授权照给，拒绝被忽略）。
// 单测 `TestEvaluate_DimensionDenyRemovesWholeDim` 当场抓住。
//
// 现在统一口径：**一律使用带前缀的完整 key**。
func denyKey(kind, key string) string {
	return kind + ":" + key
}

// Evaluate 把「个人勾选 + 若干组授权」求值为有效权限。
//
// 求值顺序（关键）：
//  1. 起点 = 个人勾选；
//  2. 叠加所有**继承中**的组授权（并集）；
//  3. 应用全部 DENY（个人 DENY ∪ 组 DENY）—— **DENY 最后生效，且优先**；
//  4. 受限分组与 IT 模板强制 canViewBusinessValues=false。
//
// 传入 memberships 与 groups 的对应关系由调用方保证（本函数按 GroupID 匹配）。
func Evaluate(p *Personal, groups []*Group, memberships []*Membership) *Evaluated {
	e := &Evaluated{
		Dimensions:    map[string][]string{},
		Sources:       map[string][]string{},
		DataUseGroups: []string{},
		Modules:       []string{},
		MaxLevel:      L1,
	}

	modSet := map[string]bool{}
	dugSet := map[string]bool{}
	denied := map[string]bool{}

	// 1) 起点：个人勾选
	if p != nil {
		e.MaxLevel = p.MaxLevel
		if Rank(e.MaxLevel) == 0 {
			e.MaxLevel = L1
		}
		e.CanViewBusinessValues = p.CanViewBusinessValues
		for _, m := range p.Modules {
			modSet[m] = true
			e.Sources[m] = append(e.Sources[m], "personal")
		}
		for _, g := range p.DataUseGroups {
			dugSet[g] = true
		}
		for dim, vals := range p.Dimensions {
			e.Dimensions[dim] = mergeValues(e.Dimensions[dim], vals)
		}
		for _, d := range p.Denies {
			denied[normDeny(d)] = true
		}
	}

	// 2) 叠加继承中的组授权
	byID := map[string]*Group{}
	for _, g := range groups {
		if g != nil {
			byID[g.ID] = g
		}
	}
	for _, ms := range memberships {
		if ms == nil || !ms.InheritsGrants {
			continue // ★ 退出继承：在组内但不继承授权
		}
		g := byID[ms.GroupID]
		if g == nil {
			continue
		}
		e.GroupIDs = append(e.GroupIDs, g.ID)
		for _, gr := range g.Grants {
			if gr.Deny {
				denied[denyKey(gr.Kind, gr.Key)] = true
				continue
			}
			switch gr.Kind {
			case KindModule:
				modSet[gr.Key] = true
				e.Sources[gr.Key] = appendUnique(e.Sources[gr.Key], g.ID)
			case KindDimension:
				dim := strings.TrimPrefix(gr.Key, "dimension.")
				e.Dimensions[dim] = mergeValues(e.Dimensions[dim], gr.Values)
				e.Sources["dimension:"+dim] = appendUnique(e.Sources["dimension:"+dim], g.ID)
			case KindLevel:
				lv := Level(strings.TrimPrefix(gr.Key, "level."))
				e.MaxLevel = MaxLevel(e.MaxLevel, lv)
				e.Sources["level:"+string(lv)] = appendUnique(e.Sources["level:"+string(lv)], g.ID)
			case KindDataUseGroup:
				dug := strings.TrimPrefix(gr.Key, "data_use_group.")
				dugSet[dug] = true
				e.Sources["data_use_group:"+dug] = appendUnique(e.Sources["data_use_group:"+dug], g.ID)
			}
		}
	}

	// 3) ★ DENY 最后生效，且优先（个人 DENY 与组 DENY 同等）
	for m := range modSet {
		if denied[denyKey(KindModule, m)] {
			delete(modSet, m)
		}
	}
	for dim := range e.Dimensions {
		if denied[denyKey(KindDimension, "dimension."+dim)] {
			delete(e.Dimensions, dim)
		}
	}
	for d := range dugSet {
		if denied[denyKey(KindDataUseGroup, "data_use_group."+d)] {
			delete(dugSet, d)
		}
	}
	// 密级 DENY：拒绝全部 level:* 时降回 L1
	if denied[denyKey(KindLevel, "level.*")] {
		e.MaxLevel = L1
	}

	// 4) 受限分组 / IT 模板 ⇒ 强制不可见业务数值
	if p != nil && IsIT(p.BaseTemplate) {
		e.CanViewBusinessValues = false
	}
	for _, id := range e.GroupIDs {
		if g := byID[id]; g != nil && g.Restricted {
			e.CanViewBusinessValues = false
		}
	}

	e.Modules = sortedKeys(modSet)
	e.DataUseGroups = sortedKeys(dugSet)
	e.Denies = sortedKeys(denied)
	for dim := range e.Dimensions {
		sort.Strings(e.Dimensions[dim])
	}
	sort.Strings(e.GroupIDs)
	return e
}

// Preview 生成「加入该组后能看到什么」的预览。
//
// ★ 这是 M-GROUP 验收里明确要求的能力：管理员在加人前必须能**先看到结果**，
// 否则批量授权就是盲操作。
//
// sampleAccount 为示例成员：预览 = 该成员**当前**权限 ∪ 该组授权（含 DENY 生效后）。
func Preview(g *Group, p *Personal, memberships []*Membership) *Evaluated {
	// 构造「假设已加入该组」的成员关系，其他关系保持不变。
	ms := make([]*Membership, 0, len(memberships)+1)
	joined := false
	for _, m := range memberships {
		if m == nil {
			continue
		}
		if m.GroupID == g.ID {
			// 已在组内：确保继承（预览「继承后」的样貌）
			ms = append(ms, &Membership{Account: m.Account, GroupID: g.ID, InheritsGrants: true, Role: m.Role})
			joined = true
			continue
		}
		ms = append(ms, m)
	}
	if !joined {
		ms = append(ms, &Membership{Account: p.account(), GroupID: g.ID, InheritsGrants: true, Role: "member"})
	}
	return Evaluate(p, []*Group{g}, ms)
}

// ───────────────────────────── 校验 ─────────────────────────────

// Validate 校验分组定义的合法性。
//
// 返回全部问题（而非首个），便于管理界面一次性提示。
func Validate(g *Group) []string {
	var problems []string
	if g == nil {
		return []string{"分组为空"}
	}
	if strings.TrimSpace(g.ID) == "" {
		problems = append(problems, "分组 ID 不能为空")
	}
	if strings.TrimSpace(g.Name) == "" {
		problems = append(problems, "分组名称不能为空")
	}
	seen := map[string]bool{}
	for i, gr := range g.Grants {
		if gr.Kind == "" {
			problems = append(problems, fmt.Sprintf("授权 #%d 缺少 kind", i))
			continue
		}
		if gr.Key == "" {
			problems = append(problems, fmt.Sprintf("授权 #%d 缺少 key", i))
			continue
		}
		// key 前缀必须与 kind 一致 —— 否则求值时会静默匹配不上
		if !strings.HasPrefix(gr.Key, gr.Kind+".") {
			problems = append(problems, fmt.Sprintf("授权 #%d 的 key=%q 与 kind=%q 前缀不符（应为 %s.*）",
				i, gr.Key, gr.Kind, gr.Kind))
		}
		k := gr.Kind + ":" + gr.Key
		if seen[k] {
			problems = append(problems, fmt.Sprintf("授权 %s 重复", k))
		}
		seen[k] = true

		if gr.Kind == KindLevel {
			lv := Level(strings.TrimPrefix(gr.Key, "level."))
			if Rank(lv) == 0 {
				problems = append(problems, fmt.Sprintf("授权 #%d 密级 %q 非法（应为 L1..L4）", i, gr.Key))
			}
		}
	}
	// ★ 受限分组不得授出业务数值（D7 兜底）：密级不得超过 L2。
	if g.Restricted {
		for _, gr := range g.Grants {
			if gr.Kind == KindLevel {
				lv := Level(strings.TrimPrefix(gr.Key, "level."))
				if Rank(lv) > Rank(L2) {
					problems = append(problems,
						fmt.Sprintf("受限分组不得授出 %s（超过 L2）—— D7 兜底", lv))
				}
			}
		}
	}
	return problems
}

// ErrDeniedByPolicy 表示操作被权限策略拒绝。
var ErrDeniedByPolicy = errors.New("group: 操作被权限策略拒绝")

// ───────────────────────────── 辅助 ─────────────────────────────

func (p *Personal) account() string {
	if p == nil {
		return ""
	}
	return p.Account
}

// normDeny 把个人 DENY 输入归一为 `kind:key` 形态（key **带前缀**，与组侧一致）。
//
// 允许的写法：
//
//	"module:module.pnl"            → "module:module.pnl"
//	"module:module.pnl"（已规范）  → 原样
//	"module.pnl"                   → "module:module.pnl"
//	"data_use_group:grp.roi"       → 原样（注意 kind 本身含下划线，不能按首个 '.' 切）
//
// ★ 归一化必须与 denyKey() 产出**完全一致**，否则 DENY 会静默失效。
func normDeny(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ":"); i > 0 {
		kind, key := s[:i], s[i+1:]
		// ★ 判据是「key 是否已带该 kind 前缀」，而不是「key 里有没有点」——
		// 值本身可能含点（如 grp.roi），用「有没有点」判会在这种输入上漏加前缀，
		// 导致 DENY 键与组侧不一致而静默失效。
		if !strings.HasPrefix(key, kind+".") {
			key = kind + "." + key
		}
		return kind + ":" + key
	}
	// 无冒号：从**已知 kind 列表**里找前缀，避免 data_use_group 这类含下划线的被判错。
	for _, kind := range []string{KindDataUseGroup, KindDimension, KindModule, KindLevel} {
		if strings.HasPrefix(s, kind+".") {
			return kind + ":" + s
		}
	}
	return s
}

func mergeValues(a, b []string) []string {
	set := map[string]bool{}
	for _, v := range a {
		set[v] = true
	}
	for _, v := range b {
		set[v] = true
	}
	// "*" 是全量标记：若出现则直接返回 ["*"]（避免与具体值并存产生歧义）
	if set["*"] {
		return []string{"*"}
	}
	return sortedKeys(set)
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
