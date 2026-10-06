// groups.go —— M-GROUP（D12）+ M-REQ（D14）的 HTTP 接口层。
//
// 与 api.go 的分工：api.go 管「数据查询」的门控与查询接口；
// 本文件管「权限本身的管理」——组的增删改、成员的进出、申请的提交与审批。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本层最需要防守的不是「功能对不对」，而是「谁能调这个接口」。
//
//	这是整个权限体系里**权力最集中**的一组接口：改一个组的 grants，
//	就等于一次性给一批人扩权。因此三条纪律：
//
//	1. **身份一律取自请求头，绝不接受请求体里的 account。**
//	   若 body 里带的 `actor` 生效，任何登录用户都能伪装成管理员去改别人的权限
//	   —— 这是最经典的越权（IDOR）。所有写接口的 actor 都从
//	   `X-Spark-Account` 取，body 里即使带了也被忽略。
//
//	2. **写操作先验「他是这个组的 owner，或者是全局管理员」。**
//	   仅靠「已登录」是不够的。
//
//	3. **申请人只能撤回自己单；审批人必须是路由算出来的那个人。**
//	   前者由 req.Withdraw 内部保证（比对 Applicant），后者由
//	   req.Approve 内部按 Approvals 匹配 —— 接口层不重复实现，
//	   只负责把「当前登录人」如实传进去。
//
// ══════════════════════════════════════════════════════════════════════════
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/group"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

// GroupService 是 groupstore 提供给接口层的最小能力集。
//
// ★ 用接口（而非直接依赖 *groupstore.Store）声明依赖：
//
//	接口层因此不依赖 pgx，可以只用内存替身做 HTTP 层单测 ——
//	HTTP 层要测的是「鉴权与参数校验」，不必每次都拖一个真库起来。
type GroupService interface {
	LoadGroup(ctx context.Context, id string) (*group.Group, error)
	LoadGroupsFor(ctx context.Context, account string) ([]*group.Group, []group.Membership, error)
	LoadPersonal(ctx context.Context, account string) (*group.Personal, error)
	UpsertGroup(ctx context.Context, g *group.Group) error
	ReplaceGrants(ctx context.Context, groupID string, grants []group.Grant, actor, reason string) error
	SetMembership(ctx context.Context, m group.Membership) error
	RemoveMembership(ctx context.Context, groupID, account string) error
	SaveRequest(ctx context.Context, r *req.Request) error
	LoadRequest(ctx context.Context, id string) (*req.Request, error)
	ListPendingFor(ctx context.Context, approver string, limit int) ([]string, error)
	ListRequestsByStatus(ctx context.Context, status req.Status, limit int) ([]string, error)
	ListReclaimable(ctx context.Context, now time.Time, limit int) ([]string, error)
	ListCosignPending(ctx context.Context) (map[string][]string, error)
}

// GroupHandlers 组与申请的 HTTP 处理器。
type GroupHandlers struct {
	Svc GroupService
	// Req 申请流服务（纯逻辑，内含路由与会签判定）。
	Req *req.Service
	// IsAdmin 判断某账号是否具备全局管理权限（可管任意组）。
	//
	// ★ 做成注入的函数而非写死规则：全局管理员的判定依据会变
	//   （先按模板、后按组），而接口层不该跟着改。
	IsAdmin func(account string) bool
}

// ───────────────────────────── 组管理 ─────────────────────────────

// handleLoadGroup GET /api/groups/{id}
func (h *GroupHandlers) handleLoadGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing group id", http.StatusBadRequest)
		return
	}
	g, err := h.Svc.LoadGroup(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// ★ 读组定义本身不泄露业务数据（只有「谁能看什么」的元数据），
	//   但仍要求调用者是该组 owner 或全局管理员 —— 避免任意用户
	//   枚举出「公司有哪些组、每组给了什么权限」。
	if !h.canManage(actor, g) {
		http.Error(w, "forbidden: 非本组管理员", http.StatusForbidden)
		return
	}
	writeJSON(w, g)
}

