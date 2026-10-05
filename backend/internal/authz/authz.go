// Package authz —— M-AUTH 权限求值引擎（D11 / D12 / D13）。
//
// 核心不变量（docs/01 §13、contracts/entitlement.ts）：
//
//	最终权限 = 模板起点
//	          ⊕ 分组授权（并集）
//	          ⊕ 勾选组（四组）⊕ 逐项勾选
//	          ⊕ 已批准申请
//	          ⊕ 上级代授（受「可授出 ⊆ 自身权限」约束）
//	          ⊖ 显式禁用（DENY 优先）
//	          ⊕ 临时授权（时间盒）
//
// 两条铁律：
//  1. **DENY 优先**：任何一处禁用，无论在多少处被启用，最终都是禁用。
//  2. **可授出 ⊆ 自身权限**：代授不得溢出授出者的权限集合（D13）。
//
// 另：D7 取消 —— IT 角色 canViewBusinessValues 恒 false 且不可勾选。
package authz

import (
	"sort"
	"strings"
	"time"
)

// Level 密级。
type Level string

const (
	L1 Level = "L1"
	L2 Level = "L2"
	L3 Level = "L3"
	L4 Level = "L4"
)

var levelRank = map[Level]int{L1: 1, L2: 2, L3: 3, L4: 4}

// MaxLevel 取更宽松的密级（并集语义）。
func MaxLevel(a, b Level) Level {
	if levelRank[a] >= levelRank[b] {
		return a
	}
	return b
}

// MinLevel 取更严格的密级（用于授权上界约束）。
func MinLevel(a, b Level) Level {
	if levelRank[a] <= levelRank[b] {
		return a
	}
	return b
}

// ModuleGrant 模块授权。
type ModuleGrant struct {
	Enabled  []string `json:"enabled"`
	Disabled []string `json:"disabled"`
}

// DimensionGrant 维度授权。
type DimensionGrant struct {
	Dim               string   `json:"dim"`
	Values            []string `json:"values"`
	IncludeDescendants bool    `json:"includeDescendants"`
}

// GroupScopeGrant 勾选组授权（四组）。
type GroupScopeGrant struct {
	Group           string           `json:"group"`
	Fields          []string         `json:"fields,omitempty"`
	ScopedBy        []DimensionGrant `json:"scopedBy,omitempty"`
	Denied          []string         `json:"denied,omitempty"`
	MaxLevel        Level            `json:"maxLevel"`
	DependenciesMet bool             `json:"dependenciesMet,omitempty"`
}

// DataUseGroup 四个数据用途组。
type DataUseGroup string

const (
	GrpOps        DataUseGroup = "grp.ops"
	GrpCostProfit DataUseGroup = "grp.cost_profit"
	GrpInventory  DataUseGroup = "grp.inventory"
	GrpROI        DataUseGroup = "grp.roi"
)

// GroupDeps 勾选组依赖表（grp.roi 依赖 cost_profit + inventory）。
var GroupDeps = map[DataUseGroup][]DataUseGroup{
	GrpOps:        {},
	GrpCostProfit: {},
	GrpInventory:  {},
	GrpROI:        {GrpCostProfit, GrpInventory},
}

// TempGrant 临时授权（时间盒）。
type TempGrant struct {
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expiresAt"`
	GrantedBy string    `json:"grantedBy"`
}

// GrantOrigin 授权来源。
type GrantOrigin string

const (
	OriginTemplate   GrantOrigin = "TEMPLATE"
	OriginGroup      GrantOrigin = "GROUP"
	OriginAdminCheck GrantOrigin = "ADMIN_CHECK"
	OriginSupervisor GrantOrigin = "SUPERVISOR_DELEGATE"
	OriginRequest    GrantOrigin = "REQUEST_APPROVED"
	OriginTemp       GrantOrigin = "TEMP"
)

// Entitlement 账号授权项（与 contracts/entitlement.ts 对齐）。
type Entitlement struct {
	Account                 string             `json:"account"`
	BaseTemplate            string             `json:"baseTemplate,omitempty"`
	Groups                  []string           `json:"groups,omitempty"`
	Supervisor              string             `json:"supervisor,omitempty"`
	DataUseGroups           []GroupScopeGrant  `json:"dataUseGroups,omitempty"`
	Modules                 ModuleGrant        `json:"modules"`
	Dimensions              []DimensionGrant   `json:"dimensions"`
	MaxLevel                Level              `json:"maxLevel"`
	FieldOverrides          []FieldOverride    `json:"fieldOverrides,omitempty"`
	CanViewBusinessValues   bool               `json:"canViewBusinessValues"`
	TempGrants              []TempGrant        `json:"tempGrants,omitempty"`
}

