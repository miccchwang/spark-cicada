package api_test

import (
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/api"
	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
)

func docWith(cols []contracts.ColumnDef, rows []contracts.Row) *contracts.DataContract {
	return &contracts.DataContract{
		V:         contracts.DataContractVersion,
		Columns:   cols,
		Rows:      rows,
		Aggregates: map[string]*float64{},
	}
}

func policy() api.FieldPolicy {
	return api.FieldPolicy{
		FieldLevel: map[string]authz.Level{
			"revenue": authz.L2,
			"gp":      authz.L3,
			"cogs":    authz.L4,
		},
		FieldGroup: map[string]authz.DataUseGroup{
			"cogs":        "grp.cost_profit",
			"net_contrib": "grp.roi",
		},
		BusinessValueFields: map[string]bool{
			"revenue": true, "gp": true, "cogs": true, "net_contrib": true,
		},
	}
}

// 未授权字段绝不进入响应（fail-closed 红线）。
func TestGate_UnauthorizedFieldsNeverEmitted(t *testing.T) {
	dc := docWith(
		[]contracts.ColumnDef{
			{Key: "month", Perm: "L1", Kind: "date"},
			{Key: "revenue", Perm: "L2", Kind: "currency"},
			{Key: "gp", Perm: "L3", Kind: "currency"},
			{Key: "cogs", Perm: "L4", Kind: "currency"},
		},
		[]contracts.Row{{
			"month":   contracts.NewStr("2026-09"),
			"revenue": contracts.NewNum(1000),
			"gp":      contracts.NewNum(380),
			"cogs":    contracts.NewNum(620),
		}},
	)
	// 用户只有 L2，且无勾选组
	view := &authz.EntitlementView{
		Account: "u", MaxLevel: authz.L2,
		CanViewBusinessValues: true,
		Dimensions:            map[string][]string{},
	}
	out := api.Gate(dc, view, policy())

	got := map[string]bool{}
	for _, c := range out.Columns {
		got[c.Key] = true
	}
	if got["gp"] || got["cogs"] {
		t.Fatalf("L3/L4 fields leaked for L2 user: %v", got)
	}
	if !got["revenue"] || !got["month"] {
		t.Fatalf("authorized fields missing: %v", got)
	}
	// 行内值也必须无泄漏
	for _, r := range out.Rows {
		if _, ok := r["gp"]; ok {
			t.Fatalf("gp value leaked in row")
		}
		if _, ok := r["cogs"]; ok {
			t.Fatalf("cogs value leaked in row")
		}
	}
}

// IT（canViewBusinessValues=false）不得看到任何业务数值。
func TestGate_ITSeesNoBusinessValues(t *testing.T) {
	dc := docWith(
		[]contracts.ColumnDef{
			{Key: "month", Perm: "L1", Kind: "date"},
			{Key: "revenue", Perm: "L2", Kind: "currency"},
		},
		[]contracts.Row{{"month": contracts.NewStr("2026-09"), "revenue": contracts.NewNum(1000)}},
	)
	view := &authz.EntitlementView{
		Account: "it", MaxLevel: authz.L4,
		CanViewBusinessValues: false, // D7
		Dimensions:            map[string][]string{},
	}
	out := api.Gate(dc, view, policy())
	for _, c := range out.Columns {
		if c.Key == "revenue" {
			t.Fatalf("D7 violated: IT must not see business value column")
		}
	}
}

// 勾选组未命中 ⇒ 字段剔除。
func TestGate_GroupRequired(t *testing.T) {
	dc := docWith(
		[]contracts.ColumnDef{
			{Key: "month", Perm: "L1", Kind: "date"},
			{Key: "cogs", Perm: "L4", Kind: "currency"},
		},
		[]contracts.Row{{"month": contracts.NewStr("2026-09"), "cogs": contracts.NewNum(620)}},
	)
	// 有 L4 密级但未勾选 grp.cost_profit
	view := &authz.EntitlementView{
		Account: "u", MaxLevel: authz.L4,
		CanViewBusinessValues: true,
		DataUseGroups:         []authz.DataUseGroup{"grp.ops"},
		Dimensions:            map[string][]string{},
	}
	out := api.Gate(dc, view, policy())
	for _, c := range out.Columns {
		if c.Key == "cogs" {
			t.Fatalf("cogs requires grp.cost_profit; should be gated out")
		}
	}
}

// 勾选组命中 ⇒ 字段保留。
func TestGate_GroupGranted(t *testing.T) {
	dc := docWith(
		[]contracts.ColumnDef{
			{Key: "month", Perm: "L1", Kind: "date"},
			{Key: "cogs", Perm: "L4", Kind: "currency"},
		},
		[]contracts.Row{{"month": contracts.NewStr("2026-09"), "cogs": contracts.NewNum(620)}},
	)
	view := &authz.EntitlementView{
		Account: "u", MaxLevel: authz.L4,
		CanViewBusinessValues: true,
		DataUseGroups:         []authz.DataUseGroup{"grp.cost_profit"},
		Dimensions:            map[string][]string{},
	}
	out := api.Gate(dc, view, policy())
	found := false
	for _, c := range out.Columns {
		if c.Key == "cogs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cogs should be visible when grp.cost_profit granted")
	}
}

// 维度键列始终可见（用于行标识/跳转）。
func TestGate_KeyColumnsAlwaysVisible(t *testing.T) {
	dc := docWith(
		[]contracts.ColumnDef{{Key: "month", Perm: "L1", Kind: "date"}},
		[]contracts.Row{{"month": contracts.NewStr("2026-09")}},
	)
	view := &authz.EntitlementView{
		Account: "u", MaxLevel: authz.L1,
		CanViewBusinessValues: true, Dimensions: map[string][]string{},
	}
	out := api.Gate(dc, view, policy())
	if len(out.Rows) != 1 || out.Rows[0]["month"] == nil {
		t.Fatalf("key column must remain visible for navigation")
	}
}

// nil 输入不得 panic（fail-closed 返回空文档）。
func TestGate_NilSafe(t *testing.T) {
	out := api.Gate(nil, nil, api.FieldPolicy{})
	if out == nil || len(out.Columns) != 0 || len(out.Rows) != 0 {
		t.Fatalf("nil input should yield empty contract")
	}
}
