package gate_test

// G7 · 代授闭环（D13）——**链路级**闸门。
//
// ★ 为什么单列一个文件：`TestG7_DelegationNoOverflow` 断言的是
// `CheckDelegationNoOverflow`（即 `DelegationAllowed`）的返回值，而
// `DelegationAllowed` 在生产代码里**唯一**的调用点就是那条断言本身 ——
// 没有任何代码会真的构造一次代授。于是「代授不溢出自身范围」这条红线
// 在实现侧是**恒真**的：没有代授路径，自然从不溢出。
//
// 本文件补的是**链路断言**：从 `Resolver.Delegate` 入口进，以
// 「落库的 GrantRecord + Resolve 出的 EntitlementView.source」出。
// 覆盖三层：
//  1. 溢出即拒（且不产出可落库的结果）；
//  2. 拒的是求值后的真实上界，不是原始声明（DENY / 组依赖 / 时间盒都不许绕过）；
//  3. 合法代授真的能落库，并在 source.fromDelegations 里可溯源。
//
// docs/01 §13.3「可授出集合(账号 A) ⊆ 账号 A 自身权限集合」；docs/05 G7。

import (
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// g7DelegResolver 造一个带固定时钟的求值器，便于时间盒用例可复现。
func g7DelegResolver(now time.Time) *authz.Resolver {
	r := authz.NewResolver(map[string]*authz.Entitlement{})
	r.Now = func() time.Time { return now }
	return r
}

// g7Granter 一个「上级」账号：持 m.ops/m.roi、L3、品牌 KONVY、含一组 grp.ops。
func g7Granter() *authz.Entitlement {
	return &authz.Entitlement{
		Account:      "t1.lead",
		MaxLevel:     authz.L3,
		Modules:      authz.ModuleGrant{Enabled: []string{"m.ops", "m.roi"}},
		Dimensions:   []authz.DimensionGrant{{Dim: "brand", Values: []string{"KONVY"}}},
		DataUseGroups: []authz.GroupScopeGrant{{
			Group:  string(authz.GrpOps),
			Fields: []string{"gp", "aov"},
		}},
	}
}

// ────────────────── ① 溢出即拒 ──────────────────

// 五种溢出形态逐一注入，全部必须被拒；且**不得返回结果**（无从落库）。
func TestG7_Delegate_OverflowRejected(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := g7DelegResolver(now)

	cases := []struct {
		name  string
		scope authz.DelegationScope
		want  string // 期望出现在错误里的关键词
	}{
		{
			name:  "模块越权",
			scope: authz.DelegationScope{Modules: []string{"m.ops", "m.secret"}},
			want:  "模块溢出",
		},
		{
			name:  "密级越权",
			scope: authz.DelegationScope{MaxLevel: authz.L4},
			want:  "密级溢出",
		},
		{
			name: "维度越权",
			scope: authz.DelegationScope{
				Dimensions: []authz.DimensionGrant{{Dim: "brand", Values: []string{"OTHER"}}},
			},
			want: "维度溢出",
		},
		{
			name: "业务数值越权",
			scope: authz.DelegationScope{
				MaxLevel:              authz.L2,
				CanViewBusinessValues: true,
			},
			want: "业务数值可见性溢出",
		},
		{
			name: "勾选组越权",
			scope: authz.DelegationScope{
				MaxLevel: authz.L2,
				DataUseGroups: []authz.GroupScopeGrant{{
					Group: string(authz.GrpROI), MaxLevel: authz.L2,
				}},
			},
			want: "勾选组溢出",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := r.Delegate(authz.DelegationRequest{
				Granter: "t1.lead", Grantee: "t3.member", Scope: tc.scope,
			}, g7Granter(), nil)
			if err == nil {
				t.Fatalf("G7 溢出代授必须被拒，实际放行: %+v", d)
			}
			if d != nil {
				t.Fatalf("G7 被拒的代授不得产出可落库结果（有结果就会被落库）")
			}
			var de *authz.DelegationError
			if !asDelegationError(err, &de) {
				t.Fatalf("G7 拒绝原因应为 DelegationError，实际 %T: %v", err, err)
			}
			if de.Code != authz.ErrCodeDelegationOverflow {
				t.Fatalf("G7 拒绝码应为 %s，实际 %s", authz.ErrCodeDelegationOverflow, de.Code)
			}
			if !strings.Contains(de.Message, tc.want) {
				t.Fatalf("G7 拒绝原因应含 %q，实际 %q", tc.want, de.Message)
			}
		})
	}
}

