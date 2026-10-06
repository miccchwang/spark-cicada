// groups_test.go —— M-GROUP / M-REQ 的 HTTP 层测试。
//
// 本文件测的**不是**业务逻辑（那在 group / req 包里已穷举），
// 而是接口层**独有的**那几类风险：
//
//   1. 越权：能不能伪装成别人？（body 里塞 account / applicant）
//   2. 鉴权：非本组管理员能不能改组？非本审批人能不能批？
//   3. 信息泄露：别人能不能读到不该看的申请单 / 组定义？
//   4. 参数校验：必填缺失时是否明确 4xx，而不是 500 或静默成功？
//
// 用内存替身而非真库：这些断言与 SQL 无关，拖一个 Postgres 起来只会
// 让测试变慢、变脆，且淹没了「这层到底在防什么」的意图。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/group"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

// ───────────────────────────── 内存替身 ─────────────────────────────

type fakeSvc struct {
	groups  map[string]*group.Group
	person  map[string]*group.Personal
	reqs    map[string]*req.Request
	members map[string][]group.Membership
	saved   int // SaveRequest 被调用次数（断言「拒绝的请求不该落库」）
}

func newFakeSvc() *fakeSvc {
	return &fakeSvc{
		groups:  map[string]*group.Group{},
		person:  map[string]*group.Personal{},
		reqs:    map[string]*req.Request{},
		members: map[string][]group.Membership{},
	}
}

func (f *fakeSvc) LoadGroup(_ context.Context, id string) (*group.Group, error) {
	g, ok := f.groups[id]
	if !ok {
		return nil, errNotFound("group not found")
	}
	return g, nil
}

func (f *fakeSvc) LoadGroupsFor(_ context.Context, account string) ([]*group.Group, []group.Membership, error) {
	var out []*group.Group
	for _, m := range f.members[account] {
		if g, ok := f.groups[m.GroupID]; ok {
			out = append(out, g)
		}
	}
	return out, f.members[account], nil
}

func (f *fakeSvc) LoadPersonal(_ context.Context, account string) (*group.Personal, error) {
	if p, ok := f.person[account]; ok {
		return p, nil
	}
	return &group.Personal{Account: account, MaxLevel: group.L1}, nil
}

func (f *fakeSvc) UpsertGroup(_ context.Context, g *group.Group) error {
	f.groups[g.ID] = g
	return nil
}

func (f *fakeSvc) ReplaceGrants(_ context.Context, id string, grants []group.Grant, actor, _ string) error {
	g, ok := f.groups[id]
	if !ok {
		return errNotFound("group not found")
	}
	g.Grants = grants
	return nil
}

func (f *fakeSvc) SetMembership(_ context.Context, m group.Membership) error {
	f.members[m.Account] = append(f.members[m.Account], m)
	return nil
}

func (f *fakeSvc) RemoveMembership(_ context.Context, gid, acct string) error {
	out := f.members[acct][:0]
	for _, m := range f.members[acct] {
		if m.GroupID != gid {
			out = append(out, m)
		}
	}
	f.members[acct] = out
	return nil
}

func (f *fakeSvc) SaveRequest(_ context.Context, r *req.Request) error {
	f.saved++
	f.reqs[r.ID] = r
	return nil
}

func (f *fakeSvc) LoadRequest(_ context.Context, id string) (*req.Request, error) {
	r, ok := f.reqs[id]
	if !ok {
		return nil, errNotFound("request not found")
	}
	return r, nil
}

func (f *fakeSvc) ListPendingFor(_ context.Context, approver string, _ int) ([]string, error) {
	var out []string
	for id, r := range f.reqs {
		for _, a := range r.Approvals {
			if a.Approver == approver && a.Action == "PENDING" {
				out = append(out, id)
			}
		}
	}
	return out, nil
}

func (f *fakeSvc) ListRequestsByStatus(_ context.Context, _ req.Status, _ int) ([]string, error) {
	return nil, nil
}

