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
	"fmt"
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

// DelegationRequest 一次「上级代授」（D13）的输入。
//
// ★ 存在的意义：`CheckDelegationNoOverflow` 断言的是 `DelegationAllowed(granter, grantee)`
// 的**返回值**，而生产侧此前**根本没有任何代码会构造这份授权** ——
// `authz.DelegationAllowed` 的唯一非测试调用点就是那条断言本身，
// 于是「代授不溢出自身范围」在实现侧同样**恒真**：没有代授路径，自然从不溢出。
// 本结构把「代授」变成一条真实可执行、且能落审计与来源分层的路径。
type DelegationRequest struct {
	// Granter 授出者账号。
	Granter string `json:"granter"`
	// Grantee 被授人账号。
	Grantee string `json:"grantee"`
	// Scope 被授出的内容（**只能收窄**，不得超出 Granter 自身权限）。
	Scope DelegationScope `json:"scope"`
	// DelegateAll true = 按 Granter 当前求值结果**整体**代授。
	// 此时审计留痕的 Scope 也取求值后的实际集合（同族祖先全展开），
	// 便于事后核对「到底授出去了什么」。
	DelegateAll bool `json:"delegateAll"`
	// At 代授发生时间（RFC3339；留痕用）。
	At string `json:"at,omitempty"`
}

// DelegationScope 代授内容（模块 / 密级 / 维度 / 勾选组 / 业务数值）。
type DelegationScope struct {
	Modules               []string           `json:"modules,omitempty"`
	MaxLevel              Level              `json:"maxLevel,omitempty"`
	Dimensions            []DimensionGrant   `json:"dimensions,omitempty"`
	DataUseGroups         []GroupScopeGrant  `json:"dataUseGroups,omitempty"`
	CanViewBusinessValues bool               `json:"canViewBusinessValues,omitempty"`
}

// Delegation 代授落库后的结果，供调用方落 Entitlement.Grants 与审计。
type Delegation struct {
	Granter string `json:"granter"`
	Grantee string `json:"grantee"`
	// Record 授权来源记录（Origin=SUPERVISOR_DELEGATE），
	// 其 UpperBoundRef 记授出者账号、GrantedBy 记具体执行人。
	Record GrantRecord `json:"record"`
	// Scope 实际授出的范围（DelegateAll 时已展开为具体集合）。
	Scope DelegationScope `json:"scope"`
}

// DelegationError 代授被拒的原因。
type DelegationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *DelegationError) Error() string { return e.Code + ": " + e.Message }

