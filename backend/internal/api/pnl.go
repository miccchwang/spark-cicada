// pnl.go —— M-PNL（损益口径）的 HTTP 接口层。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本层要防守的是「口径被谁悄悄改了」——这是一个**看起来无害、实际很危险**
//
//	的接口，因为口径改的是「同一份数据怎么读」，不是数据本身。
//
//	1. **口径必须在服务端解析与校验**。前端可以传 `?caliber=A`，但服务端
//	   绝不接受「传了脏值就当 A」——那会让财务拿着 A 的数字以为在看 B。
//	   主动切换走 ParseCaliber（非法 ⇒ 400）；首次加载走 Resolve（回退但报告）。
//
//	2. **切换必须留痕**。口径决定了同一份数据被读成「巨亏」还是「微利」。
//	   两个部门拿着不一致的截图争吵时，唯一能厘清的就是审计记录。
//
//	3. **不变量必须在响应里被验证并暴露**。net_revenue 两口径必须相同；
//	   一旦不同，宁可报 500（明确故障）也不要返回一张各口径自相矛盾的表。
//
//	4. **只回口径元数据，不回数值计算**。数值来自 compute 内核与预计算桶；
//	   本层的 /api/pnl/calibers 只回答「有哪些口径、各自什么意思、默认是哪个」。
//
// ══════════════════════════════════════════════════════════════════════════
package api

import (
	"encoding/json"
	"net/http"

	"github.com/miccchwang/spark-cicada/backend/internal/pnl"
)

// CaliberAuditor 记录口径切换（由 M-ADMIN 的 append-only 审计实现）。
//
// ★ 用接口而不是直接依赖 store：本层因此可以在无库时照常提供元数据接口，
//
//	且 HTTP 层单测用内存替身即可。
type CaliberAuditor interface {
	// RecordCaliberChange 落一条口径切换审计。
	RecordCaliberChange(change pnl.CaliberChange) error
}

// PnlHandlers 损益口径的 HTTP 处理器。
type PnlHandlers struct {
	// Auditor 可空（无库/单测时）；为空则只返回元数据、不落审计。
	Auditor CaliberAuditor
}

// Routes 注册路由。
func (h *PnlHandlers) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pnl/calibers", h.handleCalibers)
	mux.HandleFunc("POST /api/pnl/caliber", h.handleSetCaliber)
}

// caliberView 单条口径视图（字段名与 contracts/pnl.ts 的 CaliberMeta 对齐）。
type caliberView struct {
	ID                 string `json:"id"`
	Short              string `json:"short"`
	Label              string `json:"label"`
	Definition         string `json:"definition"`
	MisreadingRisk     string `json:"misreadingRisk"`
	SellerDiscountRole string `json:"sellerDiscountRole"`
	// DiscountTargetLine 折扣在该口径下归属的行（结构差异的唯一落点）。
	DiscountTargetLine string `json:"discountTargetLine"`
	// IsDefaultForModule 在当前模块下是否为默认口径。
	IsDefaultForModule bool `json:"isDefaultForModule"`
}

// handleCalibers GET /api/pnl/calibers?module=module.pnl&caliber=A
//
// 返回口径清单 + 每个口径的元数据 + 当前生效口径（含默认与回退原因）。
//
// ★ 把「为什么最终用了这个口径」一并返回（source/reason），
//
//	这样前端可以如实告诉用户，而不是让用户自己猜。
func (h *PnlHandlers) handleCalibers(w http.ResponseWriter, r *http.Request) {
	module := r.URL.Query().Get("module")
	if module == "" {
		module = pnl.ModulePnL
	}
	requested := r.URL.Query().Get("caliber")

	res := pnl.Resolve(requested, module)

	views := make([]caliberView, 0, len(pnl.Calibers))
	for _, c := range pnl.AllMeta() {
		views = append(views, caliberView{
			ID:                 string(c.ID),
			Short:              c.Short,
			Label:              c.Label,
			Definition:         c.Definition,
			MisreadingRisk:     c.MisreadingRisk,
			SellerDiscountRole: c.SellerDiscountRole,
			DiscountTargetLine: string(pnl.DiscountTarget(c.ID)),
			IsDefaultForModule: pnl.DefaultCaliber(module) == c.ID,
		})
	}

	writeJSON(w, map[string]any{
		"module":           module,
		"effectiveCaliber": string(res.Caliber),
		"source":           res.Source, // requested | module_default | fallback
		"downgraded":       res.Downgraded,
		"reason":           res.Reason,
		"calibers":         views,
		// 决策依据（供 M-ADMIN 页面展示「为什么这个模块默认是这个口径」）
		"decision": "D10：分模块各自默认（经营报表=运营口径 A，P&L=财务口径 B）",
	})
}

// setCaliberReq 切换口径的请求体。
type setCaliberReq struct {
	Module string `json:"module"`
	// To 目标口径。★ 必须显式给出且合法 —— 不提供「留空即切换」的捷径。
	To string `json:"to"`
	// From 当前口径（由前端回传，用于审计「从哪切到哪」）。
	From string `json:"from"`
}

// handleSetCaliber POST /api/pnl/caliber
//
// 记录一次口径切换（审计）并回显生效口径。
//
// ★ 本接口**不做任何数值计算** —— 它只回答「口径定成什么了」。
//
//	报表数值仍由 /api/query 走内核产出（口径通过 QueryState 携带）。
func (h *PnlHandlers) handleSetCaliber(w http.ResponseWriter, r *http.Request) {
	account := r.Header.Get("X-Spark-Account")
	if account == "" {
		http.Error(w, "missing identity", http.StatusUnauthorized)
		return
	}

	var req setCaliberReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Module == "" {
		req.Module = pnl.ModulePnL
	}

	// ★ 主动切换：非法口径**必须** 400（绝不静默回退）。
	to, err := pnl.ParseCaliber(req.To)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// From 允许为空（首次设置）；给了就必须合法，否则审计记录会失真。
	var from pnl.Caliber
	if req.From != "" {
		from, err = pnl.ParseCaliber(req.From)
		if err != nil {
			http.Error(w, "invalid from: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	change := pnl.CaliberChange{
		Account:   account,
		Module:    req.Module,
		From:      from,
		To:        to,
		Requested: req.To,
		Source:    "requested",
	}
	if err := change.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 审计失败**不阻断**用户切口径（口径是展示偏好，不是安全边界），
	// 但必须如实告知「未留痕」——而不是假装记上了。
	audited := false
	if h.Auditor != nil {
		if err := h.Auditor.RecordCaliberChange(change); err != nil {
			writeJSON(w, map[string]any{
				"ok":      true,
				"caliber": string(to),
				"module":  req.Module,
				"audited": false,
				"warning": "口径已切换，但审计未能落库：" + err.Error(),
			})
			return
		}
		audited = true
	}

	writeJSON(w, map[string]any{
		"ok":         true,
		"caliber":    string(to),
		"module":     req.Module,
		"audited":    audited,
		"summary":    change.ChangeSummary(),
		"priceNote":  "口径切换只改变呈现归属；净收入不受影响（见 docs/03 §4.3）",
		"riskNotice": pnl.MisreadingRisk(to),
	})
}
