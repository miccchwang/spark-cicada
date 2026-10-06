package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/template"
)

// ───────────────────────────── 内存替身 ─────────────────────────────

type fakeTplSvc struct {
	byID map[string]*template.Template
	// depts owner → dept
	depts map[string]string
	// viewers account → viewer（预置；未命中则只有个人档语义）
	viewers map[string]template.Viewer
	admins  map[string]bool
	// 记录副作用，便于断言
	saved   []string
	deleted []string
	applied []string
	defs    []string
}

func newFakeTplSvc() *fakeTplSvc {
	return &fakeTplSvc{
		byID:    map[string]*template.Template{},
		depts:   map[string]string{},
		viewers: map[string]template.Viewer{},
		admins:  map[string]bool{},
	}
}

func (f *fakeTplSvc) ResolveViewer(_ context.Context, account string, isAdmin func(string) bool) (template.Viewer, error) {
	v, ok := f.viewers[account]
	if !ok {
		v = template.Viewer{Account: account, Groups: []string{}}
	}
	v.Account = account
	if isAdmin != nil && isAdmin(account) {
		v.IsSystemAdmin = true
	}
	if f.admins[account] {
		v.IsSystemAdmin = true
	}
	return v, nil
}

func (f *fakeTplSvc) VisibleFor(_ context.Context, v template.Viewer, page string) ([]*template.Template, error) {
	out := []*template.Template{}
	for _, t := range f.byID {
		if t.Page != page {
			continue
		}
		ok := template.CanRead(t, v)
		if !ok && t.Scope == template.ScopeTeam && len(t.Shares) == 0 {
			ok = template.CanReadTeamWithOwnerDept(t, v, f.depts[t.Owner])
		}
		if ok {
			out = append(out, t)
		}
	}
	return template.SortByUseCountDesc(out), nil
}

func (f *fakeTplSvc) Load(_ context.Context, id string) (*template.Template, error) {
	t, ok := f.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", template.ErrNotFound, id)
	}
	cp := *t
	return &cp, nil
}

func (f *fakeTplSvc) Save(_ context.Context, t *template.Template, actor string) error {
	cp := *t
	f.byID[t.ID] = &cp
	f.saved = append(f.saved, t.ID)
	return nil
}

func (f *fakeTplSvc) Delete(_ context.Context, id, owner string, isAdmin bool) (bool, error) {
	t, ok := f.byID[id]
	if !ok {
		return false, nil
	}
	if !isAdmin && t.Owner != owner {
		return false, nil
	}
	delete(f.byID, id)
	f.deleted = append(f.deleted, id)
	return true, nil
}

func (f *fakeTplSvc) BumpUse(_ context.Context, id, _ string) error {
	if t, ok := f.byID[id]; ok {
		t.UseCount++
	}
	f.applied = append(f.applied, id)
	return nil
}

func (f *fakeTplSvc) SetDefault(_ context.Context, id string) error {
	if _, ok := f.byID[id]; !ok {
		return fmt.Errorf("%w: %s", template.ErrNotFound, id)
	}
	f.defs = append(f.defs, id)
	return nil
}

func (f *fakeTplSvc) OwnerDept(_ context.Context, account string) (string, error) {
	return f.depts[account], nil
}

// ───────────────────────────── 测试脚手架 ─────────────────────────────

func newTplServer(t *testing.T, svc *fakeTplSvc) (*httptest.Server, *TemplateHandlers) {
	t.Helper()
	h := &TemplateHandlers{
		Svc:     svc,
		IsAdmin: func(a string) bool { return svc.admins[a] },
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, h
}

func tplReq(t *testing.T, method, url, account string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("编码请求体：%v", err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatalf("构造请求：%v", err)
	}
	if account != "" {
		req.Header.Set("X-Spark-Account", account)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发请求：%v", err)
	}
	return res
}

func decodeJSON(t *testing.T, res *http.Response, v any) {
	t.Helper()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("解码响应：%v", err)
	}
}

// ───────────────────────────── 身份 ─────────────────────────────

func TestHTTP_Templates_MissingIdentity(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)

	// 所有端点缺身份都应 401，而不是「当作匿名放行」
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/templates?page=/report"},
		{"GET", "/api/templates/get?id=tpl_a"},
		{"GET", "/api/templates/default?page=/report"},
		{"POST", "/api/templates"},
		{"POST", "/api/templates/apply"},
		{"POST", "/api/templates/default"},
		{"DELETE", "/api/templates?id=tpl_a"},
	} {
		res := tplReq(t, tc.method, srv.URL+tc.path, "", nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("★ %s %s 缺身份应 401，得到 %d", tc.method, tc.path, res.StatusCode)
		}
		res.Body.Close()
	}
}

