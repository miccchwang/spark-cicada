// sparkd 端到端 HTTP 测试。
//
// 用 httptest 直接驱动 handler，验证「fail-closed」在真实 HTTP 序列化后依然成立：
//   - IT 账号请求 → 响应体中**不含**任何业务数值字段（gp/cogs/net_contrib）
//   - 未授权账号 → 403
//   - /api/me 返回权限视图
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/api"
	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/query"
)

// testServer 构造一个不依赖 DB 的 Server（BuildDoc 直接回放固定数据）。
func testServer() *api.Server {
	resolver := authz.NewResolver(builtinTemplates())
	ents := buildEntitlementSource()
	return &api.Server{
		Orch: &query.Orchestrator{
			Store: stubStore{},
			Policy: query.Policy{
				BucketAlgoMap: map[string]map[string]string{
					"pnl_month": {"gp": "algo.gp", "cogs": "algo.cogs", "net_contrib": "algo.net_contrib"},
				},
			},
		},
		Resolve: func(account string) *authz.EntitlementView { return resolveFor(resolver, ents, account) },
		Policy: api.FieldPolicy{
			FieldLevel: map[string]authz.Level{
				"gp": authz.L3, "cogs": authz.L3, "net_contrib": authz.L4,
			},
			FieldGroup: map[string]authz.DataUseGroup{
				"cogs": authz.GrpCostProfit, "net_contrib": authz.GrpROI,
			},
			BusinessValueFields: map[string]bool{"gp": true, "cogs": true, "net_contrib": true},
		},
		BuildDoc: func(rs *query.ResultSet) *contracts.DataContract {
			return &contracts.DataContract{
				V: contracts.DataContractVersion, QueryHash: rs.QueryHash,
				Rows: rs.Rows, Columns: rs.Columns, AlgoTrace: rs.AlgoTrace, Gaps: rs.Gaps,
			}
		},
	}
}

type stubStore struct{}

func (stubStore) SelectBucket(_ context.Context, _ string, _ contracts.QueryState) ([]query.BucketRow, error) {
	return []query.BucketRow{
		{Keys: map[string]string{"month": "2026-09"},
			Metrics: map[string]*float64{"gp": ptr(1000), "cogs": ptr(400), "net_contrib": ptr(250)}},
	}, nil
}
func (stubStore) BucketState(_ context.Context, _ string) (query.BucketMeta, error) {
	return query.BucketMeta{ID: "pnl_month", State: "FRESH"}, nil
}
func (stubStore) SlotHealth(_ context.Context, _ []string) (map[string]query.SlotStatus, error) {
	return nil, nil
}

func ptr(f float64) *float64 { return &f }

func TestE2E_ITSeesNoBusinessValuesOverHTTP(t *testing.T) {
	srv := testServer()

	body := `{"v":"1.0","time":{"mode":"preset","grain":"month","timezone":"Asia/Singapore"},"filters":[],"dims":{"level":"overview"},"precomputeHint":{"bucket":"pnl_month"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewBufferString(body))
	req.Header.Set("X-Spark-Account", "it.ops")
	rec := httptest.NewRecorder()
	srv.QueryHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	for _, leak := range []string{`"gp"`, `"cogs"`, `"net_contrib"`} {
		if strings.Contains(raw, leak) {
			t.Fatalf("fail-closed 被击穿：IT 响应体泄漏业务字段 %s\n%s", leak, raw)
		}
	}

	// 对照：业务负责人（lead.sea）应当能看到 gp / cogs / net_contrib
	req2 := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewBufferString(body))
	req2.Header.Set("X-Spark-Account", "lead.sea")
	rec2 := httptest.NewRecorder()
	srv.QueryHandler()(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("业务负责人查询失败: %d %s", rec2.Code, rec2.Body.String())
	}
	raw2 := rec2.Body.String()
	if !strings.Contains(raw2, `"gp"`) {
		t.Fatalf("业务负责人应可见 gp，实际响应：%s", raw2)
	}
}

func TestE2E_UnknownAccountForbidden(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("X-Spark-Account", "ghost")
	rec := httptest.NewRecorder()
	srv.MeHandler()(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未知账号应 403，实际 %d", rec.Code)
	}
}

func TestE2E_MeReturnsView(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("X-Spark-Account", "lead.sea")
	rec := httptest.NewRecorder()
	srv.MeHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var view authz.EntitlementView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.CanViewBusinessValues {
		t.Fatal("业务负责人应可见业务数值")
	}
}