// 不得给自己代授（docs/01 §13.3「同级账号互不可见」的最小推论）。
func TestG7_Delegate_SelfDelegationRejected(t *testing.T) {
	r := g7DelegResolver(time.Now())
	d, err := r.Delegate(authz.DelegationRequest{
		Granter: "t1.lead", Grantee: "t1.lead", DelegateAll: true,
	}, g7Granter(), nil)
	if err == nil || d != nil {
		t.Fatalf("G7 自己给自己代授必须被拒，实际 err=%v d=%+v", err, d)
	}
}

// 边界：严格**等于**上界不算溢出（收窄到极致 = 不缩也不涨）。
//
// ★ 夹具设计要点：granter 的**原始声明**必须与**求值结果**可区分 ——
// 否则「留痕记求值结果」与「留痕记原始声明」两种实现都会绿，
// 这条断言就退化成恒真。（本用例首版就踩了这个坑，见文件末注释。）
func TestG7_Delegate_ExactBoundaryAllowed(t *testing.T) {
	r := g7DelegResolver(time.Now())
	g := &authz.Entitlement{
		Account:  "t1.lead",
		MaxLevel: authz.L3,
		// 原始声明 3 个模块，但其中 m.secret 被 DENY 掉 ⇒ 求值结果只有 2 个
		Modules: authz.ModuleGrant{Enabled: []string{"m.ops", "m.roi", "m.secret"},
			Disabled: []string{"m.secret"}},
		Dimensions: []authz.DimensionGrant{{Dim: "brand", Values: []string{"KONVY"}}},
	}
	// 自证夹具成立
	view := r.Resolve(g, nil)
	if len(view.Modules) != 2 {
		t.Fatalf("夹具自证失败：求值结果应为 2 个模块，实际 %v", view.Modules)
	}

	d, err := r.Delegate(authz.DelegationRequest{
		Granter: "t1.lead", Grantee: "t3.member", DelegateAll: true,
	}, g, nil)
	if err != nil {
		t.Fatalf("G7 等价代授（收窄=上界）不应被拒: %v", err)
	}
	if d == nil {
		t.Fatal("G7 合法代授必须产出结果")
	}
	// ★ DelegateAll 的留痕必须是**求值结果**（2 个模块），不是原始声明（3 个）。
	//   记原始声明 = 审计看到「打算授出的」而非「真的授出的」——
	//   而被 DENY 的 m.secret 其实授不出去。
	for _, m := range d.Scope.Modules {
		if m == "m.secret" {
			t.Fatalf("G7 DelegateAll 留痕混入被 DENY 的模块（记了原始声明而非求值结果）: %v",
				d.Scope.Modules)
		}
	}
	if len(d.Scope.Modules) != 2 {
		t.Fatalf("G7 DelegateAll 留痕应为求值结果（2 个模块），实际 %v", d.Scope.Modules)
	}
	if d.Scope.MaxLevel != authz.L3 {
		t.Fatalf("G7 DelegateAll 密级应为求值结果 L3，实际 %s", d.Scope.MaxLevel)
	}
}

// ────────────────── ② 拒的是「求值后的真实上界」 ──────────────────