// handlePreviewJoin GET /api/groups/preview?id=&account=
//
// 预览「某人加入该组后会看到什么」。这是管理界面上「加入前先看一眼」的实现。
func (h *GroupHandlers) handlePreviewJoin(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	gid := r.URL.Query().Get("id")
	target := r.URL.Query().Get("account")
	if gid == "" || target == "" {
		http.Error(w, "missing id or account", http.StatusBadRequest)
		return
	}
	g, err := h.Svc.LoadGroup(r.Context(), gid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !h.canManage(actor, g) {
		http.Error(w, "forbidden: 非本组管理员", http.StatusForbidden)
		return
	}
	p, err := h.Svc.LoadPersonal(r.Context(), target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 预览：把「已加入该组」的成员关系喂进去
	m := &group.Membership{GroupID: gid, Account: target, Role: "member", InheritsGrants: true}
	ev := group.Preview(g, p, []*group.Membership{m})
	writeJSON(w, ev)
}

// handleReplaceGrants PUT /api/groups/grants
//
// 请求体：{ "groupId": "...", "grants": [...], "reason": "..." }
//
// ★ actor 不从 body 取（防越权），body 里即使有 actor 字段也被忽略。
func (h *GroupHandlers) handleReplaceGrants(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		GroupID string        `json:"groupId"`
		Grants  []group.Grant `json:"grants"`
		Reason  string        `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.GroupID == "" {
		http.Error(w, "missing groupId", http.StatusBadRequest)
		return
	}
	g, err := h.Svc.LoadGroup(r.Context(), body.GroupID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !h.canManage(actor, g) {
		http.Error(w, "forbidden: 非本组管理员", http.StatusForbidden)
		return
	}

	// 校验授权定义（在写库前拦下「kind/key 不匹配」「非法密级」等）
	if problems := group.Validate(&group.Group{
		ID: g.ID, Name: g.Name, Restricted: g.Restricted, Grants: body.Grants,
	}); len(problems) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(w, map[string]any{"ok": false, "problems": problems})
		return
	}

	if err := h.Svc.ReplaceGrants(r.Context(), body.GroupID, body.Grants, actor, body.Reason); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleSetMembership PUT /api/groups/members
//
// 请求体：{ "groupId": "...", "account": "...", "role": "member|owner", "inheritsGrants": true }
func (h *GroupHandlers) handleSetMembership(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		GroupID        string `json:"groupId"`
		Account        string `json:"account"`
		Role           string `json:"role"`
		InheritsGrants bool   `json:"inheritsGrants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.GroupID == "" || body.Account == "" {
		http.Error(w, "missing groupId or account", http.StatusBadRequest)
		return
	}
	g, err := h.Svc.LoadGroup(r.Context(), body.GroupID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !h.canManage(actor, g) {
		http.Error(w, "forbidden: 非本组管理员", http.StatusForbidden)
		return
	}
	if err := h.Svc.SetMembership(r.Context(), group.Membership{
		GroupID: body.GroupID, Account: body.Account,
		Role: body.Role, InheritsGrants: body.InheritsGrants,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleRemoveMembership DELETE /api/groups/members?groupId=&account=
func (h *GroupHandlers) handleRemoveMembership(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	gid := r.URL.Query().Get("groupId")
	acct := r.URL.Query().Get("account")
	if gid == "" || acct == "" {
		http.Error(w, "missing groupId or account", http.StatusBadRequest)
		return
	}
	g, err := h.Svc.LoadGroup(r.Context(), gid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !h.canManage(actor, g) {
		http.Error(w, "forbidden: 非本组管理员", http.StatusForbidden)
		return
	}
	if err := h.Svc.RemoveMembership(r.Context(), gid, acct); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ───────────────────────────── 申请流 ─────────────────────────────

// handleSubmitRequest POST /api/requests
//
// 请求体：{ "id": "...", "draft": {...}, "purpose": "...", "requestedExpiry": "..." }
//
// ★ 申请人取自请求头，**不接受 body 里的 applicant** —— 否则任何登录用户
//
//	都能以他人名义提交申请（申请人决定了路由到谁的上级）。
func (h *GroupHandlers) handleSubmitRequest(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		ID              string     `json:"id"`
		Draft           req.Draft  `json:"draft"`
		Purpose         string     `json:"purpose"`
		RequestedExpiry *time.Time `json:"requestedExpiry"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.ID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if body.Purpose == "" {
		// 用途说明是必填的（契约里 purpose 非可选）。
		// 空用途会让审批人无法判断「为什么要这个权限」。
		http.Error(w, "purpose is required", http.StatusBadRequest)
		return
	}

	rq := &req.Request{
		ID: body.ID, Applicant: actor, // ← 身份取自请求头
		Draft:           body.Draft,
		Purpose:         body.Purpose,
		RequestedExpiry: body.RequestedExpiry,
		Status:          req.StatusDraft,
	}
	v, err := h.Req.Submit(rq, nil)
	if err != nil {
		// ★ 路由失败（链上无人有足够权限）必须原样报错，不得吞掉后放行。
		http.Error(w, "submit failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := h.Svc.SaveRequest(r.Context(), rq); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": v.OK, "status": rq.Status, "validation": v})
}

// handleApprove POST /api/requests/approve
// 请求体：{ "id": "...", "reason": "..." }
func (h *GroupHandlers) handleApprove(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	rq, ok := h.loadReq(w, r, body.ID)
	if !ok {
		return
	}
	// ★ 审批人身份取自请求头；req.Approve 内部按 Approvals 匹配，
	//   非路由到的审批人自然匹配不上（步骤不会被标记），但状态仍会被推进 ——
	//   因此这里**先显式校验**他是该单的审批人，避免「谁都点得动批准」。
	if !hasApproval(rq, actor) {
		http.Error(w, "forbidden: 非本单审批人", http.StatusForbidden)
		return
	}
	if err := h.Req.Approve(rq, actor, body.Reason); err != nil {
		// 会签未决等业务拒绝用 409；其余按 422。
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := h.Svc.SaveRequest(r.Context(), rq); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": rq.Status})
}

// handleReject POST /api/requests/reject
func (h *GroupHandlers) handleReject(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	rq, ok := h.loadReq(w, r, body.ID)
	if !ok {
		return
	}
	if !hasApproval(rq, actor) {
		http.Error(w, "forbidden: 非本单审批人", http.StatusForbidden)
		return
	}
	if err := h.Req.Reject(rq, actor, body.Reason); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := h.Svc.SaveRequest(r.Context(), rq); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": rq.Status})
}

// handleCosign POST /api/requests/cosign
// 请求体：{ "id": "...", "approve": true, "reason": "..." }
func (h *GroupHandlers) handleCosign(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	rq, ok := h.loadReq(w, r, body.ID)
	if !ok {
		return
	}
	// req.Cosign 内部会拒绝「不在抄送名单」与「notify 模式无决定权」两种情形，
	// 接口层不重复判断 —— 单点真相。
	if err := h.Req.Cosign(rq, actor, body.Approve, body.Reason); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := h.Svc.SaveRequest(r.Context(), rq); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": rq.Status})
}

// handleWithdraw POST /api/requests/withdraw
// 请求体：{ "id": "..." }
func (h *GroupHandlers) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	rq, ok := h.loadReq(w, r, body.ID)
	if !ok {
		return
	}
	// req.Withdraw 内部比对 r.Applicant == by，非本人会被拒。
	if err := h.Req.Withdraw(rq, actor); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := h.Svc.SaveRequest(r.Context(), rq); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": rq.Status})
}

// handleMyPending GET /api/requests/pending —— 当前登录人的待批队列。
func (h *GroupHandlers) handleMyPending(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	approvals, err := h.Svc.ListPendingFor(r.Context(), actor, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cosigns, err := h.Svc.ListCosignPending(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 只保留「当前登录人确实在名单里」的会签待办 —— ListCosignPending 返回全量，
	// 直接透出会让任何用户看到全公司的会签情况。
	mine := []string{}
	for rid, ccs := range cosigns {
		for _, cc := range ccs {
			if cc == actor {
				mine = append(mine, rid)
				break
			}
		}
	}
	writeJSON(w, map[string]any{"toApprove": approvals, "toCosign": mine})
}

// handleGetRequest GET /api/requests?id=
//
// ★ 可见性：只有申请人本人、审批人、会签人可读 ——
//
//	申请单里含 purpose 与 draft，属于内部信息，不应任意用户可枚举。
func (h *GroupHandlers) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	rq, ok := h.loadReq(w, r, r.URL.Query().Get("id"))
	if !ok {
		return
	}
	if rq.Applicant != actor && !hasApproval(rq, actor) && !hasCC(rq, actor) && !h.IsAdmin(actor) {
		http.Error(w, "forbidden: 无权查看该申请", http.StatusForbidden)
		return
	}
	writeJSON(w, rq)
}

// Routes 注册 M-GROUP / M-REQ 的全部路由。
//
// ★ 路由集中在一处声明（而不是散落在各 handler 里各自判断路径）：
//
//	这样「这个接口到底暴露了哪些端点」是一个**可审计的列表** ——
//	权限系统里，多暴露一个写接口就是多一个攻击面，必须一眼可查。
//
// 用 Go 1.22+ 的 method+path 模式（`POST /api/x`），比手写 switch 更清晰，
// 也让「方法不对」由框架直接返回 405，不必每个 handler 自己判。
func (h *GroupHandlers) Routes(mux *http.ServeMux) {
	// 组管理
	mux.HandleFunc("GET /api/groups/get", h.handleLoadGroup)
	mux.HandleFunc("GET /api/groups/preview", h.handlePreviewJoin)
	mux.HandleFunc("PUT /api/groups/grants", h.handleReplaceGrants)
	mux.HandleFunc("PUT /api/groups/members", h.handleSetMembership)
	mux.HandleFunc("DELETE /api/groups/members", h.handleRemoveMembership)

	// 申请流
	mux.HandleFunc("POST /api/requests", h.handleSubmitRequest)
	mux.HandleFunc("GET /api/requests", h.handleGetRequest)
	mux.HandleFunc("GET /api/requests/pending", h.handleMyPending)
	mux.HandleFunc("POST /api/requests/approve", h.handleApprove)
	mux.HandleFunc("POST /api/requests/reject", h.handleReject)
	mux.HandleFunc("POST /api/requests/cosign", h.handleCosign)
	mux.HandleFunc("POST /api/requests/withdraw", h.handleWithdraw)
}

// ───────────────────────────── 内部帮手 ─────────────────────────────

// actor 取出当前登录账号；缺失即 401。
func (h *GroupHandlers) actor(w http.ResponseWriter, r *http.Request) (string, bool) {
	a := r.Header.Get("X-Spark-Account")
	if a == "" {
		http.Error(w, "missing identity", http.StatusUnauthorized)
		return "", false
	}
	return a, true
}

// canManage 判断 actor 是否可管理该组：全局管理员，或该组 owner。
func (h *GroupHandlers) canManage(actor string, g *group.Group) bool {
	if h.IsAdmin != nil && h.IsAdmin(actor) {
		return true
	}
	for _, o := range g.Owners {
		if o == actor {
			return true
		}
	}
	return false
}

func (h *GroupHandlers) loadReq(w http.ResponseWriter, r *http.Request, id string) (*req.Request, bool) {
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return nil, false
	}
	rq, err := h.Svc.LoadRequest(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return nil, false
	}
	return rq, true
}

// hasApproval 判断某人是否出现在该单的审批步骤里。
func hasApproval(rq *req.Request, account string) bool {
	for _, a := range rq.Approvals {
		if a.Approver == account {
			return true
		}
	}
	return false
}

// hasCC 判断某人是否出现在该单的抄送名单里。
func hasCC(rq *req.Request, account string) bool {
	for _, c := range rq.CCs {
		if c.Cc == account {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
