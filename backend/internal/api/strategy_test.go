// strategy_test.go —— M-STRATEGY HTTP 层单测。
//
// 本层测的是**边界纪律**（不拖真库）：
//  1. 身份：无 X-Spark-Account ⇒ 401；
//  2. 范围：D6 仅管理层，且**未注入 guard 时默认拒绝**（不得提权）；
//  3. 选型：卡外选项 ⇒ 422；未知动作 ⇒ 400；「填公式」类请求体不被接受；
//  4. 留痕：审计失败时如实回报 audited=false + warning，但决策仍成功；
//  5. 回滚：由服务端重新生成计划，不接受前端传 plan。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/strategy"
)

// ───────────────────────────── 替身 ─────────────────────────────

// fakeStrategy 内存版策略服务。
type fakeStrategy struct {
	choices   map[string]*strategy.Choice
	history   map[string]strategy.History
	decided   []strategy.HistoryEntry
	rolled    []strategy.RollbackPlan
	planErr   error
	loadErr   error
	decideErr error
	// staleMarked 记录被标记重算的桶（验证 keep_current 不触发重算）
	staleMarked []string
}

func newFakeStrategy() *fakeStrategy {
	c := &strategy.Choice{
		ID:         "ch.fee",
		Title:      "渠道费率口径",
		Context:    "Shopee 费率采集暂缓",
		CurrentKey: "a",
		Options: []strategy.Option{
			{Key: "a", Label: "A", Description: "d", ExpectedEffect: "不变", Risk: strategy.RiskLow, Reversible: true},
			{Key: "b", Label: "B", Description: "d", ExpectedEffect: "34.45% → 36.14%", Risk: strategy.RiskMedium, Reversible: true},
		},
		Impact: strategy.ImpactPreview{
			Metrics:         []string{"net_rate"},
			Delta:           map[string]map[string]string{"a": {"net_rate": "不变"}},
			AffectedBuckets: []string{"pnl_month"},
		},
	}
	return &fakeStrategy{
		choices: map[string]*strategy.Choice{c.ID: c},
		history: map[string]strategy.History{},
	}
}

func (f *fakeStrategy) LoadChoice(_ context.Context, id string, _ bool) (*strategy.Choice, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	c, ok := f.choices[id]
	if !ok {
		return nil, strategy.ErrInvalidChoice
	}
	return c, nil
}

