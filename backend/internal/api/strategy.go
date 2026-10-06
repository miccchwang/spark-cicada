// strategy.go —— M-STRATEGY（策略实验室）的 HTTP 接口层。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本层要防守的四件事：
//
//  1. **身份一律取自请求头 X-Spark-Account**，绝不接受 body 里的 decidedBy。
//     策略历史是「谁做的决策」的唯一记录；若 body 可自填 decidedBy，
//     任何人都能把一次激进变更记到别人名下 —— 事后追责直接失效。
//
//  2. **决策范围仅管理层（D6：T1–T2）**。策略实验室能改全公司的费率口径，
//     它不是一个「用户偏好」。因此本层用 ScopeGuard 显式把关，
//     且**默认拒绝**（未注入 guard 时一律 403），绝不「因为不知道谁能用
//     就默认所有人能用」——那是提权。
//
//  3. **只接受选型动作**，不接受任何自由输入。契约里 DecisionAction 只有
//     choose / keep_current 两种；服务端额外用 strategy.Resolve 校验
//     optionKey 必须落在本卡候选集内。想「填公式」的请求在这里被硬拒绝。
//
//  4. **变更必须留痕，且留痕失败要如实告知**。策略变更会改变全公司看到的
//     数字口径，审计不可用时应暴露 `audited:false`，而不是静默放过。
//     （与 M-PNL 的处理保持一致：不阻断，但绝不假装成功了。）
//
// ★ 与前三个模块的差别：M-PNL 的口径是「展示偏好」，可以降级；
//
//	但 M-STRATEGY 的变更**会写规则集、会让预计算桶过期**，是有副作用的。
//	因此本层在无库时**拒绝写操作**（503），只允许读元数据。
//
// ══════════════════════════════════════════════════════════════════════════
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/miccchwang/spark-cicada/backend/internal/strategy"
)

// StrategyService 是策略实验室的持久化能力集。
//
// ★ 用接口声明依赖（与 GroupService / TemplateService 同一纪律）：
//
//	接口层因此不依赖 pgx，可以用内存替身做 HTTP 层单测。
type StrategyService interface {
	// LoadChoice 读一张选型卡。
	LoadChoice(ctx context.Context, id string, reload bool) (*strategy.Choice, error)
	// ListPending 列出待决策的选型卡（blocking 优先）。
	ListPending(ctx context.Context, reload bool) ([]*strategy.Choice, error)
	// History 读某张卡的历史（已按时间正序）。
	History(ctx context.Context, choiceID string) (strategy.History, error)
	// Decide 落一次决策：写历史 + 更新当前值 + 标记受影响桶 STALE。
	//
	// 返回：落定的历史记录、需要重算的桶。
	Decide(ctx context.Context, c *strategy.Choice, target string, kind strategy.ActionKind,
		actor, snapshotHash string) (strategy.HistoryEntry, error)
	// PlanRollback 生成回滚计划（不执行）。
	PlanRollback(ctx context.Context, choiceID string, reload bool) (strategy.RollbackPlan, error)
	// Rollback 执行回滚（写历史 + 改回稳定值 + 标记桶 STALE）。
	Rollback(ctx context.Context, plan strategy.RollbackPlan, actor string) (strategy.HistoryEntry, error)
}

// StrategyAuditor 写策略审计（append-only）。
//
// ★ 与 CaliberAuditor 同样做成可注入依赖：审计存储不可用时，
//
//	本层仍能完成决策，但会如实回报「未留痕」。
type StrategyAuditor interface {
	RecordStrategyChange(ctx context.Context, actor, action, target string,
		detail map[string]any) error
}

// StrategyHandlers 策略实验室的 HTTP 处理器。
type StrategyHandlers struct {
	Svc StrategyService
	// Auditor 审计落库；nil 表示审计不可用（接口会回报 audited=false）。
	Auditor StrategyAuditor
	// ScopeGuard 判断某账号是否可用策略实验室。
	//
	// ★ D6 决策：仅管理层（T1–T2）。
	// ★ 默认（nil）**拒绝**：见文件头第 2 条。
	ScopeGuard func(account string) bool
	// SnapshotHash 取当前数据快照哈希（决策依据）。
	//
	// ★ 做成注入点而不是在 handler 里现算：快照哈希的来源会演进
	//   （从「最新桶版本」到「带时间窗的查询指纹」），接口层不该硬编码。
	//   返回空串也可接受 —— 前端会显示「未取到快照」，但不会失败。
	SnapshotHash func(ctx context.Context) string
}