// ★ 最关键的越权防线：body 里的 owner 必须被忽略。
func TestHTTP_SaveTemplate_IgnoresBodyOwner(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)

	// 攻击者 alice 试图以 bob 的名义创建模板
	body := map[string]any{
		"id": "tpl_attack", "name": "冒名模板", "scope": "personal",
		"owner": "u.bob", // ← 应被忽略
		"page":  "/report", "queryState": map[string]any{"v": "1.0"},
		"columns": []any{}, "layout": map[string]any{"expanded": map[string]bool{}},
	}
	res := tplReq(t, "POST", srv.URL+"/api/templates", "u.alice", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("本人建个人档应成功，得到 %d", res.StatusCode)
	}
	res.Body.Close()

	got := svc.byID["tpl_attack"]
	if got == nil {
		t.Fatal("模板未保存")
	}
	if got.Owner != "u.alice" {
		t.Fatalf("★ owner 应取自请求头，得到 %q（body 里的 u.bob 被错误采纳 = 冒名漏洞）", got.Owner)
	}
}

// ───────────────────────────── 档位授权 ─────────────────────────────

func TestHTTP_SaveTemplate_SystemScopeRequiresAdmin(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)

	body := map[string]any{
		"id": "tpl_sys_attack", "name": "伪系统模板", "scope": "system",
		"page": "/report", "queryState": map[string]any{"v": "1.0"},
		"columns": []any{}, "layout": map[string]any{"expanded": map[string]bool{}},
	}
	// 普通用户不得创建 system 档 —— 否则一步变成全体默认视图（提权）
	res := tplReq(t, "POST", srv.URL+"/api/templates", "u.alice", body)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("★ 普通用户建 system 档应 403，得到 %d", res.StatusCode)
	}
	res.Body.Close()

	// 管理员可以
	svc.admins["u.it"] = true
	res2 := tplReq(t, "POST", srv.URL+"/api/templates", "u.it", body)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("管理员建 system 档应成功，得到 %d", res2.StatusCode)
	}
	res2.Body.Close()
}

func TestHTTP_SaveTemplate_TeamScopeRequiresSupervisor(t *testing.T) {
	svc := newFakeTplSvc()
	h := &TemplateHandlers{
		Svc:          svc,
		IsAdmin:      func(string) bool { return false },
		IsSupervisor: func(a string) bool { return a == "u.mgr" },
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := map[string]any{
		"id": "tpl_team", "name": "团队口径", "scope": "team",
		"page": "/report", "queryState": map[string]any{"v": "1.0"},
		"columns": []any{}, "layout": map[string]any{"expanded": map[string]bool{}},
	}
	// 普通用户不得建团队档
	res := tplReq(t, "POST", srv.URL+"/api/templates", "u.staff", body)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("★ 普通用户建 team 档应 403，得到 %d", res.StatusCode)
	}
	res.Body.Close()

	// 主管可以
	res2 := tplReq(t, "POST", srv.URL+"/api/templates", "u.mgr", body)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("主管建 team 档应成功，得到 %d", res2.StatusCode)
	}
	res2.Body.Close()
}

// ★ 改档位必须重新过授权：不能把已存在的个人模板「升级」成 system 档。
func TestHTTP_SaveTemplate_CannotPromoteScope(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)

	// alice 先建一个个人档（合法）
	res := tplReq(t, "POST", srv.URL+"/api/templates", "u.alice", map[string]any{
		"id": "tpl_p", "name": "我的", "scope": "personal",
		"page": "/report", "queryState": map[string]any{"v": "1.0"},
		"columns": []any{}, "layout": map[string]any{"expanded": map[string]bool{}},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("前提：建个人档应成功，得到 %d", res.StatusCode)
	}
	res.Body.Close()

	// 再试图把它改成 system 档 ⇒ 应被拒
	res2 := tplReq(t, "POST", srv.URL+"/api/templates", "u.alice", map[string]any{
		"id": "tpl_p", "name": "我的", "scope": "system",
		"page": "/report", "queryState": map[string]any{"v": "1.0"},
		"columns": []any{}, "layout": map[string]any{"expanded": map[string]bool{}},
	})
	if res2.StatusCode != http.StatusForbidden {
		t.Fatalf("★ 非管理员把个人档提升为 system 档应 403，得到 %d", res2.StatusCode)
	}
	res2.Body.Close()
}

// ───────────────────────────── 可见性 ─────────────────────────────