// ★ 最关键的一条：授出者原始声明里**有** m.secret，但被 DENY 掉了。
// 断言代授 m.secret 仍必须被拒 —— 即上界取自 Resolve 结果，而非原始声明。
// 若实现图省事直接信 granter.Modules（原始声明），本用例会红。
func TestG7_Delegate_UpperBoundIsResolvedNotDeclared(t *testing.T) {
	r := g7DelegResolver(time.Now())
	g := g7Granter()
	// 原始声明里写了 m.secret，但同一份 Entitlement 里显式 DENY 它。
	g.Modules.Enabled = append(g.Modules.Enabled, "m.secret")
	g.Modules.Disabled = []string{"m.secret"}

	// 先自证夹具成立：DENY 之后求值结果里确实没有 m.secret。
	view := r.Resolve(g, nil)
	for _, m := range view.Modules {
		if m == "m.secret" {
			t.Fatalf("夹具自证失败：DENY 未生效，求值结果仍含 m.secret")
		}
	}

	d, err := r.Delegate(authz.DelegationRequest{
		Granter: "t1.lead", Grantee: "t3.member",
		Scope: authz.DelegationScope{Modules: []string{"m.secret"}},
	}, g, nil)
	if err == nil || d != nil {
		t.Fatalf("G7 被 DENY 的模块不得借代授洗白，实际 err=%v d=%+v", err, d)
	}
}

// 组依赖未满足而被剔除的组，同样不得借代授授出。
func TestG7_Delegate_GroupDepsNotBypassable(t *testing.T) {
	r := g7DelegResolver(time.Now())
	// grp.roi 依赖 grp.cost_profit + grp.inventory；授出者只有 grp.roi。
	g := &authz.Entitlement{
		Account:  "t1.lead",
		MaxLevel: authz.L3,
		DataUseGroups: []authz.GroupScopeGrant{{
			Group: string(authz.GrpROI), MaxLevel: authz.L2,
		}},
	}
	view := r.Resolve(g, nil)
	for _, grp := range view.DataUseGroups {
		if grp == authz.GrpROI {
			t.Fatalf("夹具自证失败：依赖未满足的 grp.roi 未从求值结果剔除")
		}
	}

	d, err := r.Delegate(authz.DelegationRequest{
		Granter: "t1.lead", Grantee: "t3.member",
		Scope: authz.DelegationScope{
			MaxLevel:      authz.L2,
			DataUseGroups: []authz.GroupScopeGrant{{Group: string(authz.GrpROI), MaxLevel: authz.L2}},
		},
	}, g, nil)
	if err == nil || d != nil {
		t.Fatalf("G7 依赖未满足的组不得借代授授出，实际 err=%v d=%+v", err, d)
	}
}

// 组内 DENY 不得被代授翻案（DENY 优先，永不被下级覆盖）。
func TestG7_Delegate_GroupDenyNotWashed(t *testing.T) {
	r := g7DelegResolver(time.Now())
	g := &authz.Entitlement{
		Account:  "t1.lead",
		MaxLevel: authz.L3,
		DataUseGroups: []authz.GroupScopeGrant{{
			Group:  string(authz.GrpOps),
			Fields: []string{"gp", "aov"},
			Denied: []string{"gp"}, // 组内把 gp 禁掉
		}},
	}
	// 被授人如果想要 gp ⇒ 等于把上界里被 DENY 的字段授出去
	d, err := r.Delegate(authz.DelegationRequest{
		Granter: "t1.lead", Grantee: "t3.member",
		Scope: authz.DelegationScope{
			MaxLevel: authz.L2,
			DataUseGroups: []authz.GroupScopeGrant{{
				Group: string(authz.GrpOps), MaxLevel: authz.L2, Fields: []string{"gp"},
			}},
		},
	}, g, nil)
	if err == nil || d != nil {
		t.Fatalf("G7 组内 DENY 字段不得借代授翻案，实际 err=%v d=%+v", err, d)
	}
}