func (f *fakeSvc) ListReclaimable(_ context.Context, _ time.Time, _ int) ([]string, error) {
	return nil, nil
}

func (f *fakeSvc) ListCosignPending(_ context.Context) (map[string][]string, error) {
	out := map[string][]string{}
	for id, r := range f.reqs {
		if r.Status != req.StatusCosignPending {
			continue
		}
		for _, c := range r.CCs {
			if c.Mode == chain.CcCosign && c.DecidedAt == nil {
				out[id] = append(out[id], c.Cc)
			}
		}
	}
	return out, nil
}

type errNotFound string

func (e errNotFound) Error() string { return string(e) }

// ───────────────────────────── 夹具 ─────────────────────────────

// newTestServer 组装一个带两级组织与固定时钟的 handler 集合。
//
// 组织：u.lead(T1,L4,all) → u.staff(T3,L1,report)
// 组：   grp.a（owner = u.lead）
func newTestServer(t *testing.T) (*GroupHandlers, *fakeSvc) {
	t.Helper()
	svc := newFakeSvc()

	org := chain.NewOrgDirectory()
	org.Put(&chain.OrgNode{Account: "u.lead", Tier: "T1", CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "u.staff", Supervisor: "u.lead", Tier: "T3",
		PrimaryDept: "d.ops", Active: true})
	org.Put(&chain.OrgNode{Account: "u.other", Supervisor: "u.lead", Tier: "T2",
		PrimaryDept: "d.fin", Active: true})

	ents := map[string]*authz.Entitlement{
		"u.lead": {
			Account: "u.lead", BaseTemplate: "tpl.admin",
			Modules:    authz.ModuleGrant{Enabled: []string{"module.report", "module.pnl"}},
			Dimensions: []authz.DimensionGrant{{Dim: "channel", Values: []string{"*"}}},
			MaxLevel:   authz.L4,
		},
		"u.staff": {
			Account: "u.staff", BaseTemplate: "tpl.base",
			Modules:    authz.ModuleGrant{Enabled: []string{"module.report"}},
			Dimensions: []authz.DimensionGrant{{Dim: "channel", Values: []string{"TK-TH"}}},
			MaxLevel:   authz.L1,
		},
	}
	res := authz.NewResolver(nil)
	svcReq := req.NewService(org, res, func(a string) (*authz.Entitlement, []*authz.Entitlement) {
		if e, ok := ents[a]; ok {
			return e, nil
		}
		return &authz.Entitlement{Account: a, MaxLevel: authz.L1}, nil
	})
	svcReq.Now = func() time.Time { return time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC) }

	svc.groups["grp.a"] = &group.Group{
		ID: "grp.a", Name: "A 组", Owners: []string{"u.lead"},
		Grants: []group.Grant{{Kind: group.KindModule, Key: "module.report"}},
	}

	h := &GroupHandlers{
		Svc:     svc,
		Req:     svcReq,
		IsAdmin: func(a string) bool { return a == "u.lead" },
	}
	return h, svc
}