func TestHTTP_GetTemplate_ForbiddenForOutsider(t *testing.T) {
	svc := newFakeTplSvc()
	// bob 的个人模板
	svc.byID["tpl_bob"] = &template.Template{
		ID: "tpl_bob", Name: "bob 的", Scope: template.ScopePersonal,
		Owner: "u.bob", Page: "/report", QueryState: []byte(`{"v":"1.0"}`),
	}
	srv, _ := newTplServer(t, svc)

	res := tplReq(t, "GET", srv.URL+"/api/templates/get?id=tpl_bob", "u.alice", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("★ 读他人个人模板应 403，得到 %d", res.StatusCode)
	}
	res.Body.Close()

	// 本人可读
	res2 := tplReq(t, "GET", srv.URL+"/api/templates/get?id=tpl_bob", "u.bob", nil)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("本人应可读，得到 %d", res2.StatusCode)
	}
	res2.Body.Close()
}

func TestHTTP_ListTemplates_ScopedToVisibility(t *testing.T) {
	svc := newFakeTplSvc()
	svc.byID["tpl_alice"] = &template.Template{
		ID: "tpl_alice", Name: "a", Scope: template.ScopePersonal,
		Owner: "u.alice", Page: "/report", QueryState: []byte(`{}`), Version: "1.0",
	}
	svc.byID["tpl_bob"] = &template.Template{
		ID: "tpl_bob", Name: "b", Scope: template.ScopePersonal,
		Owner: "u.bob", Page: "/report", QueryState: []byte(`{}`), Version: "1.0",
	}
	svc.byID["tpl_sys"] = &template.Template{
		ID: "tpl_sys", Name: "s", Scope: template.ScopeSystem,
		Owner: "u.it", Page: "/report", QueryState: []byte(`{}`), Version: "1.0",
	}
	svc.byID["tpl_pnl"] = &template.Template{
		ID: "tpl_pnl", Name: "p", Scope: template.ScopeSystem,
		Owner: "u.it", Page: "/pnl", QueryState: []byte(`{}`), Version: "1.0",
	}
	srv, _ := newTplServer(t, svc)

	res := tplReq(t, "GET", srv.URL+"/api/templates?page=/report", "u.alice", nil)
	var got struct {
		Templates []*template.Template `json:"templates"`
	}
	decodeJSON(t, res, &got)

	if len(got.Templates) != 2 {
		t.Fatalf("alice 在 /report 应看到 2 条（自己的 + system），得到 %d", len(got.Templates))
	}
	for _, x := range got.Templates {
		if x.ID == "tpl_bob" {
			t.Fatal("★ 不得出现他人个人模板")
		}
		if x.ID == "tpl_pnl" {
			t.Fatal("★ 模板按页隔离：不应出现 /pnl 页模板")
		}
	}
}