// 已过时间盒的来源不算上界：授出者的临时授权过期后，不得再往下代授。
func TestG7_Delegate_ExpiredGranterScopeIsNotUpperBound(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := g7DelegResolver(now)
	g := &authz.Entitlement{
		Account:  "t1.lead",
		MaxLevel: authz.L3,
		// 唯一带来 m.roi 的来源已在 1 小时前到期
		Grants: []authz.GrantRecord{{
			Origin:    authz.OriginRequest,
			GrantedBy: "t0.admin",
			At:        now.Add(-2 * time.Hour).Format(time.RFC3339),
			ExpiresAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
			ModuleIDs: []string{"m.roi"},
		}},
	}
	view := r.Resolve(g, nil)
	for _, m := range view.Modules {
		if m == "m.roi" {
			t.Fatalf("夹具自证失败：过期来源仍在生效（m.roi 出现）")
		}
	}

	d, err := r.Delegate(authz.DelegationRequest{
		Granter: "t1.lead", Grantee: "t3.member",
		Scope: authz.DelegationScope{Modules: []string{"m.roi"}},
	}, g, nil)
	if err == nil || d != nil {
		t.Fatalf("G7 已过期的授权不得作为上界继续下授，实际 err=%v d=%+v", err, d)
	}
}

// ────────────────── ③ 合法代授落库后可溯源 ──────────────────

// ★ 闭环断言：Delegate 产出的 GrantRecord 挂到被授人身上后，
// Resolve 必须把它如实分层进 source.fromDelegations
// （此前该桶**恒为空数组**：字段存在、接口不报错、永远 []）。
func TestG7_Delegate_LandsInSourceDelegations(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := g7DelegResolver(now)

	d, err := r.Delegate(authz.DelegationRequest{
		Granter: "t1.lead", Grantee: "t3.member",
		At: now.Format(time.RFC3339),
		Scope: authz.DelegationScope{
			Modules:  []string{"m.ops"},
			MaxLevel: authz.L2,
			Dimensions: []authz.DimensionGrant{
				{Dim: "brand", Values: []string{"KONVY"}},
			},
		},
	}, g7Granter(), nil)
	if err != nil {
		t.Fatalf("G7 合法代授不应被拒: %v", err)
	}

	// 审计字段必须齐：来源=上级代授、上界=授出者、留痕时间不为空。
	if d.Record.Origin != authz.OriginSupervisor {
		t.Fatalf("G7 来源应为 %s，实际 %s", authz.OriginSupervisor, d.Record.Origin)
	}
	if d.Record.UpperBoundRef != "t1.lead" {
		t.Fatalf("G7 上界应记为授出者 t1.lead，实际 %q", d.Record.UpperBoundRef)
	}
	if d.Record.At == "" {
		t.Fatal("G7 代授必须留痕时间（否则审计查不出何时授的）")
	}

	// 挂到被授人 → Resolve → fromDelegations 非空且可读。
	grantee := &authz.Entitlement{
		Account:  "t3.member",
		MaxLevel: authz.L1,
		Grants:   []authz.GrantRecord{d.Record},
	}
	view := r.Resolve(grantee, nil)
	if len(view.Source.FromDelegation) == 0 {
		t.Fatal("G7 代授必须出现在 source.fromDelegations（此前恒为空数组）")
	}
	joined := strings.Join(view.Source.FromDelegation, ",")
	if !strings.Contains(joined, "delegate:t1.lead") {
		t.Fatalf("G7 代授来源应含 delegate:t1.lead，实际 %q", joined)
	}
	// 代授不得混进「已批准申请」桶（两个桶的追责路径完全不同）。
	for _, s := range view.Source.FromApproved {
		if strings.Contains(s, "t1.lead") {
			t.Fatalf("G7 代授混入 fromApprovedRequests: %q", s)
		}
	}
}