func do(h http.HandlerFunc, method, target, account string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, target, &buf)
	if account != "" {
		r.Header.Set("X-Spark-Account", account)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// ───────────────────────────── 身份：必须取自请求头 ─────────────────────────────

// ★★ 最重要的一条：申请人身份必须取自请求头，body 里的 applicant 必须被忽略。
//
// 若这条挂了，任何登录用户都能以「某高管的上级」名义提交申请 ——
// 而申请人决定了整条审批路由，等于绕开审批。
func TestHTTP_SubmitRequest_IgnoresBodyApplicant(t *testing.T) {
	h, svc := newTestServer(t)

	body := map[string]any{
		"id":        "req.spoof",
		"applicant": "u.lead", // ← 伪造：实际登录人是 u.staff
		"draft":     map[string]any{"modules": []string{"module.pnl"}, "maxLevel": "L1"},
		"purpose":   "越权测试",
	}
	w := do(h.handleSubmitRequest, http.MethodPost, "/api/requests", "u.staff", body)
	if w.Code != http.StatusOK {
		t.Fatalf("应成功，实际 %d %s", w.Code, w.Body.String())
	}
	got := svc.reqs["req.spoof"]
	if got == nil {
		t.Fatal("申请单未落库")
	}
	if got.Applicant != "u.staff" {
		t.Fatalf("★ 越权：申请人被 body 里的 %q 覆盖了，实际记为 %q", "u.lead", got.Applicant)
	}
}

// ★ 无身份头 ⇒ 401，且**不得落库**。
func TestHTTP_SubmitRequest_MissingIdentity(t *testing.T) {
	h, svc := newTestServer(t)
	w := do(h.handleSubmitRequest, http.MethodPost, "/api/requests", "", map[string]any{
		"id": "req.anon", "purpose": "x",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无身份应 401，实际 %d", w.Code)
	}
	if svc.saved != 0 {
		t.Fatal("★ 未鉴权的请求绝不该落库")
	}
}

// ★ purpose 必填：空用途会让审批人无法判断申请理由。
func TestHTTP_SubmitRequest_PurposeRequired(t *testing.T) {
	h, svc := newTestServer(t)
	w := do(h.handleSubmitRequest, http.MethodPost, "/api/requests", "u.staff", map[string]any{
		"id": "req.nopurpose",
		"draft": map[string]any{"modules": []string{"module.report"}, "maxLevel": "L1"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 purpose 应 400，实际 %d", w.Code)
	}
	if svc.saved != 0 {
		t.Fatal("参数不合法的请求不该落库")
	}
}

// ★ 路由失败（链上无人覆盖）必须原样报错，不得吞掉后放行。
func TestHTTP_SubmitRequest_NoCoverFailsClosed(t *testing.T) {
	h, svc := newTestServer(t)
	w := do(h.handleSubmitRequest, http.MethodPost, "/api/requests", "u.staff", map[string]any{
		"id":      "req.nocover",
		"draft":   map[string]any{"modules": []string{"module.secret"}, "maxLevel": "L1"},
		"purpose": "无人可批",
	})
	if w.Code == http.StatusOK {
		t.Fatal("★ fail-closed 违反：无人覆盖却返回 200")
	}
	if svc.saved != 0 {
		t.Fatal("★ 路由失败的申请不该落库")
	}
}

// ───────────────────────────── 鉴权：非管理员不得改组 ─────────────────────────────

// ★ 非本组 owner、且非全局管理员 ⇒ 改授权必须 403。
func TestHTTP_ReplaceGrants_ForbiddenForOutsider(t *testing.T) {
	h, svc := newTestServer(t)
	w := do(h.handleReplaceGrants, http.MethodPut, "/api/groups/grants", "u.staff", map[string]any{
		"groupId": "grp.a",
		"grants":  []map[string]any{{"Kind": "module", "Key": "module.pnl"}},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("非管理员应 403，实际 %d %s", w.Code, w.Body.String())
	}
	// 原授权不得被改动
	if len(svc.groups["grp.a"].Grants) != 1 {
		t.Fatalf("★ 越权修改成功：授权变成了 %+v", svc.groups["grp.a"].Grants)
	}
}

// 本组 owner 可以改。
func TestHTTP_ReplaceGrants_AllowedForOwner(t *testing.T) {
	h, _ := newTestServer(t)
	w := do(h.handleReplaceGrants, http.MethodPut, "/api/groups/grants", "u.lead", map[string]any{
		"groupId": "grp.a",
		"grants":  []map[string]any{{"Kind": "module", "Key": "module.report", "Values": []string{}, "Deny": false}},
		"reason":  "正常调整",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("owner 应可改，实际 %d %s", w.Code, w.Body.String())
	}
}

// ★ 非法授权定义（kind 与 key 前缀不匹配）必须 422，且不得写库。
func TestHTTP_ReplaceGrants_InvalidGrantRejected(t *testing.T) {
	h, svc := newTestServer(t)
	w := do(h.handleReplaceGrants, http.MethodPut, "/api/groups/grants", "u.lead", map[string]any{
		"groupId": "grp.a",
		// kind=module 但 key 写成 dimension.* ⇒ 前缀不匹配
		"grants": []map[string]any{{"Kind": "module", "Key": "dimension.channel"}},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法授权应 422，实际 %d %s", w.Code, w.Body.String())
	}
	if len(svc.groups["grp.a"].Grants) != 1 {
		t.Fatalf("★ 非法授权竟被写库：%+v", svc.groups["grp.a"].Grants)
	}
}

// ★ 读组定义也要鉴权 —— 否则任意用户可枚举「公司有哪些组、各组给什么权限」。
func TestHTTP_LoadGroup_ForbiddenForOutsider(t *testing.T) {
	h, _ := newTestServer(t)
	w := do(h.handleLoadGroup, http.MethodGet, "/api/groups/get?id=grp.a", "u.staff", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("非管理员读组定义应 403，实际 %d", w.Code)
	}
	// 对照：owner 可读
	w2 := do(h.handleLoadGroup, http.MethodGet, "/api/groups/get?id=grp.a", "u.lead", nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("owner 应可读，实际 %d", w2.Code)
	}
}

// ───────────────────────────── 审批：非审批人不得批 ─────────────────────────────

// ★ 非本单审批人点「批准」必须 403。
//
// 这一条的背景：req.Approve 会推进状态，但它只在**匹配到**审批步骤时才写
// 步骤记录；若接口层不先校验「他是不是审批人」，一个不匹配的人点下去
// 仍会把单子推成 APPROVED —— 权限就白批了。
func TestHTTP_Approve_OnlyDesignatedApprover(t *testing.T) {
	h, svc := newTestServer(t)

	// 造一张待 u.lead 批的单
	svc.reqs["req.p1"] = &req.Request{
		ID: "req.p1", Applicant: "u.staff",
		Status: req.StatusApproving,
		Approvals: []req.ApprovalStep{
			{Approver: "u.lead", Tier: "T1", Action: "PENDING"},
		},
	}

	// u.other 不是审批人 ⇒ 403，状态不变
	w := do(h.handleApprove, http.MethodPost, "/api/requests/approve", "u.other",
		map[string]any{"id": "req.p1", "reason": "我批一下"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("非审批人应 403，实际 %d %s", w.Code, w.Body.String())
	}
	if svc.reqs["req.p1"].Status != req.StatusApproving {
		t.Fatalf("★ 越权审批成功：状态变成了 %s", svc.reqs["req.p1"].Status)
	}

	// 真正的审批人可以
	w2 := do(h.handleApprove, http.MethodPost, "/api/requests/approve", "u.lead",
		map[string]any{"id": "req.p1", "reason": "同意"})
	if w2.Code != http.StatusOK {
		t.Fatalf("审批人应可批，实际 %d %s", w2.Code, w2.Body.String())
	}
	if svc.reqs["req.p1"].Status != req.StatusApproved {
		t.Fatalf("应为 APPROVED，实际 %s", svc.reqs["req.p1"].Status)
	}
}

// ★ 会签未决时审批必须被拒（409），状态不得推进。
func TestHTTP_Approve_BlockedWhileCosignPending(t *testing.T) {
	h, svc := newTestServer(t)
	svc.reqs["req.c1"] = &req.Request{
		ID: "req.c1", Applicant: "u.staff",
		Status: req.StatusCosignPending,
		Approvals: []req.ApprovalStep{
			{Approver: "u.lead", Tier: "T1", Action: "PENDING"},
		},
		CCs: []chain.CcRecord{
			{Cc: "u.other", Reason: "L4", Mode: chain.CcCosign,
				NotifiedAt: time.Now()}, // DecidedAt == nil ⇒ 未表态
		},
	}
	w := do(h.handleApprove, http.MethodPost, "/api/requests/approve", "u.lead",
		map[string]any{"id": "req.c1"})
	if w.Code != http.StatusConflict {
		t.Fatalf("会签未决应 409，实际 %d %s", w.Code, w.Body.String())
	}
	if svc.reqs["req.c1"].Status != req.StatusCosignPending {
		t.Fatalf("★ 状态被推进了：%s", svc.reqs["req.c1"].Status)
	}
}

// ───────────────────────────── 撤回：只有本人 ─────────────────────────────

func TestHTTP_Withdraw_OnlyApplicant(t *testing.T) {
	h, svc := newTestServer(t)
	svc.reqs["req.w1"] = &req.Request{
		ID: "req.w1", Applicant: "u.staff", Status: req.StatusApproving,
		Approvals: []req.ApprovalStep{{Approver: "u.lead", Tier: "T1", Action: "PENDING"}},
	}
	// 别人撤 ⇒ 403
	w := do(h.handleWithdraw, http.MethodPost, "/api/requests/withdraw", "u.other",
		map[string]any{"id": "req.w1"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("非申请人撤回应 403，实际 %d", w.Code)
	}
	if svc.reqs["req.w1"].Status != req.StatusApproving {
		t.Fatalf("★ 被他人撤回了：%s", svc.reqs["req.w1"].Status)
	}
	// 本人撤 ⇒ 成功
	w2 := do(h.handleWithdraw, http.MethodPost, "/api/requests/withdraw", "u.staff",
		map[string]any{"id": "req.w1"})
	if w2.Code != http.StatusOK {
		t.Fatalf("本人应可撤回，实际 %d %s", w2.Code, w2.Body.String())
	}
	if svc.reqs["req.w1"].Status != req.StatusWithdrawn {
		t.Fatalf("应为 WITHDRAWN，实际 %s", svc.reqs["req.w1"].Status)
	}
}

// ───────────────────────────── 可见性 ─────────────────────────────

// ★ 申请单只对「申请人 / 审批人 / 抄送人 / 管理员」可见。
func TestHTTP_GetRequest_Visibility(t *testing.T) {
	h, svc := newTestServer(t)
	svc.reqs["req.v1"] = &req.Request{
		ID: "req.v1", Applicant: "u.staff", Status: req.StatusApproving,
		Purpose:   "含内部信息",
		Approvals: []req.ApprovalStep{{Approver: "u.lead", Tier: "T1", Action: "PENDING"}},
		CCs:       []chain.CcRecord{{Cc: "u.cc", Reason: "+2", Mode: chain.CcNotify}},
	}
	cases := []struct {
		account string
		want    int
	}{
		{"u.staff", http.StatusOK},    // 申请人
		{"u.lead", http.StatusOK},     // 审批人
		{"u.cc", http.StatusOK},       // 抄送人
		{"u.other", http.StatusForbidden}, // 无关者
	}
	for _, c := range cases {
		w := do(h.handleGetRequest, http.MethodGet, "/api/requests?id=req.v1", c.account, nil)
		if w.Code != c.want {
			t.Fatalf("%s 读申请单应为 %d，实际 %d", c.account, c.want, w.Code)
		}
	}
}

// ★ 会签人对「非自己所在单」不得会签。
func TestHTTP_Cosign_RejectsNonCc(t *testing.T) {
	h, svc := newTestServer(t)
	svc.reqs["req.c2"] = &req.Request{
		ID: "req.c2", Applicant: "u.staff", Status: req.StatusCosignPending,
		CCs: []chain.CcRecord{
			{Cc: "u.other", Reason: "L4", Mode: chain.CcCosign, NotifiedAt: time.Now()},
		},
	}
	w := do(h.handleCosign, http.MethodPost, "/api/requests/cosign", "u.stranger",
		map[string]any{"id": "req.c2", "approve": true})
	if w.Code == http.StatusOK {
		t.Fatal("★ 非抄送人竟能会签")
	}
}

// ★ 待批队列里不得出现「别人该批、但当前人只是抄送」的单。
func TestHTTP_MyPending_ScopedToCaller(t *testing.T) {
	h, svc := newTestServer(t)
	svc.reqs["req.mine"] = &req.Request{
		ID: "req.mine", Applicant: "u.staff", Status: req.StatusApproving,
		Approvals: []req.ApprovalStep{{Approver: "u.lead", Tier: "T1", Action: "PENDING"}},
	}
	svc.reqs["req.other"] = &req.Request{
		ID: "req.other", Applicant: "u.other", Status: req.StatusApproving,
		Approvals: []req.ApprovalStep{{Approver: "u.other", Tier: "T2", Action: "PENDING"}},
	}
	// 会签待办：只有 u.lead 在名单里的那张
	svc.reqs["req.cs"] = &req.Request{
		ID: "req.cs", Applicant: "u.staff", Status: req.StatusCosignPending,
		CCs: []chain.CcRecord{{Cc: "u.lead", Reason: "L4", Mode: chain.CcCosign, NotifiedAt: time.Now()}},
	}

	w := do(h.handleMyPending, http.MethodGet, "/api/requests/pending", "u.lead", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	var got struct {
		ToApprove []string `json:"toApprove"`
		ToCosign  []string `json:"toCosign"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	for _, id := range got.ToApprove {
		if id == "req.other" {
			t.Fatal("★ 待批队列混入了别人的单")
		}
	}
	foundMine, foundCs := false, false
	for _, id := range got.ToApprove {
		if id == "req.mine" {
			foundMine = true
		}
	}
	for _, id := range got.ToCosign {
		if id == "req.cs" {
			foundCs = true
		}
	}
	if !foundMine {
		t.Fatal("自己的待批单未出现")
	}
	if !foundCs {
		t.Fatal("自己的会签待办未出现")
	}
}

// ───────────────────────────── 路由表：可审计 ─────────────────────────────

// ★ 把暴露的端点**逐个列出来断言**。
//
// 目的不是「测路由能不能通」，而是让「新增了一个写接口」这件事
// 在 code review / CI 里**必须被显式确认** —— 权限系统里多一个写口子
// 就是多一个攻击面，不能悄悄加。
func TestHTTP_Routes_AreExplicitlyListed(t *testing.T) {
	h, _ := newTestServer(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	// 每个端点都应当被注册（用「方法不匹配应得 405 而非 404」来判定已注册）
	paths := []struct{ method, path string }{
		{"GET", "/api/groups/get"},
		{"GET", "/api/groups/preview"},
		{"PUT", "/api/groups/grants"},
		{"PUT", "/api/groups/members"},
		{"DELETE", "/api/groups/members"},
		{"POST", "/api/requests"},
		{"GET", "/api/requests"},
		{"GET", "/api/requests/pending"},
		{"POST", "/api/requests/approve"},
		{"POST", "/api/requests/reject"},
		{"POST", "/api/requests/cosign"},
		{"POST", "/api/requests/withdraw"},
	}
	for _, p := range paths {
		// 用 **一律未注册的 PATCH** 探活：已注册 ⇒ 405；未注册 ⇒ 404。
		//
		// ★ 早前这里用「GET/POST 互换」当探针，结果在 /api/requests 上
		//   假警报：该路径同时注册了 GET 与 POST，互换后的方法照样命中真
		//   handler，于是拿到 401 而非 405。探针必须是**任何路由都没用过**
		//   的方法，否则测的是「有没有身份」，不是「路径注册没注册」。
		r := httptest.NewRequest(http.MethodPatch, p.path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code == http.StatusNotFound {
			t.Fatalf("★ 路由 %s %s 未注册", p.method, p.path)
		}
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("路由 %s %s 探活得到 %d（期望 405）", p.method, p.path, w.Code)
		}
	}
}