func TestHTTP_ListTemplates_PageRequired(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)
	res := tplReq(t, "GET", srv.URL+"/api/templates", "u.alice", nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 page 应 400，得到 %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestHTTP_ListTemplates_TeamNoShareUsesOwnerDept(t *testing.T) {
	svc := newFakeTplSvc()
	svc.depts["u.mgr"] = "D_SALES"
	svc.viewers["u.alice"] = template.Viewer{Account: "u.alice", Dept: "D_SALES", Groups: []string{}}
	svc.byID["tpl_team"] = &template.Template{
		ID: "tpl_team", Name: "团队", Scope: template.ScopeTeam,
		Owner: "u.mgr", Page: "/report", QueryState: []byte(`{}`), Version: "1.0",
	}
	srv, _ := newTplServer(t, svc)

	// 同部门可见
	res := tplReq(t, "GET", srv.URL+"/api/templates?page=/report", "u.alice", nil)
	var got struct {
		Templates []*template.Template `json:"templates"`
	}
	decodeJSON(t, res, &got)
	if len(got.Templates) != 1 {
		t.Fatalf("★ 同部门应可见 team 模板（无显式分享），得到 %d 条", len(got.Templates))
	}

	// 异部门不可见
	svc.viewers["u.bob"] = template.Viewer{Account: "u.bob", Dept: "D_OTHER", Groups: []string{}}
	res2 := tplReq(t, "GET", srv.URL+"/api/templates?page=/report", "u.bob", nil)
	var got2 struct {
		Templates []*template.Template `json:"templates"`
	}
	decodeJSON(t, res2, &got2)
	if len(got2.Templates) != 0 {
		t.Fatalf("★ 异部门不得见 team 模板，得到 %d 条", len(got2.Templates))
	}
}

// ───────────────────────────── 套用 ─────────────────────────────

func TestHTTP_ApplyTemplate_RejectsInvisible(t *testing.T) {
	svc := newFakeTplSvc()
	svc.byID["tpl_bob"] = &template.Template{
		ID: "tpl_bob", Name: "b", Scope: template.ScopePersonal,
		Owner: "u.bob", Page: "/report", QueryState: []byte(`{}`), Version: "1.0",
	}
	srv, _ := newTplServer(t, svc)

	res := tplReq(t, "POST", srv.URL+"/api/templates/apply", "u.alice", map[string]any{"id": "tpl_bob"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("★ 套用不可见模板应 403，得到 %d", res.StatusCode)
	}
	res.Body.Close()
	if len(svc.applied) != 0 {
		t.Fatal("★ 被拒的套用不得累加使用次数")
	}
}

func TestHTTP_ApplyTemplate_BumpsUseAndReturnsContent(t *testing.T) {
	svc := newFakeTplSvc()
	svc.byID["tpl_a"] = &template.Template{
		ID: "tpl_a", Name: "a", Scope: template.ScopePersonal,
		Owner: "u.alice", Page: "/report", QueryState: []byte(`{"v":"1.0"}`), Version: "1.0",
		Columns: []template.ColumnPref{{Key: "month", Visible: true, Order: 0}},
	}
	srv, _ := newTplServer(t, svc)

	res := tplReq(t, "POST", srv.URL+"/api/templates/apply", "u.alice", map[string]any{"id": "tpl_a"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("套用本人模板应成功，得到 %d", res.StatusCode)
	}
	var got struct {
		OK       bool               `json:"ok"`
		Template *template.Template `json:"template"`
	}
	decodeJSON(t, res, &got)
	if !got.OK || got.Template == nil {
		t.Fatal("应回 ok + template（前端据此重建 QueryState）")
	}
	if got.Template.ID != "tpl_a" {
		t.Fatalf("回错模板：%s", got.Template.ID)
	}
	if len(svc.applied) != 1 || svc.byID["tpl_a"].UseCount != 1 {
		t.Fatal("★ 套用应累加使用次数（常用排序依赖它）")
	}
}

func TestHTTP_ApplyTemplate_NotFound(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)
	res := tplReq(t, "POST", srv.URL+"/api/templates/apply", "u.alice", map[string]any{"id": "nope"})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在的模板应 404，得到 %d", res.StatusCode)
	}
	res.Body.Close()
}

// ───────────────────────────── 设默认 ─────────────────────────────

func TestHTTP_SetDefault_RequiresWrite(t *testing.T) {
	svc := newFakeTplSvc()
	svc.byID["tpl_sys"] = &template.Template{
		ID: "tpl_sys", Name: "s", Scope: template.ScopeSystem,
		Owner: "u.it", Page: "/report", QueryState: []byte(`{}`), Version: "1.0",
	}
	srv, _ := newTplServer(t, svc)

	// ★ system 档对全体可读，但设默认是写操作 ⇒ 普通用户应被拒
	res := tplReq(t, "POST", srv.URL+"/api/templates/default", "u.alice", map[string]any{"id": "tpl_sys"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("★ 非 owner 设 system 档为默认应 403（可读≠可写），得到 %d", res.StatusCode)
	}
	res.Body.Close()

	// owner 自己可以
	res2 := tplReq(t, "POST", srv.URL+"/api/templates/default", "u.it", map[string]any{"id": "tpl_sys"})
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("owner 设默认应成功，得到 %d", res2.StatusCode)
	}
	res2.Body.Close()
	if len(svc.defs) != 1 {
		t.Fatal("应记录一次设默认")
	}
}

func TestHTTP_DefaultTemplate_NullWhenNone(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)
	res := tplReq(t, "GET", srv.URL+"/api/templates/default?page=/report", "u.alice", nil)
	var got struct {
		Template *template.Template `json:"template"`
	}
	decodeJSON(t, res, &got)
	if got.Template != nil {
		t.Fatalf("★ 无默认时应回 null（不得随便挑一个），得到 %v", got.Template)
	}
}

func TestHTTP_DefaultTemplate_PicksVisibleDefault(t *testing.T) {
	svc := newFakeTplSvc()
	svc.byID["tpl_mine"] = &template.Template{
		ID: "tpl_mine", Name: "mine", Scope: template.ScopePersonal,
		Owner: "u.alice", Page: "/report", QueryState: []byte(`{}`), Version: "1.0", IsDefault: true,
	}
	srv, _ := newTplServer(t, svc)
	res := tplReq(t, "GET", srv.URL+"/api/templates/default?page=/report", "u.alice", nil)
	var got struct {
		Template *template.Template `json:"template"`
	}
	decodeJSON(t, res, &got)
	if got.Template == nil || got.Template.ID != "tpl_mine" {
		t.Fatalf("应取到个人默认，得到 %v", got.Template)
	}
}