// ───────────────────────────── 读 ─────────────────────────────

// handleListChoices GET /api/strategy/choices
//
// 返回待决策的选型卡列表（blocking 的排在前面）。
func (h *StrategyHandlers) handleListChoices(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(w, r); !ok {
		return
	}
	if !h.allowed(r, w) {
		return
	}
	// ?reload=1 强制丢弃缓存重算候选（参数调优可能耗时，默认走缓存）
	reload := r.URL.Query().Get("reload") == "1"
	list, err := h.Svc.ListPending(r.Context(), reload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []*strategy.Choice{}
	}
	writeJSON(w, map[string]any{"choices": list})
}

// handleGetChoice GET /api/strategy/choice?id=
func (h *StrategyHandlers) handleGetChoice(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(w, r); !ok {
		return
	}
	if !h.allowed(r, w) {
		return
	}
	id := r.URL.Query().Get("id")
	if strings.TrimSpace(id) == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	c, err := h.Svc.LoadChoice(r.Context(), id, r.URL.Query().Get("reload") == "1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, c)
}

// handleHistory GET /api/strategy/history?id=
//
// 返回某张卡的全部历史（时间正序）。
//
// ★ 同时回带 lastStableOption：前端要在 UI 上标出「可回滚到哪」，
//
//	若让前端自己从历史里推，两处推导逻辑必然漂移。
func (h *StrategyHandlers) handleHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(w, r); !ok {
		return
	}
	if !h.allowed(r, w) {
		return
	}
	id := r.URL.Query().Get("id")
	if strings.TrimSpace(id) == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	hs, err := h.Svc.History(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hs = strategy.SortHistoryAsc(hs)
	last, ok := hs.LastStable()
	writeJSON(w, map[string]any{
		"history": hs,
		// 无可回滚版本时显式返回 null，不编造一个值
		"lastStableOption": nullableString(last, ok),
	})
}

// handlePreference GET /api/strategy/preference
//
// 返回当前账号的风险偏好（采纳学习结果）。
func (h *StrategyHandlers) handlePreference(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	if !h.allowed(r, w) {
		return
	}
	// 偏好从「该账号的全部决策」聚合；按卡逐个读取其选项风险等级。
	choices, err := h.Svc.ListPending(r.Context(), false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	riskOf := h.riskResolver(r.Context(), choices)
	// 汇总全部卡的历史
	var all strategy.History
	for _, c := range choices {
		h2, err := h.Svc.History(r.Context(), c.ID)
		if err != nil {
			// 单卡历史读失败不应让整个偏好查询失败：跳过并继续。
			continue
		}
		all = append(all, h2...)
	}
	writeJSON(w, strategy.LearnPreference(actor, all, riskOf))
}

// ───────────────────────────── 写 ─────────────────────────────

// decideReq POST /api/strategy/decide 的请求体。
//
// ★ 结构上只有 kind / optionKey —— 没有 formula、没有 value、没有 params。
//
//	这不是「忘了加」，是刻意不给：见 strategy 包注释第一纪律。
type decideReq struct {
	ChoiceID string `json:"choiceId"`
	// Kind 只能是 choose | keep_current。
	Kind string `json:"kind"`
	// OptionKey 仅当 kind=choose 时必填，且必须落在本卡候选集内。
	OptionKey string `json:"optionKey,omitempty"`
}

// handleDecide POST /api/strategy/decide
//
// 请求体：{"choiceId":"...","kind":"choose","optionKey":"b"}
//
// 语义：
//
//	choose(x)      → 历史记 from=current, to=x；更新当前值；标记桶 STALE
//	keep_current   → 历史记 from=current, to=current；**不动**桶
//
// ★ 为什么 keep_current 也要落历史：见 strategy 包注释第二纪律。
// ★ 为什么 keep_current 不标记桶：见 strategy.Choice.IsChange 注释。
func (h *StrategyHandlers) handleDecide(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	if !h.allowed(r, w) {
		return
	}
	var body decideReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.ChoiceID) == "" {
		http.Error(w, "missing choiceId", http.StatusBadRequest)
		return
	}
	kind, err := strategy.ParseAction(body.Kind)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// ★ 决策人放进 ctx：审计帮手据此落 actor，避免每个内部函数都多一个参数。
	ctx := withActor(r.Context(), actor)
	c, err := h.Svc.LoadChoice(ctx, body.ChoiceID, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	action := strategy.Action{Kind: kind, OptionKey: body.OptionKey}
	// ★ 候选集守卫在这里：卡外选项一律硬拒绝（越权/脏数据）
	target, err := c.Resolve(action)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, strategy.ErrUnknownOption) {
			// 422 而非 400：卡本身合法，是请求指了不存在的选项
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), status)
		return
	}

	entry, err := h.Svc.Decide(ctx, c, target, kind, actor, h.snapshotHash(ctx))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// ★ 接口层留痕用 **Attempt** 动作名，不用 AuditActionDecide：
	//
	//	策略历史的唯一来源是 store 在业务事务里写的那条（同事务改 current_key
	//	+ 标记桶 STALE）。若这里也写 "strategy.decide"，历史里会多出一条
	//	不带 reversible 的影子记录，被 History() 读成「不可逆」，
	//	导致 LastStable() 立刻停下 —— 回滚永久不可达。详见 strategy 包常量注释。
	audited, warn := h.audit(ctx, strategy.AuditActionDecideAttempt, body.ChoiceID, map[string]any{
		"choiceId":        entry.ChoiceID,
		"from":            entry.FromOption,
		"to":              entry.ToOption,
		"kind":            string(entry.Kind),
		"changed":         entry.IsChange(),
		"affectedBuckets": entry.AffectedBuckets,
		"snapshotHash":    entry.SnapshotHash,
	})
	writeJSON(w, map[string]any{
		"ok":      true,
		"entry":   entry,
		"audited": audited,
		"warning": warn,
	})
}

