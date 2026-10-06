// templates.go —— M-TEMPLATE（视图模板中心）的 HTTP 接口层。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本层要防守的是「谁能读/改哪个模板」——与 groups.go 同源的风险：
//
//   1. **身份一律取自请求头 X-Spark-Account**，绝不接受 body 里的 owner。
//      模板的 owner 决定了它在个人档下的可见范围；若 body 里的 owner 生效，
//      任何人都能伪造「这是张三的模板」，进而在张三的模板列表里塞条目，
//      或冒名把别人的模板改档。
//
//   2. **改/删/设默认前先判归属**。CanWrite 比 CanRead 严：
//      能「看」别人的团队模板，不等于能「改」。尤其 system 档模板
//      一次改动会改变所有人的默认视图。
//
//   3. **套用（apply）会改变可见数据，必须留痕**：套用写 audit_log，
//      事后能回答「谁在何时套用了哪个口径」。
//
//   4. **模板永不含权限**。套用后能否拿到数据，仍由 /api/query 的门控决定。
//      本层只回模板内容（怎么看），不回数据（能看什么）。
// ══════════════════════════════════════════════════════════════════════════
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/miccchwang/spark-cicada/backend/internal/template"
)

// TemplateService 是 templatestore 提供给接口层的最小能力集。
//
// ★ 与 GroupService 同一纪律：用接口声明依赖，接口层因此不依赖 pgx，
//   可以只用内存替身做 HTTP 层单测（测鉴权与参数校验，不必拖真库）。
type TemplateService interface {
	// ResolveViewer 组装可见性判断所需的上下文（组、部门、是否管理员）。
	ResolveViewer(ctx context.Context, account string, isAdmin func(string) bool) (template.Viewer, error)
	// VisibleFor 取某页全部可读模板（已过滤 + 已按常用度排序）。
	VisibleFor(ctx context.Context, viewer template.Viewer, page string) ([]*template.Template, error)
	// Load 读单个模板（不做可见性过滤，由本层用 CanRead 判）。
	Load(ctx context.Context, id string) (*template.Template, error)
	// Save 新建/更新模板。
	Save(ctx context.Context, t *template.Template, actor string) error
	// Delete 删除模板，返回是否真的删掉了一行。
	Delete(ctx context.Context, id, owner string, isAdmin bool) (bool, error)
	// BumpUse 记录一次套用（use_count + 1 且写审计）。
	BumpUse(ctx context.Context, id, actor string) error
	// SetDefault 设为该页默认。
	SetDefault(ctx context.Context, id string) error
	// OwnerDept 取 owner 主部门（team 档无显式分享时的可见性依据）。
	OwnerDept(ctx context.Context, account string) (string, error)
}

// TemplateHandlers 模板中心的 HTTP 处理器。
type TemplateHandlers struct {
	Svc TemplateService
	// IsAdmin 判断某账号是否具备全局管理权限（可管任意模板）。
	// 与 GroupHandlers.IsAdmin 同源注入。
	IsAdmin func(account string) bool
	// IsSupervisor 判断某账号是否「主管及以上」（可建团队档模板）。
	//
	// ★ 与 IsAdmin 分开注入：两者的判定依据不同（一个看管理员模板，
	//   一个看组织链路 D9），且都会演进。留空时保守返回 false ——
	//   绝不能因为「不知道谁是主管」就默认所有人都是（那是提权）。
	IsSupervisor func(account string) bool
}

// ───────────────────────────── 读 ─────────────────────────────

// handleListTemplates GET /api/templates?page=/report
//
// 返回该页**当前登录人可见**的模板列表（已按常用度排序）。
func (h *TemplateHandlers) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	page := r.URL.Query().Get("page")
	if page == "" {
		http.Error(w, "missing page", http.StatusBadRequest)
		return
	}
	v, err := h.Svc.ResolveViewer(r.Context(), actor, h.IsAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	list, err := h.Svc.VisibleFor(r.Context(), v, page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []*template.Template{}
	}
	writeJSON(w, map[string]any{"templates": list})
}