// ───────────────────────────── 删除 ─────────────────────────────

func TestHTTP_DeleteTemplate_OnlyOwnerOrAdmin(t *testing.T) {
	svc := newFakeTplSvc()
	svc.byID["tpl_bob"] = &template.Template{
		ID: "tpl_bob", Name: "b", Scope: template.ScopePersonal,
		Owner: "u.bob", Page: "/report", QueryState: []byte(`{}`), Version: "1.0",
	}
	srv, _ := newTplServer(t, svc)

	// 非 owner 删除 ⇒ 404（不区分「不存在」与「非本人」，避免探测）
	res := tplReq(t, "DELETE", srv.URL+"/api/templates?id=tpl_bob", "u.alice", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("非 owner 删应 404，得到 %d", res.StatusCode)
	}
	res.Body.Close()
	if _, still := svc.byID["tpl_bob"]; !still {
		t.Fatal("★ 他人模板不得被删")
	}

	// owner 删除成功
	res2 := tplReq(t, "DELETE", srv.URL+"/api/templates?id=tpl_bob", "u.bob", nil)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("owner 删应成功，得到 %d", res2.StatusCode)
	}
	res2.Body.Close()
	if _, still := svc.byID["tpl_bob"]; still {
		t.Fatal("owner 删除应生效")
	}
}

func TestHTTP_DeleteTemplate_AdminCanDeleteAny(t *testing.T) {
	svc := newFakeTplSvc()
	svc.admins["u.it"] = true
	svc.byID["tpl_bob"] = &template.Template{
		ID: "tpl_bob", Name: "b", Scope: template.ScopePersonal,
		Owner: "u.bob", Page: "/report", QueryState: []byte(`{}`), Version: "1.0",
	}
	srv, _ := newTplServer(t, svc)
	res := tplReq(t, "DELETE", srv.URL+"/api/templates?id=tpl_bob", "u.it", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("管理员应可删任意模板，得到 %d", res.StatusCode)
	}
	res.Body.Close()
}

// ───────────────────────────── 校验 ─────────────────────────────

func TestHTTP_SaveTemplate_EmptyNameRejected(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)
	res := tplReq(t, "POST", srv.URL+"/api/templates", "u.alice", map[string]any{
		"id": "tpl_x", "name": "  ", "scope": "personal",
		"page": "/report", "queryState": map[string]any{"v": "1.0"},
		"columns": []any{}, "layout": map[string]any{"expanded": map[string]bool{}},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("★ 空名应 422（自命名是需求能力），得到 %d", res.StatusCode)
	}
	var got struct {
		OK       bool     `json:"ok"`
		Problems []string `json:"problems"`
	}
	decodeJSON(t, res, &got)
	if got.OK || len(got.Problems) == 0 {
		t.Fatal("应回 ok=false + problems")
	}
}

func TestHTTP_SaveTemplate_MissingQueryStateRejected(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)
	res := tplReq(t, "POST", srv.URL+"/api/templates", "u.alice", map[string]any{
		"id": "tpl_x", "name": "无状态", "scope": "personal",
		"page": "/report", "columns": []any{}, "layout": map[string]any{"expanded": map[string]bool{}},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("★ 缺 queryState 应 422，得到 %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestHTTP_SaveTemplate_MissingIDRejected(t *testing.T) {
	svc := newFakeTplSvc()
	srv, _ := newTplServer(t, svc)
	res := tplReq(t, "POST", srv.URL+"/api/templates", "u.alice", map[string]any{
		"name": "x", "scope": "personal", "page": "/report",
		"queryState": map[string]any{"v": "1.0"},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 id 应 400，得到 %d", res.StatusCode)
	}
	res.Body.Close()
}

// ───────────────────────────── 路由表可审计 ─────────────────────────────

func TestHTTP_TemplateRoutes_AreExplicitlyListed(t *testing.T) {
	svc := newFakeTplSvc()
	h := &TemplateHandlers{Svc: svc, IsAdmin: func(string) bool { return false }}
	mux := http.NewServeMux()
	h.Routes(mux)

	// 用一律未注册的 PATCH 探活：已注册 ⇒ 405；未注册 ⇒ 404
	for _, p := range []string{
		"/api/templates",
		"/api/templates/get",
		"/api/templates/default",
		"/api/templates/apply",
	} {
		req := httptest.NewRequest(http.MethodPatch, p, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("★ 路由 %s 未注册", p)
		}
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("路由 %s 探活得到 %d（期望 405）", p, rec.Code)
		}
	}
}