func (f *fakeStrategy) ListPending(_ context.Context, _ bool) ([]*strategy.Choice, error) {
	out := make([]*strategy.Choice, 0, len(f.choices))
	for _, c := range f.choices {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeStrategy) History(_ context.Context, id string) (strategy.History, error) {
	return f.history[id], nil
}

func (f *fakeStrategy) Decide(_ context.Context, c *strategy.Choice, target string,
	kind strategy.ActionKind, actor, snap string) (strategy.HistoryEntry, error) {
	if f.decideErr != nil {
		return strategy.HistoryEntry{}, f.decideErr
	}
	e := strategy.HistoryEntry{
		ID: "h1", ChoiceID: c.ID,
		FromOption: c.CurrentKey, ToOption: target,
		DecidedBy: actor, DecidedAt: time.Unix(1, 0),
		SnapshotHash: snap, Kind: kind, Reversible: true,
	}
	if kind == strategy.ActionChoose && target != c.CurrentKey {
		e.AffectedBuckets = append([]string(nil), c.Impact.AffectedBuckets...)
		f.staleMarked = append(f.staleMarked, e.AffectedBuckets...)
		c.CurrentKey = target
	}
	f.history[c.ID] = f.history[c.ID].Append(e)
	f.decided = append(f.decided, e)
	return e, nil
}

func (f *fakeStrategy) PlanRollback(_ context.Context, id string, _ bool) (strategy.RollbackPlan, error) {
	if f.planErr != nil {
		return strategy.RollbackPlan{}, f.planErr
	}
	c := f.choices[id]
	if c == nil {
		return strategy.RollbackPlan{}, strategy.ErrInvalidChoice
	}
	return strategy.PlanRollback(c, f.history[id])
}

func (f *fakeStrategy) Rollback(_ context.Context, p strategy.RollbackPlan, actor string) (strategy.HistoryEntry, error) {
	c := f.choices[p.ChoiceID]
	e := strategy.HistoryEntry{
		ID: "h2", ChoiceID: p.ChoiceID,
		FromOption: p.FromOption, ToOption: p.ToOption,
		DecidedBy: actor, DecidedAt: time.Unix(2, 0),
		Kind: strategy.ActionChoose, Reversible: true,
		AffectedBuckets: append([]string(nil), p.AffectedBuckets...),
	}
	c.CurrentKey = p.ToOption
	f.history[p.ChoiceID] = f.history[p.ChoiceID].Append(e)
	f.rolled = append(f.rolled, p)
	f.staleMarked = append(f.staleMarked, p.AffectedBuckets...)
	return e, nil
}

// fakeStrategyAuditor 记录审计调用；可注入失败。
type fakeStrategyAuditor struct {
	calls  []string
	actors []string
	fail   bool
}

func (a *fakeStrategyAuditor) RecordStrategyChange(_ context.Context, actor, action, target string,
	_ map[string]any) error {
	a.calls = append(a.calls, action+"|"+target)
	a.actors = append(a.actors, actor)
	if a.fail {
		return errStrategyAuditDown
	}
	return nil
}

var errStrategyAuditDown = &fakeErr{"审计库不可用"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }

// ───────────────────────────── 帮手 ─────────────────────────────

func strategyRequest(t *testing.T, h http.HandlerFunc, method, target string,
	account string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败：%v", err)
		}
		r = httptest.NewRequest(method, target, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if account != "" {
		r.Header.Set("X-Spark-Account", account)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func allowAll(string) bool { return true }

// ───────────────────────────── 身份 ─────────────────────────────

func TestStrategy_无身份401(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	for _, tc := range []struct {
		name string
		fn   http.HandlerFunc
		tgt  string
	}{
		{"choices", h.handleListChoices, "/api/strategy/choices"},
		{"choice", h.handleGetChoice, "/api/strategy/choice?id=ch.fee"},
		{"history", h.handleHistory, "/api/strategy/history?id=ch.fee"},
		{"preference", h.handlePreference, "/api/strategy/preference"},
		{"plan", h.handlePlanRollback, "/api/strategy/rollback/plan?id=ch.fee"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := strategyRequest(t, tc.fn, "GET", tc.tgt, "", nil)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("期望 401，得 %d", w.Code)
			}
		})
	}
}

func TestStrategy_写接口无身份401(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "",
		decideReq{ChoiceID: "ch.fee", Kind: "keep_current"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，得 %d", w.Code)
	}
	w2 := strategyRequest(t, h.handleRollback, "POST", "/api/strategy/rollback", "",
		map[string]string{"choiceId": "ch.fee"})
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，得 %d", w2.Code)
	}
}

// ───────────────────────────── D6 范围守卫 ─────────────────────────────

func TestStrategy_未注入guard默认拒绝(t *testing.T) {
	// ★ 关键：nil guard 必须是「拒绝」，不能是「放行」
	h := &StrategyHandlers{Svc: newFakeStrategy()}
	w := strategyRequest(t, h.handleListChoices, "GET", "/api/strategy/choices", "ceo", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("未注入 ScopeGuard 时必须 403（不得提权），得 %d", w.Code)
	}
}

func TestStrategy_guard拒绝则403(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: func(string) bool { return false }}
	w := strategyRequest(t, h.handleListChoices, "GET", "/api/strategy/choices", "ops.sea", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("期望 403，得 %d", w.Code)
	}
}

