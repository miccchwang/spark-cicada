// templates_tenant_test.go —— 模板接口的**租户路由**守卫（Task #57）。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本文件只测一件事：handler 是否**经租户解析**取服务实例。
//
//   不测业务可见性（那在 templatestore 集成测试里）。
//   这两者必须分开测，因为它们的失效模式完全不同：
//     · 可见性逻辑错 ⇒ 某个用户看到不该看的模板（有症状）；
//     · 租户路由错   ⇒ **所有**租户共用未绑定实例，RLS 静默不过滤
//                      （在单租户测试里完全无症状）。
//   后者是"看起来一切正常"的那类，必须单独用替身把它逼出来。
// ══════════════════════════════════════════════════════════════════════════
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/template"
)

// tenantProbeSvc 是一个记录「自己被哪个租户调用过」的替身。
//
// ★ 它把"收到了租户"记成事实，供断言；不依赖任何 DB。
type tenantProbeSvc struct {
	label string
	calls *[]string
	// viewer 返回这个替身服务看到的 viewer（用于断言请求确实走到了它）。
	err error
}

func (s *tenantProbeSvc) note(method string) {
	*s.calls = append(*s.calls, s.label+":"+method)
}

func (s *tenantProbeSvc) ResolveViewer(_ context.Context, account string, _ func(string) bool) (template.Viewer, error) {
	s.note("ResolveViewer")
	if s.err != nil {
		return template.Viewer{}, s.err
	}
	return template.Viewer{Account: account, IsSystemAdmin: true}, nil
}

func (s *tenantProbeSvc) VisibleFor(_ context.Context, _ template.Viewer, _ string) ([]*template.Template, error) {
	s.note("VisibleFor")
	return []*template.Template{}, nil
}

func (s *tenantProbeSvc) Load(_ context.Context, id string) (*template.Template, error) {
	s.note("Load")
	return &template.Template{
		ID: id, Name: "t", Scope: template.ScopePersonal,
		Owner: "u1", Page: "/report",
	}, nil
}

func (s *tenantProbeSvc) Save(_ context.Context, _ *template.Template, _ string) error {
	s.note("Save")
	return nil
}

func (s *tenantProbeSvc) Delete(_ context.Context, _, _ string, _ bool) (bool, error) {
	s.note("Delete")
	return true, nil
}

func (s *tenantProbeSvc) BumpUse(_ context.Context, _, _ string) error {
	s.note("BumpUse")
	return nil
}

func (s *tenantProbeSvc) SetDefault(_ context.Context, _ string) error {
	s.note("SetDefault")
	return nil
}

func (s *tenantProbeSvc) OwnerDept(_ context.Context, _ string) (string, error) {
	s.note("OwnerDept")
	return "", nil
}

// doTenant 与 do 类似，但额外设置租户头。
func doTenant(h http.HandlerFunc, method, target, account, tenantID string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if account != "" {
		r.Header.Set("X-Spark-Account", account)
	}
	if tenantID != "" {
		r.Header.Set(tenantHeader, tenantID)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// TestHTTP_Templates_必须经租户解析取实例 —— 替身必须收到租户标识。
//
// ★ 若 handler 直接用 h.Svc（未绑定租户），本测试的 calls 里会缺少
//   "tenantA:VisibleFor"，而是出现 "base:VisibleFor" ⇒ 红。
func TestHTTP_Templates_必须经租户解析取实例(t *testing.T) {
	var calls []string
	base := &tenantProbeSvc{label: "base", calls: &calls}
	tenantA := &tenantProbeSvc{label: "tenantA", calls: &calls}

	h := &TemplateHandlers{
		Svc:          base,
		IsAdmin:      func(string) bool { return true },
		IsSupervisor: func(string) bool { return true },
		ForTenant: func(tid string) TemplateService {
			if tid == "aaaaaaaa-0000-4000-8000-000000000001" {
				return tenantA
			}
			return nil
		},
	}

	w := doTenant(h.handleListTemplates, http.MethodGet,
		"/api/templates?page=/report", "u1",
		"aaaaaaaa-0000-4000-8000-000000000001")
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d：%s", w.Code, w.Body.String())
	}
	for _, c := range calls {
		if len(c) >= 4 && c[:4] == "base" {
			t.Fatalf("handler 用了未绑定租户的实例（calls=%v）—— 这正是 RLS 静默不过滤的入口", calls)
		}
	}
	if !containsCall(calls, "tenantA:VisibleFor") {
		t.Fatalf("租户实例未被调用（calls=%v）", calls)
	}
}

// TestHTTP_Templates_多租户配置下缺租户头必须拒绝 —— fail-closed，不得回退基实例。
func TestHTTP_Templates_多租户配置下缺租户头必须拒绝(t *testing.T) {
	var calls []string
	base := &tenantProbeSvc{label: "base", calls: &calls}
	h := &TemplateHandlers{
		Svc:          base,
		IsAdmin:      func(string) bool { return true },
		IsSupervisor: func(string) bool { return true },
		ForTenant:    func(string) TemplateService { return &tenantProbeSvc{label: "x", calls: &calls} },
	}

	w := doTenant(h.handleListTemplates, http.MethodGet, "/api/templates?page=/report", "u1", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺少租户头时应当拒绝（400），得 %d", w.Code)
	}
	if len(calls) != 0 {
		t.Fatalf("拒绝后不应调用任何服务（calls=%v）", calls)
	}
}

// TestHTTP_Templates_租户不可用必须503 —— 未知租户不得回退基实例。
func TestHTTP_Templates_租户不可用必须503(t *testing.T) {
	var calls []string
	base := &tenantProbeSvc{label: "base", calls: &calls}
	h := &TemplateHandlers{
		Svc:          base,
		IsAdmin:      func(string) bool { return true },
		IsSupervisor: func(string) bool { return true },
		ForTenant:    func(string) TemplateService { return nil }, // 解析不出 ⇒ nil
	}

	w := doTenant(h.handleListTemplates, http.MethodGet,
		"/api/templates?page=/report", "u1",
		"bbbbbbbb-0000-4000-8000-000000000009")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("租户不可用应当 503，得 %d", w.Code)
	}
	if len(calls) != 0 {
		t.Fatalf("拒绝后不应调用任何服务（calls=%v）", calls)
	}
}

