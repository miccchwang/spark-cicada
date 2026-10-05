// admin_handlers.go —— M-ADMIN 集成控制面的 HTTP 端点。
//
// 端点（均需 X-Spark-Account 且必须是管理员）：
//
//	GET  /api/admin/overview     控制面总览（槽 / 算法 / 模块）
//	POST /api/admin/slots        登记/更新数据槽
//	POST /api/admin/algorithms   登记算法（G4 校验 + fail-closed 传播）
//
// **纪律**：控制面**只改注册表**，绝不触碰业务事实表；
// 每次变更都写审计（G10）。DB 未就绪 ⇒ 503（fail-closed，不假装成功）。
package main

import (
	"encoding/json"
	"net/http"

	"github.com/miccchwang/spark-cicada/backend/internal/admin"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// adminGuard 校验调用者是管理员；返回 (actor, ok)。
//
// 生产应接真实鉴权（角色表 + 职权向下覆盖，D13）；此处以账号白名单兜底。
func adminGuard(w http.ResponseWriter, r *http.Request) (string, bool) {
	actor := r.Header.Get("X-Spark-Account")
	if actor == "" {
		http.Error(w, "missing identity", http.StatusUnauthorized)
		return "", false
	}
	// 最小管理员集（演示）：ceo / vp.* 具备控制面权限
	switch actor {
	case "ceo", "vp.sea", "vp.us":
		return actor, true
	}
	http.Error(w, "forbidden: 需要管理员权限", http.StatusForbidden)
	return "", false
}

// adminOverviewHandler GET /api/admin/overview
func adminOverviewHandler(dp *dataPlane) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if _, ok := adminGuard(w, r); !ok {
			return
		}
		if dp.admin == nil {
			http.Error(w, "control plane unavailable: db not configured", http.StatusServiceUnavailable)
			return
		}
		ov, err := dp.admin.Snapshot(r.Context())
		if err != nil {
			http.Error(w, "snapshot failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, ov)
	}
}

// adminSlotHandler POST /api/admin/slots —— 登记/更新数据槽。
func adminSlotHandler(dp *dataPlane) http.HandlerFunc {
	type body struct {
		ID           string  `json:"id"`
		Name         string  `json:"name"`
		SourceKind   string  `json:"sourceKind"`
		SourceRef    string  `json:"sourceRef"`
		KeyStrategy  string  `json:"keyStrategy"`
		CoverageGate float64 `json:"coverageGate"`
		Freshness    string  `json:"freshness"`
		Permission   string  `json:"permission"`
		Status       string  `json:"status"`
		Notes        string  `json:"notes"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor, ok := adminGuard(w, r)
		if !ok {
			return
		}
		if dp.admin == nil {
			http.Error(w, "control plane unavailable: db not configured", http.StatusServiceUnavailable)
			return
		}
		var in body
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid slot: "+err.Error(), http.StatusBadRequest)
			return
		}
		err := dp.admin.RegisterSlot(r.Context(), actor, admin.Slot{
			ID: in.ID, Name: in.Name, SourceKind: in.SourceKind, SourceRef: in.SourceRef,
			KeyStrategy: in.KeyStrategy, CoverageGate: in.CoverageGate,
			Freshness: in.Freshness, Permission: in.Permission, Status: in.Status, Notes: in.Notes,
		})
		if err != nil {
			// 校验失败属客户端错误
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": in.ID})
	}
}

// adminAlgoHandler POST /api/admin/algorithms —— 登记算法。
//
// 关键：G4（公式不含数据源）+ G5（依赖槽不可 MISSING）在 admin.Plane 内强制，
// 失败返回 400 并说明原因 —— 拒绝「带病登记」。
func adminAlgoHandler(dp *dataPlane) http.HandlerFunc {
	type body struct {
		ID             string   `json:"id"`
		Name           string   `json:"name"`
		Version        int      `json:"version"`
		Formula        string   `json:"formula"`
		Unit           string   `json:"unit"`
		Permission     string   `json:"permission"`
		DependsOnSlots []string `json:"dependsOnSlots"`
		WritesBucket   string   `json:"writesBucket"`
		MissingPolicy  string   `json:"missingPolicy"`
		Trace          bool     `json:"trace"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor, ok := adminGuard(w, r)
		if !ok {
			return
		}
		if dp.admin == nil {
			http.Error(w, "control plane unavailable: db not configured", http.StatusServiceUnavailable)
			return
		}
		var in body
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid algorithm: "+err.Error(), http.StatusBadRequest)
			return
		}
		err := dp.admin.RegisterAlgorithm(r.Context(), actor, admin.Algorithm{
			ID: in.ID, Name: in.Name, Version: in.Version, Formula: in.Formula,
			Unit: in.Unit, Permission: in.Permission, DependsOnSlots: in.DependsOnSlots,
			WritesBucket: in.WritesBucket, MissingPolicy: in.MissingPolicy, Trace: in.Trace,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": in.ID})
	}
}

// adminModulesHandler GET /api/admin/modules —— 模块渲染清单（G9）。
func adminModulesHandler(dp *dataPlane) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminGuard(w, r); !ok {
			return
		}
		if dp.registry == nil {
			http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
			return
		}
		var mods []gate.Module
		for _, m := range dp.registry.ActiveModules() {
			mods = append(mods, m)
		}
		writeJSON(w, map[string]any{
			"rendered": dp.registry.RenderModules(),
			"modules":  mods,
		})
	}
}