// FieldOverride 逐字段覆写。
type FieldOverride struct {
	Field string `json:"field"`
	Allow bool   `json:"allow"`
}

// EntitlementView 求值结果（渲染层与后端共用）。
type EntitlementView struct {
	Account               string              `json:"account"`
	Modules               []string            `json:"modules"`
	Dimensions            map[string][]string `json:"dimensions"`
	MaxLevel              Level               `json:"maxLevel"`
	CanViewBusinessValues bool                `json:"canViewBusinessValues"`
	DataUseGroups         []DataUseGroup      `json:"dataUseGroups"`
	Source                SourceBreakdown     `json:"source"`
}

// SourceBreakdown 命中来源（审计/排障）。
type SourceBreakdown struct {
	FromTemplate   []string `json:"fromTemplate"`
	FromGroups     []string `json:"fromGroups"`
	FromGrant      []string `json:"fromGrant"`
	FromApproved   []string `json:"fromApprovedRequests"`
	FromDelegation []string `json:"fromDelegations"`
	FromTemp       []string `json:"fromTemp"`
	DeniedBy       []string `json:"deniedBy"`
}

// Resolver 执行权限求值。
type Resolver struct {
	// Templates 模板注册表（模板 ID → 授权草案）。
	Templates map[string]*Entitlement
	// Now 注入时钟（便于测试时间盒过期）。
	Now func() time.Time
}

// NewResolver 构造求值器。
func NewResolver(templates map[string]*Entitlement) *Resolver {
	return &Resolver{Templates: templates, Now: time.Now}
}