func TestStrategy_guard放行则200(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	w := strategyRequest(t, h.handleListChoices, "GET", "/api/strategy/choices", "ceo", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d", w.Code)
	}
	var out struct {
		Choices []*strategy.Choice `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("期望 1 张卡，得 %d", len(out.Choices))
	}
}

func TestStrategy_写接口也受guard约束(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: func(string) bool { return false }}
	w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ops.sea",
		decideReq{ChoiceID: "ch.fee", Kind: "keep_current"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("写接口也必须过 D6 守卫，期望 403，得 %d", w.Code)
	}
}

// ───────────────────────────── 选型守卫 ─────────────────────────────

func TestStrategy_决策卡外选项422(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "choose", OptionKey: "zzz"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("卡外选项应 422，得 %d（body=%s）", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "候选集") {
		t.Fatalf("错误信息应说明候选集约束：%s", w.Body.String())
	}
}

func TestStrategy_未知动作400(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	// ★ 「填公式」是最该被拦下的：kind=set_value / write_formula 都不存在
	for _, kind := range []string{"set_value", "write_formula", "apply_formula", ""} {
		w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
			decideReq{ChoiceID: "ch.fee", Kind: kind, OptionKey: "b"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("kind=%q 应 400，得 %d", kind, w.Code)
		}
	}
}

func TestStrategy_请求体里的额外字段被忽略(t *testing.T) {
	// 即便客户端硬塞 formula/value，也不会被消费（结构里没有对应字段）
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	body := map[string]any{
		"choiceId": "ch.fee", "kind": "choose", "optionKey": "b",
		"formula": "net_revenue = gross_listing", "value": 999,
	}
	w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo", body)
	if w.Code != http.StatusOK {
		t.Fatalf("合法动作应成功（多余字段被忽略），得 %d", w.Code)
	}
}

func TestStrategy_缺choiceId400(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{Kind: "keep_current"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，得 %d", w.Code)
	}
}

func TestStrategy_非法JSON400(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	r := httptest.NewRequest("POST", "/api/strategy/decide", strings.NewReader("{not json"))
	r.Header.Set("X-Spark-Account", "ceo")
	w := httptest.NewRecorder()
	h.handleDecide(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，得 %d", w.Code)
	}
}

// ───────────────────────────── 决策语义 ─────────────────────────────

func TestStrategy_choose落历史并标记桶(t *testing.T) {
	f := newFakeStrategy()
	aud := &fakeStrategyAuditor{}
	h := &StrategyHandlers{Svc: f, Auditor: aud, ScopeGuard: allowAll,
		SnapshotHash: func(context.Context) string { return "sha256:snap" }}

	w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "choose", OptionKey: "b"})
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d（%s）", w.Code, w.Body.String())
	}
	if len(f.staleMarked) == 0 {
		t.Fatal("实质变更必须标记受影响桶 STALE")
	}
	if len(f.decided) != 1 || f.decided[0].FromOption != "a" || f.decided[0].ToOption != "b" {
		t.Fatalf("历史记录不对：%+v", f.decided)
	}
	if f.decided[0].SnapshotHash != "sha256:snap" {
		t.Fatalf("快照哈希未落库：%q", f.decided[0].SnapshotHash)
	}
	// ★ 接口层留痕用 Attempt 动作名（不能是 strategy.decide —— 那会污染策略历史）
	if len(aud.calls) != 1 || aud.calls[0] != strategy.AuditActionDecideAttempt+"|ch.fee" {
		t.Fatalf("审计未写：%v", aud.calls)
	}
	if aud.actors[0] != "ceo" {
		t.Fatalf("审计 actor 应取自身份头，得 %q", aud.actors[0])
	}
}

func TestStrategy_keepCurrent落历史但不标记桶(t *testing.T) {
	f := newFakeStrategy()
	h := &StrategyHandlers{Svc: f, ScopeGuard: allowAll}
	w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "keep_current"})
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d", w.Code)
	}
	// ★ 必须留痕（「决策过」是可审计的事实）
	if len(f.decided) != 1 {
		t.Fatal("keep_current 也必须落一条历史")
	}
	if f.decided[0].FromOption != f.decided[0].ToOption {
		t.Fatalf("keep_current 的 from/to 应相同：%+v", f.decided[0])
	}
	// ★ 但绝不能触发重算
	if len(f.staleMarked) != 0 {
		t.Fatalf("keep_current 不应标记任何桶为 STALE，实际：%v", f.staleMarked)
	}
}

func TestStrategy_审计失败仍成功但回报未留痕(t *testing.T) {
	f := newFakeStrategy()
	aud := &fakeStrategyAuditor{fail: true}
	h := &StrategyHandlers{Svc: f, Auditor: aud, ScopeGuard: allowAll}
	w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "choose", OptionKey: "b"})
	if w.Code != http.StatusOK {
		t.Fatalf("审计失败不应阻断决策（否则用户会重复点击落两条），得 %d", w.Code)
	}
	var out struct {
		Audited bool   `json:"audited"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if out.Audited {
		t.Fatal("审计失败时必须如实回报 audited=false")
	}
	if out.Warning == "" {
		t.Fatal("审计失败时必须给出告警文案")
	}
}

// ★ 回归：接口层留痕必须用 *_attempt 动作名。
//
// 若接口层写 strategy.decide，就会与 store 事务写的历史同名，
// History() 会把这条不带 reversible 的影子记录也算进去，
// LastStable() 遇第一条不可逆即停 ⇒ 决策之后回滚永久不可达。
// 详见 strategy.AuditActionDecideAttempt 的注释。
func TestStrategy_接口层留痕用attempt动作名(t *testing.T) {
	aud := &fakeStrategyAuditor{}
	h := &StrategyHandlers{Svc: newFakeStrategy(), Auditor: aud, ScopeGuard: allowAll}

	strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "choose", OptionKey: "b"})

	if len(aud.calls) != 1 {
		t.Fatalf("期望 1 次留痕，得 %d", len(aud.calls))
	}
	if !strings.HasPrefix(aud.calls[0], strategy.AuditActionDecideAttempt) {
		t.Fatalf("接口层留痕必须用 %q，实际用了 %q —— 用 %q 会污染策略历史并锁死回滚",
			strategy.AuditActionDecideAttempt, aud.calls[0], strategy.AuditActionDecide)
	}

	// 回滚同理：先做一次决策，才有可回滚的版本
	aud2 := &fakeStrategyAuditor{}
	f2 := newFakeStrategy()
	h2 := &StrategyHandlers{Svc: f2, Auditor: aud2, ScopeGuard: allowAll}
	_ = strategyRequest(t, h2.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "choose", OptionKey: "b"})
	aud2.calls = nil // 清掉决策那次的留痕，只看回滚
	strategyRequest(t, h2.handleRollback, "POST", "/api/strategy/rollback", "ceo",
		map[string]string{"choiceId": "ch.fee"})
	if len(aud2.calls) != 1 {
		t.Fatalf("期望 1 次回滚留痕，得 %d", len(aud2.calls))
	}
	if !strings.HasPrefix(aud2.calls[0], strategy.AuditActionRollbackAttempt) {
		t.Fatalf("回滚留痕必须用 %q，实际用了 %q", strategy.AuditActionRollbackAttempt, aud2.calls[0])
	}
}