// TestHTTP_Templates_单租户部署忽略租户头 —— ForTenant==nil 时沿用基实例。
//
// ★ 这条是「不破坏单租户部署」的回归守卫：单租户下没有租户头也应正常工作。
func TestHTTP_Templates_单租户部署忽略租户头(t *testing.T) {
	var calls []string
	base := &tenantProbeSvc{label: "base", calls: &calls}
	h := &TemplateHandlers{
		Svc:          base,
		IsAdmin:      func(string) bool { return true },
		IsSupervisor: func(string) bool { return true },
		// ForTenant 留 nil ⇒ 单租户
	}
	w := doTenant(h.handleListTemplates, http.MethodGet, "/api/templates?page=/report", "u1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("单租户部署无租户头应当正常（200），得 %d", w.Code)
	}
	if !containsCall(calls, "base:VisibleFor") {
		t.Fatalf("单租户应走基实例（calls=%v）", calls)
	}
}

// TestHTTP_Templates_写路径也必须带租户 —— Save 走的是同一套解析。
//
// ★ 只测读路径是不够的：写路径漏租户会让数据写进错误的租户（比读到别人的更糟，
//   因为它是**持久性**的污染）。
func TestHTTP_Templates_写路径也必须带租户(t *testing.T) {
	var calls []string
	base := &tenantProbeSvc{label: "base", calls: &calls}
	tenantA := &tenantProbeSvc{label: "tenantA", calls: &calls}
	h := &TemplateHandlers{
		Svc:          base,
		IsAdmin:      func(string) bool { return true },
		IsSupervisor: func(string) bool { return true },
		ForTenant:    func(string) TemplateService { return tenantA },
	}

	body := map[string]any{
		"id": "tpl.x", "name": "x", "scope": "personal",
		"page": "/report", "queryState": map[string]any{},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("编码请求体: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/templates", bytes.NewReader(buf))
	r.Header.Set("X-Spark-Account", "u1")
	r.Header.Set(tenantHeader, "aaaaaaaa-0000-4000-8000-000000000001")
	w := httptest.NewRecorder()
	h.handleSaveTemplate(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d：%s", w.Code, w.Body.String())
	}
	if containsCall(calls, "base:Save") {
		t.Fatalf("写路径走了未绑定租户的实例（calls=%v）", calls)
	}
	if !containsCall(calls, "tenantA:Save") {
		t.Fatalf("写路径未经租户实例（calls=%v）", calls)
	}
}

// TestTenantIDFromRequest_优先context再头 —— 中间件解析结果优先于可伪造的头。
func TestTenantIDFromRequest_优先context再头(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(tenantHeader, "header-value")
	if got := TenantIDFromRequest(r); got != "header-value" {
		t.Fatalf("无 context 时应取头，得 %q", got)
	}
	r2 := r.WithContext(WithTenantID(r.Context(), "ctx-value"))
	if got := TenantIDFromRequest(r2); got != "ctx-value" {
		t.Fatalf("有 context 时应以 context 为准，得 %q", got)
	}
	// 空 context 值不应覆盖头（空串视为"中间件没设"）
	r3 := r.WithContext(WithTenantID(r.Context(), ""))
	if got := TenantIDFromRequest(r3); got != "header-value" {
		t.Fatalf("context 为空时应回退头，得 %q", got)
	}
}

// TestTenantIDFromRequest_nil请求不panic。
func TestTenantIDFromRequest_nil请求不panic(t *testing.T) {
	if got := TenantIDFromRequest(nil); got != "" {
		t.Fatalf("nil 请求应返回空串，得 %q", got)
	}
}

// ───────────────────────────── 帮手 ─────────────────────────────

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
