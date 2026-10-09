// delegation.go —— M-AUTH「上级代授」（D13）的 HTTP 接口层。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本文件存在的唯一理由，是补上一个**恒真断言**：
//
//	`gate.CheckDelegationNoOverflow` 断言的是 `DelegationAllowed(granter, grantee)`
//	的返回值，而生产侧此前**没有任何代码会构造一份代授** ——
//	`authz.DelegationAllowed` 的唯一非测试调用点就是那条断言本身。
//	于是「代授不溢出自身范围」在实现侧恒真：没有代授路径，自然从不溢出。
//	（`contracts/entitlement.ts:CheckDelegationBound` 的注释把这件事写得很直白：
//	「若生产侧从不构造 DelegationRequest，则『代授不溢出』恒真」。）
//
//	`POST /api/authz/delegate` 把「代授」变成一条真实可执行、
//	且能落审计与来源分层的路径。
//
// ══════════════════════════════════════════════════════════════════════════
// 三条纪律（接口层负责，业务层不重复）：
//
//  1. **身份一律取自请求头**，`granter` 必须**逐字等于**当前登录人。
//     若允许请求体里的 `granter` 生效，任何登录用户都能以别人的名义代授
//     —— 而「谁授出的」正是审计与不溢出的**唯一依据**（upperBoundRef）。
//     管理员也不例外：代授的可归责性优先于便利。
//
//  2. **被授人必须真实存在**。代授给一个查不到的账号是**静默无效**：
//     求值不会报错，但那份权限永远没人用得上，事后无从发现。
//     故不存在即 404，而不是「照授不误」。
//
//  3. **落库失败不得谎报成功**。求值成功只说明「不溢出」；
//     权限真正生效还差落库那一步。落库失败 ⇒ 明确 500 并说明「未生效」。
//
// ══════════════════════════════════════════════════════════════════════════
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
)

// DelegationSink 代授落库（挂到被授人 `Entitlement.grants` + 落审计）。
//
// ★ 用接口而不是直接依赖 store：HTTP 层因此可以只用内存替身做单测，
//
//	且库未就绪时能如实回报 `persisted=false`，而不是挂一个假实现。
type DelegationSink interface {
	SaveDelegation(ctx context.Context, d *authz.Delegation, actor string) error
}

// DelegationHandlers 代授的 HTTP 处理器。
type DelegationHandlers struct {
	// Resolver 求值器（提供 Delegate 的「不溢出」判定）。
	Resolver *authz.Resolver
	// Entitlements 取某账号的原始授权（授出者上界 + 被授人存在性）。
	Entitlements func(account string) (*authz.Entitlement, []*authz.Entitlement)
	// Sink 落库；nil = 只求值不落库（响应如实回报 persisted=false）。
	Sink DelegationSink
	// Now 可注入时钟（测试用）；nil ⇒ time.Now().UTC()。
	Now func() time.Time
}

// Routes 注册代授路由。
func (h *DelegationHandlers) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/authz/delegate", h.handleDelegate)
}

// delegateBody 代授请求体。
//
// ★ 刻意**不含** `actor` 字段：身份只从请求头取，body 里带了也无效。
//
//	这是防 IDOR 的第一道门（与 groups.go 同源纪律）。
type delegateBody struct {
	Granter     string                `json:"granter"`
	Grantee     string                `json:"grantee"`
	Scope       authz.DelegationScope `json:"scope"`
	DelegateAll bool                  `json:"delegateAll"`
	At          string                `json:"at,omitempty"`
}

func (h *DelegationHandlers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now().UTC()
}

func (h *DelegationHandlers) handleDelegate(w http.ResponseWriter, r *http.Request) {
	// 纪律 1（上半）：身份只从请求头取。
	actor := r.Header.Get("X-Spark-Account")
	if actor == "" {
		http.Error(w, "missing identity", http.StatusUnauthorized)
		return
	}
	var in delegateBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid delegation request: "+err.Error(), http.StatusBadRequest)
		return
	}
	granter := strings.TrimSpace(in.Granter)
	grantee := strings.TrimSpace(in.Grantee)
	if granter == "" || grantee == "" {
		http.Error(w, "granter/grantee 不得为空", http.StatusBadRequest)
		return
	}
	// 纪律 1（下半）：不得以他人名义代授。
	if granter != actor {
		http.Error(w, "forbidden: granter 必须为当前登录人（不得以他人名义代授）", http.StatusForbidden)
		return
	}
	if in.At != "" {
		if _, err := time.Parse(time.RFC3339, in.At); err != nil {
			http.Error(w, "at 必须是 RFC3339 时间："+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if h.Resolver == nil || h.Entitlements == nil {
		http.Error(w, "delegation unavailable", http.StatusServiceUnavailable)
		return
	}

	// 纪律 2：被授人必须真实存在（代授给幽灵账号是静默无效）。
	if _, _, ok := h.entitlementOf(grantee); !ok {
		http.Error(w, "unknown grantee: 被授人不存在", http.StatusNotFound)
		return
	}
	granterEnt, granterGroups, ok := h.entitlementOf(granter)
	if !ok {
		http.Error(w, "unknown granter: 授出者不存在", http.StatusForbidden)
		return
	}

	req := authz.DelegationRequest{
		Granter:     granter,
		Grantee:     grantee,
		Scope:       in.Scope,
		DelegateAll: in.DelegateAll,
		At:          in.At,
	}
	if req.At == "" {
		req.At = h.now().Format(time.RFC3339)
	}

	d, err := h.Resolver.Delegate(req, granterEnt, granterGroups)
	if err != nil {
		var de *authz.DelegationError
		if errors.As(err, &de) {
			status := http.StatusConflict
			if de.Code == authz.ErrCodeSameAccount {
				status = http.StatusBadRequest
			}
			writeJSONStatus(w, status, map[string]any{
				"code":    de.Code,
				"message": de.Message,
			})
			return
		}
		http.Error(w, "delegate failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 纪律 3：落库失败不得谎报成功。
	persisted := false
	if h.Sink != nil {
		if err := h.Sink.SaveDelegation(r.Context(), d, actor); err != nil {
			http.Error(w, "代授已求值但落库失败（未生效）："+err.Error(), http.StatusInternalServerError)
			return
		}
		persisted = true
	}

	writeJSON(w, map[string]any{
		"delegation": d,
		"persisted":  persisted,
		// 未装配 Sink 时权限不会生效 —— 如实告知，不假装成功。
		"audited": persisted,
		"note":    delegationNote(persisted),
	})
}

func delegationNote(persisted bool) string {
	if persisted {
		return ""
	}
	return "已通过不溢出校验，但未装配落库（persisted=false）⇒ 权限尚未生效"
}

// entitlementOf 取账号的原始授权；不存在返回 ok=false。
func (h *DelegationHandlers) entitlementOf(account string) (*authz.Entitlement, []*authz.Entitlement, bool) {
	e, groups := h.Entitlements(account)
	if e == nil {
		return nil, nil, false
	}
	return e, groups, true
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