func TestStrategy_无auditor时回报未留痕(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	w := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "keep_current"})
	var out struct {
		Audited bool `json:"audited"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Audited {
		t.Fatal("未注入 auditor 时必须 audited=false")
	}
}

// ───────────────────────────── 回滚 ─────────────────────────────

func TestStrategy_回滚计划需先有变更(t *testing.T) {
	f := newFakeStrategy()
	h := &StrategyHandlers{Svc: f, ScopeGuard: allowAll}
	w := strategyRequest(t, h.handlePlanRollback, "GET",
		"/api/strategy/rollback/plan?id=ch.fee", "ceo", nil)
	// 无历史 ⇒ 无可回滚版本 ⇒ 409（状态冲突，而非服务端故障）
	if w.Code != http.StatusConflict {
		t.Fatalf("期望 409，得 %d（%s）", w.Code, w.Body.String())
	}
}

func TestStrategy_回滚全流程(t *testing.T) {
	f := newFakeStrategy()
	h := &StrategyHandlers{Svc: f, ScopeGuard: allowAll}
	// 先改 a → b
	w1 := strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "choose", OptionKey: "b"})
	if w1.Code != http.StatusOK {
		t.Fatalf("决策失败：%d", w1.Code)
	}
	// 计划：应从 b 回到 a
	w2 := strategyRequest(t, h.handlePlanRollback, "GET",
		"/api/strategy/rollback/plan?id=ch.fee", "ceo", nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("计划失败：%d（%s）", w2.Code, w2.Body.String())
	}
	var plan strategy.RollbackPlan
	if err := json.Unmarshal(w2.Body.Bytes(), &plan); err != nil {
		t.Fatalf("解析计划失败：%v", err)
	}
	if plan.FromOption != "b" || plan.ToOption != "a" {
		t.Fatalf("计划方向错误：%s→%s", plan.FromOption, plan.ToOption)
	}
	if len(plan.AffectedBuckets) == 0 {
		t.Fatal("回滚计划必须带 affectedBuckets")
	}
	// 执行
	w3 := strategyRequest(t, h.handleRollback, "POST", "/api/strategy/rollback", "ceo",
		map[string]string{"choiceId": "ch.fee"})
	if w3.Code != http.StatusOK {
		t.Fatalf("回滚失败：%d（%s）", w3.Code, w3.Body.String())
	}
	if f.choices["ch.fee"].CurrentKey != "a" {
		t.Fatalf("回滚后当前值应为 a，得 %q", f.choices["ch.fee"].CurrentKey)
	}
	// 回滚也必须标记桶重算
	if len(f.staleMarked) == 0 {
		t.Fatal("回滚必须标记桶重算（只改参数不重算 = 数字错得毫无征兆）")
	}
}

func TestStrategy_回滚不接受前端传plan(t *testing.T) {
	// ★ 前端塞 plan 字段应被忽略；服务端用重新生成的计划（守卫候选集）
	f := newFakeStrategy()
	h := &StrategyHandlers{Svc: f, ScopeGuard: allowAll}
	_ = strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "choose", OptionKey: "b"})
	evil := map[string]any{
		"choiceId": "ch.fee",
		"plan":     map[string]any{"toOption": "HACKED", "affectedBuckets": []string{}},
	}
	w := strategyRequest(t, h.handleRollback, "POST", "/api/strategy/rollback", "ceo", evil)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d", w.Code)
	}
	if f.rolled[0].ToOption != "a" {
		t.Fatalf("回滚目标被前端篡改：%q（必须由服务端重新生成计划）", f.rolled[0].ToOption)
	}
}

func TestStrategy_回滚缺choiceId400(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	w := strategyRequest(t, h.handleRollback, "POST", "/api/strategy/rollback", "ceo",
		map[string]string{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，得 %d", w.Code)
	}
}

// ───────────────────────────── 历史 / 偏好 ─────────────────────────────

func TestStrategy_历史返回lastStable(t *testing.T) {
	f := newFakeStrategy()
	h := &StrategyHandlers{Svc: f, ScopeGuard: allowAll}
	_ = strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "choose", OptionKey: "b"})
	w := strategyRequest(t, h.handleHistory, "GET", "/api/strategy/history?id=ch.fee", "ceo", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d", w.Code)
	}
	var out struct {
		History          strategy.History `json:"history"`
		LastStableOption *string          `json:"lastStableOption"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(out.History) != 1 {
		t.Fatalf("期望 1 条历史，得 %d", len(out.History))
	}
	if out.LastStableOption == nil || *out.LastStableOption != "a" {
		t.Fatalf("lastStableOption 应为 a，得 %v", out.LastStableOption)
	}
}