// handlePlanRollback GET /api/strategy/rollback/plan?id=
//
// 只产出计划，不执行 —— 让前端先展示「将从 X 回到 Y、需重算 N 个桶」，
// 用户确认后再调 POST。
func (h *StrategyHandlers) handlePlanRollback(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(w, r); !ok {
		return
	}
	if !h.allowed(r, w) {
		return
	}
	id := r.URL.Query().Get("id")
	if strings.TrimSpace(id) == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	plan, err := h.Svc.PlanRollback(r.Context(), id, false)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, strategy.ErrNoStableVersion) {
			// 409：状态冲突（当前没有可回滚的版本），不是服务端故障
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, plan)
}

// handleRollback POST /api/strategy/rollback
//
// 请求体：{"choiceId":"..."}
//
// ★ 由服务端**重新**生成回滚计划，而不是接受前端传来的 plan：
//
//	前端传 plan 的话，用户可以在浏览器里把 ToOption 改成任意值 ——
//	那就等于绕过了候选集守卫，与「不许任意输入」的纪律冲突。
func (h *StrategyHandlers) handleRollback(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	if !h.allowed(r, w) {
		return
	}
	var body struct {
		ChoiceID string `json:"choiceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.ChoiceID) == "" {
		http.Error(w, "missing choiceId", http.StatusBadRequest)
		return
	}
	ctx := withActor(r.Context(), actor)
	plan, err := h.Svc.PlanRollback(ctx, body.ChoiceID, true)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, strategy.ErrNoStableVersion) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	entry, err := h.Svc.Rollback(ctx, plan, actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// ★ 同 handleDecide：接口层用 Attempt 动作名，策略历史仍由 store 的事务那条承担。
	audited, warn := h.audit(ctx, strategy.AuditActionRollbackAttempt, body.ChoiceID, map[string]any{
		"choiceId":        plan.ChoiceID,
		"from":            plan.FromOption,
		"to":              plan.ToOption,
		"undoOf":          plan.UndoOfEntryID,
		"affectedBuckets": entry.AffectedBuckets,
		"snapshotHash":    entry.SnapshotHash,
	})
	writeJSON(w, map[string]any{
		"ok":      true,
		"plan":    plan,
		"entry":   entry,
		"audited": audited,
		"warning": warn,
	})
}

// ───────────────────────────── 路由 ─────────────────────────────

// Routes 注册 M-STRATEGY 的全部路由（集中声明，可审计）。
func (h *StrategyHandlers) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/strategy/choices", h.handleListChoices)
	mux.HandleFunc("GET /api/strategy/choice", h.handleGetChoice)
	mux.HandleFunc("GET /api/strategy/history", h.handleHistory)
	mux.HandleFunc("GET /api/strategy/preference", h.handlePreference)
	mux.HandleFunc("GET /api/strategy/rollback/plan", h.handlePlanRollback)
	mux.HandleFunc("POST /api/strategy/decide", h.handleDecide)
	mux.HandleFunc("POST /api/strategy/rollback", h.handleRollback)
}

// ───────────────────────────── 内部帮手 ─────────────────────────────

func (h *StrategyHandlers) actor(w http.ResponseWriter, r *http.Request) (string, bool) {
	a := r.Header.Get("X-Spark-Account")
	if a == "" {
		http.Error(w, "missing identity", http.StatusUnauthorized)
		return "", false
	}
	return a, true
}

// allowed 判断该账号是否可用策略实验室（D6：仅管理层）。
//
// ★ nil guard ⇒ 拒绝。绝不能「未配置就放行」：策略变更会改全公司口径。
func (h *StrategyHandlers) allowed(r *http.Request, w http.ResponseWriter) bool {
	if h.ScopeGuard == nil {
		http.Error(w, "forbidden: 策略实验室仅限管理层（D6）", http.StatusForbidden)
		return false
	}
	acct := r.Header.Get("X-Spark-Account")
	if !h.ScopeGuard(acct) {
		http.Error(w, "forbidden: 策略实验室仅限管理层（D6）", http.StatusForbidden)
		return false
	}
	return true
}

// audit 写审计，返回 (是否留痕, 告警文案)。
//
// ★ 审计失败**不阻断**决策：策略变更本身已落库，此时再报错会让用户
//
//	以为「没改成功」而重复点击 —— 那会落两条历史。
//	所以返回 audited=false + 人话告警，由前端显式提示。
func (h *StrategyHandlers) audit(ctx context.Context, action, target string, detail map[string]any) (bool, string) {
	if h.Auditor == nil {
		return false, "审计存储不可用：本次变更未留痕，请人工记录"
	}
	if err := h.Auditor.RecordStrategyChange(ctx, actorOf(ctx), action, target, detail); err != nil {
		return false, "审计写入失败（变更已生效）：" + err.Error()
	}
	return true, ""
}

// strategyActorKey 是放在 ctx 里的决策人（避免每个助手函数都传 actor）。
type strategyActorKey struct{}

// withActor 把决策人放进 ctx（审计帮手用）。
func withActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, strategyActorKey{}, actor)
}

func actorOf(ctx context.Context) string {
	if a, ok := ctx.Value(strategyActorKey{}).(string); ok {
		return a
	}
	return ""
}

func (h *StrategyHandlers) snapshotHash(ctx context.Context) string {
	if h.SnapshotHash == nil {
		return ""
	}
	return h.SnapshotHash(ctx)
}

// riskResolver 构造「(choiceID, optionKey) → Risk」的查询函数。
//
// ★ 用闭包从已加载的卡片里查，避免为每个历史条目各发一次查询
//
//	（历史可能有几百条，N+1 会拖垮偏好接口）。
func (h *StrategyHandlers) riskResolver(ctx context.Context, choices []*strategy.Choice) func(string, string) (strategy.Risk, bool) {
	idx := map[string]*strategy.Choice{}
	for _, c := range choices {
		if c != nil {
			idx[c.ID] = c
		}
	}
	return func(choiceID, optionKey string) (strategy.Risk, bool) {
		c, ok := idx[choiceID]
		if !ok {
			// 缓存未命中：仅此时才回源（历史里可能含已不在待决策列表的卡）
			c2, err := h.Svc.LoadChoice(ctx, choiceID, false)
			if err != nil || c2 == nil {
				return "", false
			}
			c = c2
		}
		o, ok := c.Option(optionKey)
		if !ok {
			return "", false
		}
		return o.Risk, true
	}
}

// nullableString 把 (值, 是否存在) 映射为「值或 null」，供 JSON 输出。
func nullableString(s string, ok bool) any {
	if !ok {
		return nil
	}
	return s
}
