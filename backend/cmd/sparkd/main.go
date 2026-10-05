// Command sparkd —— 平台主进程（编排层入口）。
//
// 职责（docs/02）：
//   * 装配 M-QUERY / M-AUTH / M-REQ / M-PRECOMP；
//   * 暴露 HTTP（/api/query、/api/me、/healthz）；
//   * 启动时执行**启动自检**（闸门 G6：桶版本 vs 注册表版本）。
//
// **边界纪律**：本进程不做任何业务公式计算，一切数值经 compute 内核（Rust）。
// 内核二进制路径由 SPARK_COMPUTE_BIN 指定（默认 ./spark-compute）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/api"
	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/compute"
	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/query"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

func main() {
	var (
		addr       = flag.String("addr", ":8080", "HTTP 监听地址")
		computeBin = flag.String("compute-bin", envOr("SPARK_COMPUTE_BIN", "spark-compute"), "compute 内核二进制路径")
		selftest   = flag.Bool("selftest", false, "仅启动自检后退出（CI/容器 liveness）")
	)
	flag.Parse()

	kernel := compute.NewSubprocessKernel(*computeBin)

	// ── 启动自检：内核可用性 + 版本漂移（G6）──
	if err := startupChecks(context.Background(), kernel); err != nil {
		log.Fatalf("启动自检失败（fail-closed，拒绝启动）：%v", err)
	}
	if *selftest {
		fmt.Println("selftest ok")
		return
	}

	// ── 装配 ──
	org := buildOrgDirectory()
	resolver := authz.NewResolver(builtinTemplates())
	ents := buildEntitlementSource()
	reqSvc := req.NewService(org, resolver, ents)

	orch := &query.Orchestrator{
		Store:  nil, // 生产接 DB；未配置 ⇒ 查询返回错误（fail-closed）
		Cache:  nil,
		Kernel: kernel,
		Policy: query.Policy{
			AllowStaleBucket: false, // 默认 fail-closed
			CacheTTL:         60 * time.Second,
			BucketAlgoMap: map[string]map[string]string{
				"pnl_month": {"gp": "algo.gp", "cogs": "algo.cogs", "net_contrib": "algo.net_contrib"},
			},
		},
	}

	srv := &api.Server{
		Orch:    orch,
		Resolve: func(account string) *authz.EntitlementView { return resolveFor(resolver, ents, account) },
		Policy: api.FieldPolicy{
			FieldLevel: map[string]authz.Level{
				"gp":    authz.L3,
				"cogs":  authz.L3,
				"net_contrib": authz.L4,
			},
			FieldGroup: map[string]authz.DataUseGroup{
				"cogs":        authz.GrpCostProfit,
				"net_contrib": authz.GrpROI,
			},
			BusinessValueFields: map[string]bool{
				"gp": true, "cogs": true, "net_contrib": true,
			},
		},
		BuildDoc: func(rs *query.ResultSet) *contracts.DataContract {
			return &contracts.DataContract{
				V:           contracts.DataContractVersion,
				QueryHash:   rs.QueryHash,
				Rows:        rs.Rows,
				Columns:     rs.Columns,
				Aggregates:  rs.Aggregates,
				AlgoTrace:   rs.AlgoTrace,
				Gaps:        rs.Gaps,
				Precomputed: rs.Precomputed,
				GeneratedAt: rs.GeneratedAt.Format(time.RFC3339),
			}
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/query", srv.QueryHandler())
	mux.HandleFunc("/api/me", srv.MeHandler())
	mux.HandleFunc("/api/request", requestHandler(reqSvc))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		v, err := kernel.Health(r.Context())
		if err != nil {
			http.Error(w, "kernel unhealthy: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "kernel": v})
	})

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// ── 优雅退出 ──
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	log.Printf("sparkd listening on %s (compute kernel: %s)", *addr, *computeBin)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
	log.Println("sparkd stopped")
}

// startupChecks 启动自检（G6：桶版本一致性 + 内核健康）。
func startupChecks(ctx context.Context, k compute.Kernel) error {
	v, err := k.Health(ctx)
	if err != nil {
		return fmt.Errorf("compute kernel health: %w", err)
	}
	log.Printf("compute kernel ready: %s", v)

	// 桶版本 vs 注册表版本（此处以内置演示版本为例；生产从 DB 读）
	bucket := map[string]int{"algo.gp": 3, "algo.cogs": 2}
	current := map[string]int{"algo.gp": 3, "algo.cogs": 2}
	if drift := gate.VersionDrift(bucket, current); len(drift) > 0 {
		return fmt.Errorf("预计算桶版本漂移（G6）：%v", drift)
	}
	return nil
}

func resolveFor(res *authz.Resolver, ents func(string) (*authz.Entitlement, []*authz.Entitlement), account string) *authz.EntitlementView {
	e, groups := ents(account)
	if e == nil {
		return nil
	}
	return res.Resolve(e, groups)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// requestHandler 处理 POST /api/request —— 提交一次权限申请（M-REQ，D14）。
//
// 请求体：{"applicant":"...","draft":{...},"purpose":"...","dataChain":{...}}
// 出参：校验结果 + 申请单状态（BLOCKED_CROSS_DEPT / COSIGN_PENDING / APPROVING）。
func requestHandler(svc *req.Service) http.HandlerFunc {
	type dataChainDTO struct {
		CrossDept bool     `json:"crossDept"`
		Depts     []string `json:"depts"`
	}
	type body struct {
		Applicant string       `json:"applicant"`
		Purpose   string       `json:"purpose"`
		Draft     req.Draft    `json:"draft"`
		DataChain *dataChainDTO `json:"dataChain,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var in body
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		var dc *chain.DataChain
		if in.DataChain != nil {
			dc = &chain.DataChain{CrossDept: in.DataChain.CrossDept, Depts: in.DataChain.Depts}
		}
		reqObj := &req.Request{
			ID:        fmt.Sprintf("R-%d", time.Now().UnixNano()),
			Applicant: in.Applicant,
			Draft:     in.Draft,
			Purpose:   in.Purpose,
			CreatedAt: time.Now().UTC(),
		}
		v, err := svc.Submit(reqObj, dc)
		if err != nil {
			http.Error(w, "submit failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"id":         reqObj.ID,
			"status":     reqObj.Status,
			"validation": v,
			"approvals":  reqObj.Approvals,
			"ccs":        reqObj.CCs,
		})
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
