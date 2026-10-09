package api

// delegation_test.go —— M-AUTH 代授 HTTP 层的**链路级**断言。
//
// ★ 本文件的重点不是「200 还是 409」，而是三条接口层纪律**真的**成立：
//   ① 身份只认请求头（body 里的 granter 必须是登录人本人）；
//   ② 被授人必须存在（否则静默无效）；
//   ③ 落库失败不得谎报成功。
//
// ★ 每条用例都从 `Routes` 注册的真实路由进（而非直接调 handler），
//   这样「路由是否真的注册」也被一并覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
)

// ───────────────────────────── 夹具 ─────────────────────────────

func delegTestNow() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }

func delegResolver() *authz.Resolver {
	r := authz.NewResolver(map[string]*authz.Entitlement{})
	r.Now = delegTestNow
	return r
}

// delegEnts 授出者持 m.ops/m.roi + L3 + 品牌 KONVY + grp.ops 组；
// 被授人存在但权限很低（用于「不溢出」的成功路径）。
func delegEnts(account string) (*authz.Entitlement, []*authz.Entitlement) {
	switch account {
	case "t1.lead":
		return &authz.Entitlement{
			Account:    "t1.lead",
			MaxLevel:   authz.L3,
			Modules:    authz.ModuleGrant{Enabled: []string{"m.ops", "m.roi"}},
			Dimensions: []authz.DimensionGrant{{Dim: "brand", Values: []string{"KONVY"}}},
			DataUseGroups: []authz.GroupScopeGrant{{
				Group: string(authz.GrpOps), Fields: []string{"gp", "aov"},
			}},
		}, nil
	case "t3.member":
		return &authz.Entitlement{Account: "t3.member", MaxLevel: authz.L1}, nil
	}
	return nil, nil
}

func newDelegHandler(sink DelegationSink) *DelegationHandlers {
	return &DelegationHandlers{
		Resolver:     delegResolver(),
		Entitlements: delegEnts,
		Sink:         sink,
		Now:          delegTestNow,
	}
}