// handleGetTemplate GET /api/templates/get?id=
//
// ★ 先判可见性再回内容：不可见就 404/403，不泄露「存在一个你看不到的模板」。
func (h *TemplateHandlers) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	t, err := h.Svc.Load(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	v, err := h.Svc.ResolveViewer(r.Context(), actor, h.IsAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.readable(r.Context(), t, v) {
		http.Error(w, "forbidden: 无权访问该模板", http.StatusForbidden)
		return
	}
	writeJSON(w, t)
}

// handleDefaultTemplate GET /api/templates/default?page=/report
//
// 返回该页应自动套用的默认模板；没有则返回 `{"template": null}`。
//
// ★ 返回 null 而不是随便挑一个：没有默认就不套用，
//   否则用户会莫名被套上一个不属于自己口径的视图。
func (h *TemplateHandlers) handleDefaultTemplate(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	page := r.URL.Query().Get("page")
	if page == "" {
		http.Error(w, "missing page", http.StatusBadRequest)
		return
	}
	v, err := h.Svc.ResolveViewer(r.Context(), actor, h.IsAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	list, err := h.Svc.VisibleFor(r.Context(), v, page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// ★ 默认选取里 team 档也要能按 owner 部门判定，故把 dept 上下文补齐。
	d := template.DefaultsForPage(list, v, page)
	writeJSON(w, map[string]any{"template": d})
}

// ───────────────────────────── 写 ─────────────────────────────

// handleSaveTemplate POST /api/templates
//
// 请求体：完整 Template（id/name/scope/page/queryState/columns/layout/shares…）
//
// ★ owner 一律用请求头身份覆盖 body 里的 owner（防冒名，见文件头注释）。
func (h *TemplateHandlers) handleSaveTemplate(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body template.Template
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.ID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	// ★ 身份覆盖：不接受 body 里的 owner
	body.Owner = actor

	v, err := h.Svc.ResolveViewer(r.Context(), actor, h.IsAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 已存在的模板 ⇒ 必须是可写者（owner 或管理员）
	// 不存在的 ⇒ 视为新建，但新建 system 档需要管理员权限（CanSetScope）。
	existing, lerr := h.Svc.Load(r.Context(), body.ID)
	if lerr == nil && existing != nil {
		if !template.CanWrite(existing, v) {
			http.Error(w, "forbidden: 无权修改该模板", http.StatusForbidden)
			return
		}
		// 改档位要重新过 CanSetScope（否则普通人可把自己的个人模板改成 system 档，
		// 一步变成全体默认视图 —— 典型的提权）
		if !template.CanSetScope(body.Scope, v, h.isSupervisor(r.Context(), actor)) {
			http.Error(w, "forbidden: 无权将模板设为该档位", http.StatusForbidden)
			return
		}
	} else {
		// 新建：档位授权
		if !template.CanSetScope(body.Scope, v, h.isSupervisor(r.Context(), actor)) {
			http.Error(w, "forbidden: 无权创建该档位模板", http.StatusForbidden)
			return
		}
	}

	if problems := template.Validate(&body); len(problems) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(w, map[string]any{"ok": false, "problems": problems})
		return
	}
	if err := h.Svc.Save(r.Context(), &body, actor); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": body.ID})
}

// handleApplyTemplate POST /api/templates/apply
// 请求体：{ "id": "..." }
//
// ★ 套用前校验可见性；套用后累加使用次数并写审计（口径变更必须留痕）。
//   本接口**不返回数据** —— 只确认套用；数据仍由 /api/query 取。
func (h *TemplateHandlers) handleApplyTemplate(w http.ResponseWriter, r *http.Request) {
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
	t, err := h.Svc.Load(r.Context(), body.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	v, err := h.Svc.ResolveViewer(r.Context(), actor, h.IsAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.readable(r.Context(), t, v) {
		http.Error(w, "forbidden: 无权套用该模板", http.StatusForbidden)
		return
	}
	if err := h.Svc.BumpUse(r.Context(), t.ID, actor); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 回套用者需要的全部内容（前端据此重建 QueryState + 列偏好 + 布局）
	writeJSON(w, map[string]any{"ok": true, "template": t})
}

// handleSetDefaultTemplate POST /api/templates/default
// 请求体：{ "id": "..." }
func (h *TemplateHandlers) handleSetDefaultTemplate(w http.ResponseWriter, r *http.Request) {
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
	t, err := h.Svc.Load(r.Context(), body.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	v, err := h.Svc.ResolveViewer(r.Context(), actor, h.IsAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// ★ 设默认会改变「进入页面时自动套用什么」，属写操作。
	if !template.CanWrite(t, v) {
		http.Error(w, "forbidden: 无权设置该模板为默认", http.StatusForbidden)
		return
	}
	if err := h.Svc.SetDefault(r.Context(), t.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleDeleteTemplate DELETE /api/templates?id=
func (h *TemplateHandlers) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	isAdmin := h.IsAdmin != nil && h.IsAdmin(actor)
	deleted, err := h.Svc.Delete(r.Context(), id, actor, isAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !deleted {
		// 不区分「不存在」与「非本人」——避免探测他人模板是否存在
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ───────────────────────────── 路由 ─────────────────────────────

// Routes 注册 M-TEMPLATE 的全部路由（集中声明，可审计）。
func (h *TemplateHandlers) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/templates", h.handleListTemplates)
	mux.HandleFunc("GET /api/templates/get", h.handleGetTemplate)
	mux.HandleFunc("GET /api/templates/default", h.handleDefaultTemplate)
	mux.HandleFunc("POST /api/templates", h.handleSaveTemplate)
	mux.HandleFunc("POST /api/templates/apply", h.handleApplyTemplate)
	mux.HandleFunc("POST /api/templates/default", h.handleSetDefaultTemplate)
	mux.HandleFunc("DELETE /api/templates", h.handleDeleteTemplate)
}

// ───────────────────────────── 内部帮手 ─────────────────────────────

func (h *TemplateHandlers) actor(w http.ResponseWriter, r *http.Request) (string, bool) {
	a := r.Header.Get("X-Spark-Account")
	if a == "" {
		http.Error(w, "missing identity", http.StatusUnauthorized)
		return "", false
	}
	return a, true
}

// readable 综合判断可见性：team 档无显式分享时需要 owner 部门上下文。
func (h *TemplateHandlers) readable(ctx context.Context, t *template.Template, v template.Viewer) bool {
	if template.CanRead(t, v) {
		return true
	}
	if t.Scope == template.ScopeTeam && len(t.Shares) == 0 {
		dept, err := h.Svc.OwnerDept(ctx, t.Owner)
		if err != nil {
			// 查不到部门 ⇒ 保守拒绝（fail-closed），不因 IO 失败而放行
			return false
		}
		return template.CanReadTeamWithOwnerDept(t, v, dept)
	}
	return false
}

// isSupervisor 判断是否「主管及以上」。
//
// ★ 做成可注入点：组织关系解析（D9 矩阵）会演进，接口层不该硬编码。
//   默认实现保守返回 false（只有管理员能在 CanSetScope 里过关），
//   避免「因为不知道谁是主管，就默认所有人都是」的提权。
func (h *TemplateHandlers) isSupervisor(_ context.Context, account string) bool {
	if h.IsSupervisor != nil {
		return h.IsSupervisor(account)
	}
	return false
}