// 代授拒绝码。
const (
	ErrCodeDelegationOverflow = "DELEGATION_OVERFLOW"
	ErrCodeSameAccount        = "SAME_ACCOUNT"
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

// ScopeOverflow 校验「代授范围 ⊆ 授出者自身范围」（D13 核心不变量）。
//
// 与 DelegationAllowed 的分工：
//   - DelegationAllowed 吃两份 EntitlementView，适合已有求值结果时快速判定；
//   - ScopeOverflow 吃「授出者求值结果 + 代授草案 Scope」，适合**落库前**用
//     同一套口径复判，避免「先落库、后校验」的窗口期。
//
// 返回违规明细（空 = 全部合法），每条都带 docs/01 §13.3 的约束名。
func ScopeOverflow(granter *EntitlementView, s DelegationScope) []string {
	if granter == nil {
		return []string{"授出者求值结果为空，无法证明不溢出"}
	}
	var v []string

	// ① 业务数值：授出者自己看不见，就不得授出
	if s.CanViewBusinessValues && !granter.CanViewBusinessValues {
		v = append(v, "业务数值可见性溢出：授出者不可见业务数值，不得授出")
	}

	// ② 密级不得溢出
	if levelRank[s.MaxLevel] > levelRank[granter.MaxLevel] {
		v = append(v, fmt.Sprintf("密级溢出：授出 %s > 自身 %s", s.MaxLevel, granter.MaxLevel))
	}

	// ③ 模块不得溢出
	own := map[string]bool{}
	for _, m := range granter.Modules {
		own[m] = true
	}
	for _, m := range s.Modules {
		if !own[m] {
			v = append(v, "模块溢出：授出 "+m+"（授出者自身未持有）")
		}
	}

	// ④ 维度不得溢出
	for _, d := range s.Dimensions {
		gVals, ok := granter.Dimensions[d.Dim]
		if !ok {
			v = append(v, "维度溢出：授出者在该维度无任何取值（"+d.Dim+"）")
			continue
		}
		v = append(v, dimensionOverflow(d.Dim, gVals, d.Values)...)
	}

	// ⑤ 勾选组：组必须在授出者已生效组内；组级密级/模块/字段同样不得更宽。
	ownGroups := map[DataUseGroup]*GroupScopeGrant{}
	for i := range granter.GroupScopes {
		ownGroups[DataUseGroup(granter.GroupScopes[i].Group)] = &granter.GroupScopes[i]
	}
	ownGroupSet := map[DataUseGroup]bool{}
	for _, g := range granter.DataUseGroups {
		ownGroupSet[g] = true
	}
	for _, g := range s.DataUseGroups {
		key := DataUseGroup(g.Group)
		if !ownGroupSet[key] {
			v = append(v, "勾选组溢出：授出 "+g.Group+"（授出者自身未持有该组）")
			continue
		}
		ownG := ownGroups[key]
		if ownG == nil {
			continue // 授出者未声明组级限定 ⇒ 无上界可溢出
		}
		// 组内密级：授出者对该组声明了更严的 ScopedLevel 时不得超过它
		if ownG.ScopedLevel != "" && levelRank[g.MaxLevel] > levelRank[ownG.ScopedLevel] {
			v = append(v, fmt.Sprintf(
				"组内密级溢出：%s 授出 %s > 自身 %s", g.Group, g.MaxLevel, ownG.ScopedLevel))
		}
		// 字段 / 模块：空 = 组内全部；只允许收窄
		v = append(v, groupSubsetOverflow(g.Group, "字段", ownG.Fields, g.Fields)...)
		v = append(v, groupSubsetOverflow(g.Group, "模块", ownG.ScopedModules, g.ScopedModules)...)
		// 组内 DENY 只增不减：授出者被 DENY 的字段不得借代授洗白
		denied := map[string]bool{}
		for _, f := range ownG.Denied {
			denied[f] = true
		}
		granted := map[string]bool{}
		for _, f := range g.Fields {
			granted[f] = true
		}
		var washed []string
		for f := range denied {
			if granted[f] {
				washed = append(washed, f)
			}
		}
		sort.Strings(washed)
		for _, f := range washed {
			v = append(v, "组内 DENY 被翻案："+g.Group+"/"+f)
		}
	}
	return v
}

// dimensionOverflow 判定「请求的维度取值是否超出授出者持有」。
func dimensionOverflow(dim string, own, want []string) []string {
	allowed := map[string]bool{}
	for _, v := range own {
		allowed[v] = true
	}
	if allowed["*"] {
		return nil // 授出者持全部
	}
	var extra []string
	for _, w := range want {
		if !allowed[w] {
			extra = append(extra, w)
		}
	}
	sort.Strings(extra)
	var v []string
	for _, e := range extra {
		v = append(v, "维度溢出："+dim+"="+e+"（授出者未持有）")
	}
	return v
}

// groupSubsetOverflow 判定组内子集（字段 / 模块）是否超出；空 = 组内全部。
func groupSubsetOverflow(group, kind string, own, want []string) []string {
	if len(own) == 0 || len(want) == 0 {
		return nil // 任一方为空 ⇒ 「全部」，不存在收窄溢出
	}
	allowed := map[string]bool{}
	for _, x := range own {
		allowed[x] = true
	}
	var extra []string
	for _, x := range want {
		if !allowed[x] {
			extra = append(extra, x)
		}
	}
	sort.Strings(extra)
	var v []string
	for _, e := range extra {
		v = append(v, "组内"+kind+"溢出："+group+"/"+e)
	}
	return v
}

// Delegate 执行一次「上级代授」（D13）。
//
// 纪律（三条，缺一不可）：
//  1. **先证明不溢出，再落库**：溢出即拒，且**不返回任何结果**（调用方无从落库）。
//  2. **按求值结果代授，不按原始声明代授**：授出者自身被 DENY / 组依赖未满足的
//     内容，不会因为「模板里写了」就被授出去 —— 一律先 Resolve 取上界。
//  3. **留痕可溯源**：返回 GrantRecord（Origin=SUPERVISOR_DELEGATE、
//     UpperBoundRef=授出者账号），经 e.Grants 进入 source.fromDelegations。
//
// 自己授自己：直接拒（docs/01 §13.3「同级账号互不可见」）。
func (r *Resolver) Delegate(req DelegationRequest, granter *Entitlement, groupGrants []*Entitlement) (*Delegation, error) {
	if req.Granter == "" || req.Grantee == "" {
		return nil, &DelegationError{ErrCodeSameAccount, "授出者/被授人账号不得为空"}
	}
	if strings.EqualFold(req.Granter, req.Grantee) {
		return nil, &DelegationError{ErrCodeSameAccount, "不得给自己代授"}
	}
	if granter == nil {
		return nil, &DelegationError{ErrCodeDelegationOverflow, "授出者不存在"}
	}

	// ★ 一律按**求值结果**作为上界：原始声明里的 DENY / 未满足依赖都不算数。
	view := r.Resolve(granter, groupGrants)

	scope := req.Scope
	if req.DelegateAll {
		// 留痕必须记「实际生效的集合」，而非原始声明：
		// 否则审计看到的是「打算授出的」而不是「真的授出的」。
		scope = scopeFromView(view)
	}
	if overflow := ScopeOverflow(view, scope); len(overflow) > 0 {
		return nil, &DelegationError{
			ErrCodeDelegationOverflow,
			fmt.Sprintf("代授范围超出 %s 自身权限：%s", req.Granter, strings.Join(overflow, "; ")),
		}
	}

	rec := GrantRecord{
		Origin:        OriginSupervisor,
		GrantedBy:     req.Granter,
		UpperBoundRef: req.Granter,
		At:            req.At,
		ModuleIDs:     append([]string(nil), scope.Modules...),
	}
	for _, g := range scope.DataUseGroups {
		rec.GroupIDs = append(rec.GroupIDs, g.Group)
	}
	sort.Strings(rec.GroupIDs)

	return &Delegation{Granter: req.Granter, Grantee: req.Grantee, Record: rec, Scope: scope}, nil
}

// scopeFromView 把授出者的求值结果展开成可留痕的具体范围。
func scopeFromView(v *EntitlementView) DelegationScope {
	s := DelegationScope{
		Modules:               append([]string(nil), v.Modules...),
		MaxLevel:              v.MaxLevel,
		CanViewBusinessValues: v.CanViewBusinessValues,
	}
	for dim, vals := range v.Dimensions {
		s.Dimensions = append(s.Dimensions, DimensionGrant{
			Dim:                dim,
			Values:             append([]string(nil), vals...),
			IncludeDescendants: true,
		})
	}
	sort.Slice(s.Dimensions, func(i, j int) bool { return s.Dimensions[i].Dim < s.Dimensions[j].Dim })
	s.DataUseGroups = append(s.DataUseGroups, v.GroupScopes...)
	sort.Slice(s.DataUseGroups, func(i, j int) bool { return s.DataUseGroups[i].Group < s.DataUseGroups[j].Group })
	return s
}
