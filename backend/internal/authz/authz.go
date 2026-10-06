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
	// ScopedModules 本组可见的模块子集（渲染层据此裁剪，空 = 不限）。
	ScopedModules []string `json:"scopedModules,omitempty"`
	// ScopedLevel 本组内密级上限（四组各自独立；空 = 沿用账号 MaxLevel）。
	// ★ 这是「勾选组」区别于「模块开关」的关键：同一账号里
	//   运营数据可以是 L2，而投资与回报只是 L1。
	ScopedLevel Level `json:"scopedLevel,omitempty"`
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

// GrantRecord 授权来源记录（与 contracts/entitlement.ts 的 GrantRecord 对齐）。
//
// ★ 存在的意义：契约 v1.2 把 `grants` 列为 Entitlement 的正式字段，用于「审计 +
// 校验不溢出」，但 v1.2 前服务端**完全没有对应结构** —— 结果是授权从哪来、
// 谁授的、上界是谁，在服务端无从得知；`source.fromApprovedRequests` /
// `source.fromDelegations` 两个字段因此**恒为空**，看着有、实则从不填。
// 本结构补齐这一层，并由 Resolve 如实分层。
type GrantRecord struct {
	Origin GrantOrigin `json:"origin"`
	// GrantedBy 授出者账号；TEMPLATE/GROUP 可为系统。
	GrantedBy string `json:"grantedBy"`
	// UpperBoundRef 授出者自身在该 scope 的上界（用于校验不溢出）。
	UpperBoundRef string `json:"upperBoundRef,omitempty"`
	At            string `json:"at,omitempty"`
	// RequestID 关联的申请单 ID（Origin=REQUEST_APPROVED 时）。
	RequestID string `json:"requestId,omitempty"`
	// ModuleIDs 该来源**实际生效的模块**（REQUEST_TEMP/TEMP 等按模块限定的来源用）。
	// 为空 = 不限定（并入来源携带的全部模块）。
	ModuleIDs []string `json:"moduleIds,omitempty"`
	// GroupIDs 该来源带来的勾选组，供回收时判定「组是否本单独有」。
	GroupIDs []string `json:"groupIds,omitempty"`
	// ExpiresAt 该来源自身的时间盒（RFC3339）。空 = 长期有效，永不到期。
	// ★ 到期回收的唯一事实来源：Resolve 直接据此判活，回收器无需另建索引。
	ExpiresAt string `json:"expiresAt,omitempty"`
	// Reclaimed 该来源已被到期回收（保留记录以满足「全链路可审计」）。
	Reclaimed   bool   `json:"reclaimed,omitempty"`
	ReclaimedAt string `json:"reclaimedAt,omitempty"`
}

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
	// Grants 授权来源记录（审计用）；按 origin 分层进 EntitlementView.Source。
	Grants                  []GrantRecord      `json:"grants,omitempty"`
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
	// GroupScopes 各勾选组的组级限定（字段子集 / 维度 / 密级）。
	// 仅含确实声明了限定的组；用于渲染层做组级裁边。
	GroupScopes           []GroupScopeGrant   `json:"groupScopes,omitempty"`
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
	// ExpiredGrants 已过时间盒、本次**不再生效**的授权来源。
	// 与 DeniedBy 分开：被 DENY 是「不允许」，过期是「曾经允许、现已失效」——
	// 两者在排障时的处置完全不同，混在一起会误导追责。
	ExpiredGrants []string `json:"expiredGrants"`
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
	// groupScopes 收集四个勾选组各自的限定（字段/维度/密级），
	// 供渲染层做「组级裁边」；此前这些限定在 absorb 中被整体丢弃。
	groupScopes := map[DataUseGroup]*GroupScopeGrant{}
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
			// 勾选组自带密级：此前 absorb 只把组名收录进 groupSet，
			// **组的 MaxLevel / ScopedModules / Fields / Denied 全部被丢弃** ——
			// 于是「四组各自独立密级」这一 D11 设计形同虚设（写了不生效）。
			// 现按「同组取并集、密级取更宽松、DENY 只增不减」合并。
			mergeGroupScopes(groupScopes, g)
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

	// ─────────── 来源分层（审计）───────────
	// 契约 v1.2 的 EntitlementView.source 有 7 个桶。此前只有 3 个被填充
	// （template / group / grant），另外 2 个 —— fromApprovedRequests 与
	// fromDelegations —— **恒为空数组**：字段存在、接口不报错、但永远是 []，
	// 排障时看不出「这条权限是上次申请批下来的」。
	// 现按 e.Grants 的 origin 如实分层，并对未知来源显式标注（不静默丢弃）。
	//
	// ★ 时间盒（2026-10-06 补）：来源记录**必须带自己的到期时间**。
	//   在此之前到期回收只能靠调用方另建索引 —— 但 GrantRecord 里根本没有
	//   时间盒字段可查，于是「到期自动回收」（docs/02 M-REQ 闭环最后一步）
	//   在服务端同样**恒真**：回收器查不到任何到期信息，自然「从不漏收」。
	//   现改为：来源记录自带 ExpiresAt，Resolve 直接按它判活；
	//   已到期的授权**照常分层**（排障要看得见它曾经生效）但在标识上标记
	//   `[expired]`，并由 view.Source.ExpiredGrants 分类计数。
	for _, g := range e.Grants {
		// 时间盒：长授权（ExpiresAt 为空）视为永不到期。
		// 已回收（Reclaimed）或已过期 ⇒ 本次**不再生效**，但仍照常分层，
		// 让排障者看得见「这条权限曾经生效过、现已被收回」。
		dead := g.Reclaimed
		tag := ""
		if dead {
			tag = "[reclaimed]"
		} else if isGrantExpired(g, now) {
			dead = true
			tag = "[expired]"
		}
		if dead {
			view.Source.ExpiredGrants = append(view.Source.ExpiredGrants,
				describeGrant(g, "request:")+tag)
		}
		switch g.Origin {
		case OriginRequest:
			view.Source.FromApproved = append(
				view.Source.FromApproved, describeGrant(g, "request:"))
		case OriginSupervisor:
			view.Source.FromDelegation = append(
				view.Source.FromDelegation, describeGrant(g, "delegate:"))
		case OriginTemplate:
			view.Source.FromTemplate = append(
				view.Source.FromTemplate, describeGrant(g, "template:"))
		case OriginGroup:
			view.Source.FromGroups = append(
				view.Source.FromGroups, describeGrant(g, "group:"))
		case OriginAdminCheck:
			view.Source.FromGrant = append(
				view.Source.FromGrant, describeGrant(g, "grant:"))
		case OriginTemp:
			view.Source.FromTemp = append(
				view.Source.FromTemp, describeGrant(g, "temp:"))
		default:
			// 未知来源既不丢弃也不冒充：显式落到 grant 桶并保留原样标识，
			// 避免「新来源加进来但 source 里查无此人」的静默黑洞。
			view.Source.FromGrant = append(view.Source.FromGrant,
				"unknown:"+string(g.Origin)+":"+g.GrantedBy)
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
		// 组级限定随组一并带出；仅当确实填了限定才输出（避免契约噪声）。
		if s := groupScopes[g]; s != nil {
			if s.MaxLevel != "" || len(s.Fields) > 0 ||
				len(s.ScopedModules) > 0 || len(s.Denied) > 0 {
				view.GroupScopes = append(view.GroupScopes, *s)
			}
		}
		view.DataUseGroups = append(view.DataUseGroups, g)
	}
	sort.Slice(view.DataUseGroups, func(i, j int) bool {
		return view.DataUseGroups[i] < view.DataUseGroups[j]
	})
	sort.Slice(view.GroupScopes, func(i, j int) bool {
		return view.GroupScopes[i].Group < view.GroupScopes[j].Group
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

// describeGrant 渲染一条来源记录为可读标识：
// `<prefix><授出者>[@<申请单ID>]`。requestId 存在时带上，便于排障时直接跳单。
func describeGrant(g GrantRecord, prefix string) string {
	by := g.GrantedBy
	if by == "" {
		by = "system"
	}
	if g.RequestID != "" {
		return prefix + by + "@" + g.RequestID
	}
	return prefix + by
}

// mergeGroupScopes 把一份勾选组授权并入收集表（同组并集）。
//
// 纪律与其余授权一致：
//   - 密级取**更宽松**者（并集语义；严格化由 DENY / 审批上界负责）
//   - 字段 / 模块取并集；**空 = 组内全部**（空吞并任何集合仍为「全部」）
//   - DENY（Denied）**只增不减**
func mergeGroupScopes(m map[DataUseGroup]*GroupScopeGrant, g GroupScopeGrant) {
	key := DataUseGroup(g.Group)
	cur, ok := m[key]
	if !ok {
		cp := g
		cp.Fields = append([]string(nil), g.Fields...)
		cp.Denied = append([]string(nil), g.Denied...)
		cp.ScopedModules = append([]string(nil), g.ScopedModules...)
		cp.ScopedBy = append([]DimensionGrant(nil), g.ScopedBy...)
		m[key] = &cp
		return
	}
	cur.MaxLevel = MaxLevel(cur.MaxLevel, g.MaxLevel)
	cur.Fields = unionOrAll(cur.Fields, g.Fields)
	cur.ScopedModules = unionOrAll(cur.ScopedModules, g.ScopedModules)
	cur.Denied = unionContains(cur.Denied, g.Denied)
}

// unionContains 普通并集（与 req 包同语义，此处自持以避免依赖倒置）。
func unionContains(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, v := range b {
		if !containsStr(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// unionOrAll 并集语义，但空集合表示「全部」——空吞并任何集合仍为「全部」。
func unionOrAll(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	return unionContains(a, b)
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// isGrantExpired 判定一条来源记录是否已过时间盒。
//
// 纪律：
//   - ExpiresAt 为空 = 长期授权，**永不到期**（不能因为「没填就当作过期」而误回收）。
//   - ExpiresAt 不可解析 = 视为**到期**（fail-closed）。解析不了的到期时间无法
//     证明它还没过期；授权系统宁可少给，不可多给。
//   - 恰好等于 now = 已到期（与 req.Service.Reclaim 的 `!Before(expiry)` 同口径）。
func isGrantExpired(g GrantRecord, now time.Time) bool {
	if strings.TrimSpace(g.ExpiresAt) == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339, g.ExpiresAt)
	if err != nil {
		return true // fail-closed
	}
	return !now.Before(exp)
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