func delegRequest(t *testing.T, h *DelegationHandlers, actor string, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/authz/delegate", strings.NewReader(body))
	if actor != "" {
		req.Header.Set("X-Spark-Account", actor)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ───────────────────────────── 纪律 ①：身份只认请求头 ─────────────────────────────

func TestDelegation_MissingIdentityIs401(t *testing.T) {
	rec := delegRequest(t, newDelegHandler(nil), "", `{"granter":"t1.lead","grantee":"t3.member","delegateAll":true}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无身份应 401，实际 %d", rec.Code)
	}
}

// ★ 最关键的一条：body 里的 granter 与登录人不一致必须被拒 ——
// 否则任何登录用户都能以别人的名义代授，而 upperBoundRef 正是审计依据。
func TestDelegation_GranterMustEqualActor(t *testing.T) {
	rec := delegRequest(t, newDelegHandler(nil), "t3.member",
		`{"granter":"t1.lead","grantee":"t3.member","delegateAll":true}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("granter≠actor 应 403，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

// ───────────────────────────── 纪律 ②：被授人必须存在 ─────────────────────────────

func TestDelegation_UnknownGranteeIs404(t *testing.T) {
	rec := delegRequest(t, newDelegHandler(nil), "t1.lead",
		`{"granter":"t1.lead","grantee":"ghost","delegateAll":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("幽灵被授人应 404，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

// ───────────────────────────── 拒绝码映射 ─────────────────────────────

func TestDelegation_SameAccountIs400WithCode(t *testing.T) {
	rec := delegRequest(t, newDelegHandler(nil), "t1.lead",
		`{"granter":"t1.lead","grantee":"t1.lead","delegateAll":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("自授自应 400，实际 %d", rec.Code)
	}
	var got struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应非 JSON：%v（%s）", err, rec.Body.String())
	}
	if got.Code != authz.ErrCodeSameAccount {
		t.Fatalf("拒绝码应为 %s，实际 %q", authz.ErrCodeSameAccount, got.Code)
	}
}

func TestDelegation_OverflowIs409WithCode(t *testing.T) {
	// 授出者只有 L3 + m.ops/m.roi；声明 L4 + m.secret 属溢出。
	rec := delegRequest(t, newDelegHandler(nil), "t1.lead",
		`{"granter":"t1.lead","grantee":"t3.member","scope":{"modules":["m.secret"],"maxLevel":"L4"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("溢出应 409，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var got struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应非 JSON：%v", err)
	}
	if got.Code != authz.ErrCodeDelegationOverflow {
		t.Fatalf("拒绝码应为 %s，实际 %q", authz.ErrCodeDelegationOverflow, got.Code)
	}
}

// ───────────────────────────── 成功路径 + 纪律 ③ ─────────────────────────────

func TestDelegation_SuccessWithoutSinkReportsNotPersisted(t *testing.T) {
	rec := delegRequest(t, newDelegHandler(nil), "t1.lead",
		`{"granter":"t1.lead","grantee":"t3.member","delegateAll":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var got struct {
		Delegation struct {
			Granter string            `json:"granter"`
			Grantee string            `json:"grantee"`
			Record  authz.GrantRecord `json:"record"`
		} `json:"delegation"`
		Persisted bool   `json:"persisted"`
		Audited   bool   `json:"audited"`
		Note      string `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应非 JSON：%v（%s）", err, rec.Body.String())
	}
	if got.Delegation.Record.Origin != authz.OriginSupervisor {
		t.Fatalf("留痕来源应为 %s，实际 %q", authz.OriginSupervisor, got.Delegation.Record.Origin)
	}
	if got.Delegation.Record.UpperBoundRef != "t1.lead" {
		t.Fatalf("上界应记授出者 t1.lead，实际 %q", got.Delegation.Record.UpperBoundRef)
	}
	// ★ 无 Sink ⇒ 权限未生效，必须如实回报，不得假装成功。
	if got.Persisted {
		t.Fatalf("未装配 Sink 时 persisted 必须为 false")
	}
	if got.Audited {
		t.Fatalf("未装配 Sink 时 audited 必须为 false")
	}
	if strings.TrimSpace(got.Note) == "" {
		t.Fatalf("未生效时必须给出说明（note 不得为空）")
	}
}

type recordingSink struct {
	saved []*authz.Delegation
	actor string
	err   error
}

func (s *recordingSink) SaveDelegation(_ context.Context, d *authz.Delegation, actor string) error {
	if s.err != nil {
		return s.err
	}
	s.saved = append(s.saved, d)
	s.actor = actor
	return nil
}

func TestDelegation_SinkPersists(t *testing.T) {
	sink := &recordingSink{}
	rec := delegRequest(t, newDelegHandler(sink), "t1.lead",
		`{"granter":"t1.lead","grantee":"t3.member","delegateAll":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if len(sink.saved) != 1 {
		t.Fatalf("应落库 1 条，实际 %d", len(sink.saved))
	}
	if sink.actor != "t1.lead" {
		t.Fatalf("落库 actor 应为登录人，实际 %q", sink.actor)
	}
	var got struct {
		Persisted bool `json:"persisted"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Persisted {
		t.Fatalf("落库成功时 persisted 应为 true")
	}
}

// ★ 纪律 ③：落库失败 ⇒ 明确 500，绝不回报「已成功」。
func TestDelegation_SinkFailureIsNotReportedAsSuccess(t *testing.T) {
	sink := &recordingSink{err: errors.New("db down")}
	rec := delegRequest(t, newDelegHandler(sink), "t1.lead",
		`{"granter":"t1.lead","grantee":"t3.member","delegateAll":true}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("落库失败应 500，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "未生效") {
		t.Fatalf("落库失败必须说明「未生效」，实际 %q", rec.Body.String())
	}
}

func TestDelegation_BadAtIs400(t *testing.T) {
	rec := delegRequest(t, newDelegHandler(nil), "t1.lead",
		`{"granter":"t1.lead","grantee":"t3.member","delegateAll":true,"at":"2026/10/09"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 at 应 400，实际 %d", rec.Code)
	}
}