func TestStrategy_空历史lastStable为null(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	w := strategyRequest(t, h.handleHistory, "GET", "/api/strategy/history?id=ch.fee", "ceo", nil)
	if !strings.Contains(w.Body.String(), `"lastStableOption":null`) {
		t.Fatalf("无可回滚版本时应显式返回 null（不编造），得 %s", w.Body.String())
	}
}

func TestStrategy_偏好按账号(t *testing.T) {
	f := newFakeStrategy()
	h := &StrategyHandlers{Svc: f, ScopeGuard: allowAll}
	_ = strategyRequest(t, h.handleDecide, "POST", "/api/strategy/decide", "ceo",
		decideReq{ChoiceID: "ch.fee", Kind: "choose", OptionKey: "b"})
	w := strategyRequest(t, h.handlePreference, "GET", "/api/strategy/preference", "ceo", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d", w.Code)
	}
	var p strategy.Preference
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if p.Account != "ceo" || p.Samples != 1 {
		t.Fatalf("偏好统计不对：%+v", p)
	}
	// 单样本 ⇒ 不应给倾向性标签
	if !strings.Contains(p.Note, "暂不做倾向性推荐") {
		t.Fatalf("单样本不应给倾向标签：%q", p.Note)
	}
}

// ───────────────────────────── 读接口参数 ─────────────────────────────

func TestStrategy_读接口缺参400(t *testing.T) {
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	for _, tc := range []struct {
		name string
		fn   http.HandlerFunc
		tgt  string
	}{
		{"choice", h.handleGetChoice, "/api/strategy/choice"},
		{"history", h.handleHistory, "/api/strategy/history"},
		{"plan", h.handlePlanRollback, "/api/strategy/rollback/plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := strategyRequest(t, tc.fn, "GET", tc.tgt, "ceo", nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("期望 400，得 %d", w.Code)
			}
		})
	}
}

// ───────────────────────────── 路由表 ─────────────────────────────

func TestStrategy_Routes显式列出(t *testing.T) {
	// 路由集中声明，改动必须是有意识的
	h := &StrategyHandlers{Svc: newFakeStrategy(), ScopeGuard: allowAll}
	mux := http.NewServeMux()
	h.Routes(mux)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/strategy/choices"},
		{"GET", "/api/strategy/choice"},
		{"GET", "/api/strategy/history"},
		{"GET", "/api/strategy/preference"},
		{"GET", "/api/strategy/rollback/plan"},
		{"POST", "/api/strategy/decide"},
		{"POST", "/api/strategy/rollback"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		_, pattern := mux.Handler(r)
		if pattern == "" {
			t.Fatalf("%s %s 未注册", tc.method, tc.path)
		}
	}
}