// 被授者视图中不得出现任何「授出者自己都没有」的项（闭环终检）。
func TestG7_Delegate_GranteeViewSubsetOfGranterView(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := g7DelegResolver(now)

	d, err := r.Delegate(authz.DelegationRequest{
		Granter: "t1.lead", Grantee: "t3.member",
		Scope: authz.DelegationScope{
			Modules:  []string{"m.ops"},
			MaxLevel: authz.L2,
		},
	}, g7Granter(), nil)
	if err != nil {
		t.Fatalf("G7 合法代授不应被拒: %v", err)
	}

	granterView := r.Resolve(g7Granter(), nil)
	granteeView := r.Resolve(&authz.Entitlement{
		Account:  "t3.member",
		MaxLevel: d.Scope.MaxLevel,
		Modules:  authz.ModuleGrant{Enabled: d.Scope.Modules},
		Grants:   []authz.GrantRecord{d.Record},
	}, nil)

	own := map[string]bool{}
	for _, m := range granterView.Modules {
		own[m] = true
	}
	for _, m := range granteeView.Modules {
		if !own[m] {
			t.Fatalf("G7 被授人拿到授出者没有的模块: %s", m)
		}
	}
	if levelRankOf(granteeView.MaxLevel) > levelRankOf(granterView.MaxLevel) {
		t.Fatalf("G7 被授人密级 %s 超出授出者 %s",
			granteeView.MaxLevel, granterView.MaxLevel)
	}
}

// 纯函数侧：ScopeOverflow 对 nil 上界必须 fail-closed（不是"跳过校验"）。
func TestG7_ScopeOverflow_NilGranterFailsClosed(t *testing.T) {
	if v := authz.ScopeOverflow(nil, authz.DelegationScope{Modules: []string{"m.ops"}}); len(v) == 0 {
		t.Fatal("G7 上界不可得时必须 fail-closed 拒绝，不得静默放行")
	}
}

// 纯函数侧：gate.CheckDelegationNoOverflow 与 authz 判定同源（防两套口径漂移）。
func TestG7_DelegationGateMatchesAuthz(t *testing.T) {
	granter := &authz.EntitlementView{
		Modules: []string{"m.ops"}, MaxLevel: authz.L3,
	}
	grantees := []*authz.EntitlementView{
		{Modules: []string{"m.ops"}, MaxLevel: authz.L2},  // 合法
		{Modules: []string{"m.secret"}, MaxLevel: authz.L2}, // 模块越权
		{Modules: []string{"m.ops"}, MaxLevel: authz.L4},  // 密级越权
	}
	for i, g := range grantees {
		want := authz.DelegationAllowed(granter, g)
		v := gate.CheckDelegationNoOverflow([]gate.DelegationCase{{
			Name: "c", Granter: granter, Grantee: g, ExpectAllowed: want,
		}})
		if len(v) != 0 {
			t.Fatalf("G7 case#%d 闸门与 authz 判定漂移: %v", i, v)
		}
	}
}

// ────────────────── 小工具 ──────────────────
//
// ★ 本轮（2026-10-06 22:12）自测逼出的**夹具缺陷**，留作警示：
//   `TestG7_Delegate_ExactBoundaryAllowed` 首版用的 granter，其**原始声明**
//   与**求值结果**恰好相同（3 个模块都没被 DENY、组依赖也都满足）。
//   于是「留痕记求值结果」与「留痕记原始声明」两种实现都能通过 ——
//   断言退化成恒真，注入「改用原始声明」的破坏时**不报错**。
//   修法：让夹具的声明与结果**可区分**（声明 3 个模块、DENY 掉 1 个 ⇒ 结果 2 个），
//   并在用例开头 `自证夹具成立`。修复后该破坏被准确拦下。
//   教训：负向自测的价值不止"证明实现被改坏会响"，还能暴露**断言本身太弱**。

func asDelegationError(err error, out **authz.DelegationError) bool {
	de, ok := err.(*authz.DelegationError)
	if ok {
		*out = de
	}
	return ok
}

func levelRankOf(l authz.Level) int {
	switch l {
	case authz.L4:
		return 4
	case authz.L3:
		return 3
	case authz.L2:
		return 2
	case authz.L1:
		return 1
	}
	return 0
}