// Resolve 求最终权限视图。
func (r *Resolver) Resolve(e *Entitlement, groupGrants []*Entitlement) *EntitlementView {
	view := &EntitlementView{
		Account:       e.Account,
		Dimensions:    map[string][]string{},
		MaxLevel:      L1,
		DataUseGroups: []DataUseGroup{},
	}
	now := r.now()

	modEnabled := map[string]bool{}
	deniedModules := map[string]bool{}
	dimValues := map[string]map[string]bool{}
	groupSet := map[DataUseGroup]bool{}
	fieldAllow := map[string]bool{}   // 逐字段覆写
	fieldDeny := map[string]bool{}
	groupFieldDeny := map[string]bool{}
	// businessValuesWanted：任一授权来源（含模板）声明可见业务数值。
	// 最终仍需过 D7 的 IT 硬约束。
	businessValuesWanted := false

	// 收集器：把一份授权并入（不计 DENY）
	absorb := func(src *Entitlement, origin GrantOrigin) {
		if src == nil {
			return
		}
		for _, m := range src.Modules.Enabled {
			modEnabled[m] = true
		}
		for _, m := range src.Modules.Disabled {
			deniedModules[m] = true
		}
		for _, d := range src.Dimensions {
			if dimValues[d.Dim] == nil {
				dimValues[d.Dim] = map[string]bool{}
			}
			// 空 values = 全部（仍受密级约束）
			if len(d.Values) == 0 {
				dimValues[d.Dim]["*"] = true
			}
			for _, v := range d.Values {
				dimValues[d.Dim][v] = true
			}
		}
		for _, g := range src.DataUseGroups {
			groupSet[DataUseGroup(g.Group)] = true
			for _, f := range g.Denied {
				groupFieldDeny[f] = true
			}
		}
		for _, fo := range src.FieldOverrides {
			if fo.Allow {
				fieldAllow[fo.Field] = true
			} else {
				fieldDeny[fo.Field] = true
			}
		}
		// 业务数值可见性随授权来源并入（模板亦可携带）——
		// 注意：这只是「可能可见」；D7 的 IT 硬约束在末尾统一兜底。
		if src.CanViewBusinessValues {
			businessValuesWanted = true
		}
		view.MaxLevel = MaxLevel(view.MaxLevel, src.MaxLevel)
		_ = origin
	}

	// 1) 模板起点
	if e.BaseTemplate != "" {
		if tpl, ok := r.Templates[e.BaseTemplate]; ok {
			absorb(tpl, OriginTemplate)
			view.Source.FromTemplate = append(view.Source.FromTemplate,
				"template:"+e.BaseTemplate)
		}
	}
	// 2) 分组授权（并集）
	for _, g := range groupGrants {
		absorb(g, OriginGroup)
		view.Source.FromGroups = append(view.Source.FromGroups, "group:"+g.Account)
	}
	// 3) 账号自身的勾选 / 勾选组 / 逐项
	absorb(e, OriginAdminCheck)
	view.Source.FromGrant = append(view.Source.FromGrant, "entitlement:"+e.Account)
	// 4) 临时授权（时间盒；过期即失效）
	for _, tg := range e.TempGrants {
		if tg.ExpiresAt.After(now) {
			view.Source.FromTemp = append(view.Source.FromTemp, "temp:"+tg.Scope)
		}
	}

	// ─────────── 应用 DENY（优先，最高优先级） ───────────
	for m := range deniedModules {
		delete(modEnabled, m)
		view.Source.DeniedBy = append(view.Source.DeniedBy, "module:"+m)
	}
	for f := range fieldDeny {
		// 逐字段显式禁用（若同时被允许，DENY 胜出）
		delete(fieldAllow, f)
		view.Source.DeniedBy = append(view.Source.DeniedBy, "field:"+f)
	}
	for f := range groupFieldDeny {
		delete(fieldAllow, f)
		view.Source.DeniedBy = append(view.Source.DeniedBy, "groupField:"+f)
	}

	// ─────────── 勾选组依赖闭合 ───────────
	// grp.roi 依赖 grp.cost_profit + grp.inventory；依赖未满足则剔除该组
	for g := range groupSet {
		for _, dep := range GroupDeps[g] {
			if !groupSet[dep] {
				delete(groupSet, g)
				view.Source.DeniedBy = append(view.Source.DeniedBy,
					"groupDeps:"+string(g)+"<-"+string(dep))
				break
			}
		}
	}

	// ─────────── 汇总 ───────────
	for m := range modEnabled {
		view.Modules = append(view.Modules, m)
	}
	sort.Strings(view.Modules)

	for dim, set := range dimValues {
		var vals []string
		for v := range set {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		view.Dimensions[dim] = vals
	}
	for g := range groupSet {
		view.DataUseGroups = append(view.DataUseGroups, g)
	}
	sort.Slice(view.DataUseGroups, func(i, j int) bool {
		return view.DataUseGroups[i] < view.DataUseGroups[j]
	})

	// ─────────── D7：IT 恒不可见业务数值（硬约束，不可被任何来源翻盘） ───────────
	// 判定依据可以是「模板 ID」或「账号本身是 IT 模板」（模板注册表里以 tpl.it 为键）。
	// 业务数值可见 = 任一来源声明可见 且 账号非 IT。
	isITAccount := isIT(e.BaseTemplate) || isIT(e.Account)
	view.CanViewBusinessValues = businessValuesWanted && !isITAccount

	return view
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// isIT 判定是否 IT 模板（D7：IT 恒不可见业务数值）。
func isIT(template string) bool {
	return strings.EqualFold(template, "tpl.it") ||
		strings.HasPrefix(strings.ToLower(template), "tpl.it")
}

// DelegationAllowed 校验「可授出 ⊆ 自身权限」（D13 核心不变量）。
//
// 规则：
//   - 模块：grantee 的任一模块必须 ⊆ granter 的模块集合
//   - 密级：grantee.MaxLevel 不得超过 granter.MaxLevel
//   - 维度：grantee 的每个维度取值必须 ⊆ granter 在该维度的取值（"*" 表全部）
//   - 业务数值：granter 不可见业务数值时，不得授出「可见业务数值」
//
// 返回 true = 允许授予。
func DelegationAllowed(granter, grantee *EntitlementView) bool {
	if granter == nil || grantee == nil {
		return false
	}
	// 业务数值不可越授
	if grantee.CanViewBusinessValues && !granter.CanViewBusinessValues {
		return false
	}
	// 密级不得溢出
	if levelRank[grantee.MaxLevel] > levelRank[granter.MaxLevel] {
		return false
	}
	// 模块不得溢出
	gm := map[string]bool{}
	for _, m := range granter.Modules {
		gm[m] = true
	}
	for _, m := range grantee.Modules {
		if !gm[m] {
			return false
		}
	}
	// 维度不得溢出
	for dim, vals := range grantee.Dimensions {
		gVals, ok := granter.Dimensions[dim]
		if !ok {
			return false // 授出者在该维度无任何取值
		}
		allowed := map[string]bool{}
		for _, v := range gVals {
			allowed[v] = true
		}
		if allowed["*"] {
			continue // 授出者持全部
		}
		for _, v := range vals {
			if !allowed[v] {
				return false
			}
		}
	}
	return true
}
