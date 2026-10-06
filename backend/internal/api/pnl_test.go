package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/pnl"
)

// ───────────────────────────── 内存替身 ─────────────────────────────

type fakeAuditor struct {
	changes []pnl.CaliberChange
	fail    error
}

func (f *fakeAuditor) RecordCaliberChange(c pnl.CaliberChange) error {
	if f.fail != nil {
		return f.fail
	}
	f.changes = append(f.changes, c)
	return nil
}

func newPnlMux(a CaliberAuditor) *http.ServeMux {
	mux := http.NewServeMux()
	h := &PnlHandlers{Auditor: a}
	h.Routes(mux)
	return mux
}

// ───────────────────────────── GET /api/pnl/calibers ─────────────────────────────

func TestHTTP_Calibers_ReturnsBothWithMeta(t *testing.T) {
	mux := newPnlMux(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/pnl/calibers?module=module.pnl", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200：%s", w.Code, w.Body.String())
	}
	var resp struct {
		Module           string `json:"module"`
		EffectiveCaliber string `json:"effectiveCaliber"`
		Source           string `json:"source"`
		Downgraded       bool   `json:"downgraded"`
		Calibers         []struct {
			ID                 string `json:"id"`
			Definition         string `json:"definition"`
			MisreadingRisk     string `json:"misreadingRisk"`
			SellerDiscountRole string `json:"sellerDiscountRole"`
			DiscountTargetLine string `json:"discountTargetLine"`
			IsDefaultForModule bool   `json:"isDefaultForModule"`
		} `json:"calibers"`
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}

	if len(resp.Calibers) != 2 {
		t.Fatalf("应返回 2 个口径，得 %d", len(resp.Calibers))
	}
	if resp.Calibers[0].ID != "A" || resp.Calibers[1].ID != "B" {
		t.Fatalf("口径顺序应为 A,B：%+v", resp.Calibers)
	}
	// ★ 每个口径都必须带「容易被误读成什么」——否则用户会误用
	for _, c := range resp.Calibers {
		if c.MisreadingRisk == "" {
			t.Fatalf("口径 %s 缺少误读风险提示（KODP 教训）", c.ID)
		}
		if c.SellerDiscountRole == "" || c.DiscountTargetLine == "" {
			t.Fatalf("口径 %s 缺少折扣归属信息：%+v", c.ID, c)
		}
	}
	// 折扣归属必须是两口径的唯一差异落点
	if resp.Calibers[0].DiscountTargetLine == resp.Calibers[1].DiscountTargetLine {
		t.Fatal("★ 两口径的折扣归属行必须不同")
	}
	// 模块默认（D10）
	if resp.EffectiveCaliber != "B" {
		t.Fatalf("P&L 默认口径应为 B，得 %s", resp.EffectiveCaliber)
	}
	if !resp.Calibers[1].IsDefaultForModule {
		t.Fatal("口径 B 应被标记为 P&L 的默认口径")
	}
	if !strings.Contains(resp.Decision, "D10") {
		t.Fatalf("响应应说明 D10 依据，得 %q", resp.Decision)
	}
}

func TestHTTP_Calibers_ReportModuleDefaultsA(t *testing.T) {
	mux := newPnlMux(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/pnl/calibers?module=module.report", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["effectiveCaliber"] != "A" {
		t.Fatalf("★ 经营报表默认口径应为 A（D10 分模块各自默认），得 %v", resp["effectiveCaliber"])
	}
}

func TestHTTP_Calibers_RequestedWins(t *testing.T) {
	mux := newPnlMux(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/pnl/calibers?module=module.pnl&caliber=A", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["effectiveCaliber"] != "A" {
		t.Fatalf("显式请求 A 应生效，得 %v", resp["effectiveCaliber"])
	}
	if resp["source"] != "requested" {
		t.Fatalf("来源应为 requested，得 %v", resp["source"])
	}
	if resp["downgraded"] != false {
		t.Fatalf("请求值合法不应标记降级，得 %v", resp["downgraded"])
	}
}

func TestHTTP_Calibers_DirtyValueFallsBackWithReason(t *testing.T) {
	mux := newPnlMux(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/pnl/calibers?module=module.pnl&caliber=ZZZ", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET 脏值应仍返回 200（用默认兜底），得 %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["effectiveCaliber"] != "B" {
		t.Fatalf("脏值应回退到模块默认 B，得 %v", resp["effectiveCaliber"])
	}
	if resp["downgraded"] != true {
		t.Fatal("★ 脏值必须标记 downgraded（前端要据此提示用户）")
	}
	if s, _ := resp["reason"].(string); !strings.Contains(s, "非法") {
		t.Fatalf("原因应说明「非法」，得 %q", s)
	}
}

func TestHTTP_Calibers_NoModuleDefaultsToPnL(t *testing.T) {
	mux := newPnlMux(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/pnl/calibers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["module"] != pnl.ModulePnL {
		t.Fatalf("未给 module 应默认 %s，得 %v", pnl.ModulePnL, resp["module"])
	}
}

// ───────────────────────────── POST /api/pnl/caliber ─────────────────────────────

func TestHTTP_SetCaliber_RequiresIdentity(t *testing.T) {
	mux := newPnlMux(&fakeAuditor{})
	req := httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{"to":"B"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺身份应 401，得 %d", w.Code)
	}
}

func TestHTTP_SetCaliber_InvalidTargetIs400NotSilentFallback(t *testing.T) {
	mux := newPnlMux(&fakeAuditor{})
	req := httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{"module":"module.pnl","to":"C"}`))
	req.Header.Set("X-Spark-Account", "u.fin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// ★ 主动切换必须报错 —— 静默回退会让财务拿着 A 的数字以为在看 B
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法目标口径应 400，得 %d：%s", w.Code, w.Body.String())
	}
}

func TestHTTP_SetCaliber_EmptyTargetIs400(t *testing.T) {
	mux := newPnlMux(&fakeAuditor{})
	req := httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{"module":"module.pnl","to":""}`))
	req.Header.Set("X-Spark-Account", "u.fin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空目标口径应 400（不提供「留空即切换」的捷径），得 %d", w.Code)
	}
}

