// Package api —— HTTP 接口层（Go）。
//
// 职责：把 M-QUERY / M-AUTH / M-REQ 暴露为 HTTP。
//
// **fail-closed 红线（docs/01 §13、闸门 G2）**：
//
//	任何未授权字段**绝不能**进入响应体（DOM/导出/API 一律如此）。
//	渲染层拿不到 = 攻击面为零。本层在序列化前做字段级门控。
package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/query"
)

// FieldPolicy 字段级访问策略。
type FieldPolicy struct {
	// field → 所需密级
	FieldLevel map[string]authz.Level
	// field → 所需勾选组（空 = 无组要求）
	FieldGroup map[string]authz.DataUseGroup
	// 业务数值字段（IT 不可见时全部剔除）
	BusinessValueFields map[string]bool
}

// Gate 依据 EntitlementView 过滤 DataContract（fail-closed）。
//
// 规则：
//  1. 字段密级 > view.MaxLevel ⇒ 剔除
//  2. 字段所需勾选组不在 view.DataUseGroups ⇒ 剔除
//  3. 字段属业务数值 且 !view.CanViewBusinessValues ⇒ 剔除（D7）
//  4. 被剔除的列，其行内值也一并移除
func Gate(dc *contracts.DataContract, view *authz.EntitlementView, p FieldPolicy) *contracts.DataContract {
	if dc == nil || view == nil {
		return &contracts.DataContract{V: contracts.DataContractVersion, Rows: []contracts.Row{}}
	}
	groupSet := map[authz.DataUseGroup]bool{}
	for _, g := range view.DataUseGroups {
		groupSet[g] = true
	}

	allowedCols := make([]contracts.ColumnDef, 0, len(dc.Columns))
	allowed := map[string]bool{}
	for _, col := range dc.Columns {
		// ① 密级
		if lvl, ok := p.FieldLevel[col.Key]; ok {
			if rank(lvl) > rank(view.MaxLevel) {
				continue
			}
		} else if rank(authz.Level(col.Perm)) > rank(view.MaxLevel) {
			continue
		}
		// ② 勾选组
		if g, ok := p.FieldGroup[col.Key]; ok && !groupSet[g] {
			continue
		}
		// ③ 业务数值（D7）
		if p.BusinessValueFields[col.Key] && !view.CanViewBusinessValues {
			continue
		}
		allowedCols = append(allowedCols, col)
		allowed[col.Key] = true
	}

	// 行内值剔除（未授权字段绝不出现在响应里）
	rows := make([]contracts.Row, 0, len(dc.Rows))
	for _, r := range dc.Rows {
		nr := contracts.Row{}
		for k, v := range r {
			if allowed[k] || isKeyColumn(k, dc) {
				nr[k] = v
			}
		}
		rows = append(rows, nr)
	}

	// 聚合值同样门控
	aggs := map[string]*float64{}
	for k, v := range dc.Aggregates {
		if allowed[k] {
			aggs[k] = v
		}
	}

	// algoTrace / gaps 只保留可解释的列
	traces := make([]contracts.AlgoTrace, 0, len(dc.AlgoTrace))
	for _, t := range dc.AlgoTrace {
		if allowed[t.Field] {
			traces = append(traces, t)
		}
	}
	gaps := make([]contracts.DataGap, 0, len(dc.Gaps))
	for _, g := range dc.Gaps {
		if allowed[g.Field] {
			gaps = append(gaps, g)
		}
	}

	out := *dc
	out.Columns = allowedCols
	out.Rows = rows
	out.Aggregates = aggs
	out.AlgoTrace = traces
	out.Gaps = gaps
	return &out
}

// isKeyColumn 维度键列（非值列）始终可见，用于行标识与跳转。
func isKeyColumn(k string, dc *contracts.DataContract) bool {
	switch k {
	case "month", "channel_code", "shop_id", "brand", "spu", "sku", "level", "key", "label":
		return true
	}
	return false
}

func rank(l authz.Level) int {
	switch l {
	case authz.L1:
		return 1
	case authz.L2:
		return 2
	case authz.L3:
		return 3
	case authz.L4:
		return 4
	}
	return 0
}

// ───────────────────────────── HTTP Handlers ─────────────────────────────

// Server 聚合依赖。
type Server struct {
	Orch     *query.Orchestrator
	Resolve  func(account string) *authz.EntitlementView
	Policy   FieldPolicy
	BuildDoc func(rs *query.ResultSet) *contracts.DataContract
}

// QueryHandler 处理 POST /api/query。
// 入参：QueryState（JSON）。出参：门控后的 DataContract。
func (s *Server) QueryHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var q contracts.QueryState
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			http.Error(w, "invalid QueryState: "+err.Error(), http.StatusBadRequest)
			return
		}
		account := r.Header.Get("X-Spark-Account")
		if account == "" {
			http.Error(w, "missing identity", http.StatusUnauthorized)
			return
		}
		view := s.Resolve(account)
		if view == nil {
			http.Error(w, "unknown account", http.StatusForbidden)
			return
		}

		rs, err := s.Orch.Run(r.Context(), q)
		if err != nil {
			http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		doc := s.BuildDoc(rs)
		// ★ fail-closed 门控：未授权字段在序列化前剔除
		doc = Gate(doc, view, s.Policy)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// MeHandler 处理 GET /api/me —— 返回当前账号权限视图（供前端勾选树渲染）。
func (s *Server) MeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("X-Spark-Account")
		if account == "" {
			http.Error(w, "missing identity", http.StatusUnauthorized)
			return
		}
		view := s.Resolve(account)
		if view == nil {
			http.Error(w, "unknown account", http.StatusForbidden)
			return
		}
		// 规范化输出（键排序，便于测试与缓存）
		sort.Strings(view.Modules)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(view)
	}
}