func TestHTTP_SetCaliber_InvalidFromIs400(t *testing.T) {
	mux := newPnlMux(&fakeAuditor{})
	req := httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{"module":"module.pnl","to":"B","from":"X"}`))
	req.Header.Set("X-Spark-Account", "u.fin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 from 应 400（否则审计记录失真），得 %d", w.Code)
	}
}

func TestHTTP_SetCaliber_AuditsChange(t *testing.T) {
	aud := &fakeAuditor{}
	mux := newPnlMux(aud)
	req := httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{"module":"module.pnl","to":"A","from":"B"}`))
	req.Header.Set("X-Spark-Account", "u.fin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("合法切换应 200，得 %d：%s", w.Code, w.Body.String())
	}
	if len(aud.changes) != 1 {
		t.Fatalf("应落 1 条审计，得 %d", len(aud.changes))
	}
	c := aud.changes[0]
	if c.Account != "u.fin" || c.From != pnl.CaliberB || c.To != pnl.CaliberA || c.Module != pnl.ModulePnL {
		t.Fatalf("审计内容不对：%+v", c)
	}

	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["audited"] != true {
		t.Fatalf("应回显 audited=true，得 %v", resp["audited"])
	}
	if s, _ := resp["summary"].(string); !strings.Contains(s, "B → A") {
		t.Fatalf("摘要应含切换方向，得 %q", s)
	}
	// ★ 响应必须主动说明「净收入没变」——否则用户怀疑数据被篡改
	if s, _ := resp["priceNote"].(string); !strings.Contains(s, "净收入不受影响") {
		t.Fatalf("应提示净收入不受影响，得 %q", s)
	}
	if s, _ := resp["riskNotice"].(string); s == "" {
		t.Fatal("应回显该口径的误读风险")
	}
}

func TestHTTP_SetCaliber_FirstTimeFromEmptyIsAllowed(t *testing.T) {
	aud := &fakeAuditor{}
	mux := newPnlMux(aud)
	req := httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{"module":"module.pnl","to":"B"}`))
	req.Header.Set("X-Spark-Account", "u.fin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("首次设置（无 from）应 200，得 %d", w.Code)
	}
	if len(aud.changes) != 1 || aud.changes[0].From != "" {
		t.Fatalf("首次设置 From 应为空：%+v", aud.changes)
	}
}

func TestHTTP_SetCaliber_AuditFailureDoesNotBlockButIsDisclosed(t *testing.T) {
	// 审计失败不阻断（口径是展示偏好，不是安全边界），
	// 但**必须如实告知未留痕**，绝不能假装记上了。
	aud := &fakeAuditor{fail: errors.New("db down")}
	mux := newPnlMux(aud)
	req := httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{"module":"module.pnl","to":"A"}`))
	req.Header.Set("X-Spark-Account", "u.fin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("审计失败不应阻断切换，得 %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["audited"] != false {
		t.Fatalf("应如实回显 audited=false，得 %v", resp["audited"])
	}
	if s, _ := resp["warning"].(string); !strings.Contains(s, "db down") {
		t.Fatalf("应说明审计失败原因，得 %q", s)
	}
}

func TestHTTP_SetCaliber_DefaultModuleIsPnL(t *testing.T) {
	aud := &fakeAuditor{}
	mux := newPnlMux(aud)
	req := httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{"to":"B"}`))
	req.Header.Set("X-Spark-Account", "u.fin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("缺 module 应默认 P&L，得 %d：%s", w.Code, w.Body.String())
	}
	if aud.changes[0].Module != pnl.ModulePnL {
		t.Fatalf("module 应默认 %s，得 %s", pnl.ModulePnL, aud.changes[0].Module)
	}
}

func TestHTTP_SetCaliber_InvalidJSONIs400(t *testing.T) {
	mux := newPnlMux(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{not json`))
	req.Header.Set("X-Spark-Account", "u.fin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("非 JSON 应 400，得 %d", w.Code)
	}
}

// ───────────────────────────── 路由登记 ─────────────────────────────

func TestHTTP_PnlRoutes_AreExplicitlyListed(t *testing.T) {
	mux := newPnlMux(nil)

	req := httptest.NewRequest(http.MethodGet, "/api/pnl/calibers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusMethodNotAllowed {
		t.Fatal("GET /api/pnl/calibers 应已登记")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pnl/caliber", strings.NewReader(`{"to":"B"}`))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusMethodNotAllowed {
		t.Fatal("POST /api/pnl/caliber 应已登记")
	}

	// 未登记路径必须是 404（而不是被兜底成 200）
	req = httptest.NewRequest(http.MethodGet, "/api/pnl/nope", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("未登记路径应 404，得 %d", w.Code)
	}
}
