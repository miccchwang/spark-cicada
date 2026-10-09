// Command sparkd —— 平台主进程（编排层入口）。
//
// 职责（docs/02）：
//   * 装配 M-QUERY / M-AUTH / M-REQ / M-PRECOMP；
//   * 暴露 HTTP（/api/query、/api/me、/healthz）+ 前端静态托管（SPARK_WEB_DIR）；
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
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// buildBucketAlgoRefs 从**真实注册表**派生「桶 → 字段 → 算法引用」，供 query
// 组装可回溯的 AlgoTrace（G4 第十四侧 / docs/01 §5.5、docs/02 验收「可回溯算法与数据槽」）。
//
// ★ 为什么不在装配层手抄：手抄的映射必然与 algorithms/*.yaml 分叉
//   （依赖槽 / trace 任一改动都不会同步），而 AlgoTrace 的可回溯性正建在它上面。
//   这里以 slotReg（算法注册表）+ buckets（桶定义）为**唯一事实源**。
//
// ★ 注册表未就绪（降级运行）时返回 nil：query 仍产出 AlgoTrace 但 dataSlots 为空，
//   出站契约守卫会 fail-closed 拒绝出站（宁可拒绝，也不出不可回溯的契约）。
func buildBucketAlgoRefs(dp *dataPlane) map[string]map[string]query.AlgoRef {
	if dp == nil || dp.slotReg == nil {
		return nil
	}
	raw := dp.slotReg.BucketAlgoRefs(dp.buckets)
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]map[string]query.AlgoRef, len(raw))
	for bucket, fields := range raw {
		m := make(map[string]query.AlgoRef, len(fields))
		for field, info := range fields {
			m[field] = query.AlgoRef{ID: info.AlgoID, Slots: info.Slots, Trace: info.Trace}
		}
		out[bucket] = m
	}
	return out
}

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

	// ── 数据面装配（DB 可选；缺席则降级为 fail-closed）──
	dp := buildDataPlane(context.Background())
	if err := dp.startupGateChecks(context.Background()); err != nil {
		log.Fatalf("数据面自检失败（fail-closed，拒绝启动）：%v", err)
	}

	// ── 装配 ──
	org := dp.org
	resolver := dp.resolver
	ents := dp.ents
	reqSvc := req.NewService(org, resolver, ents)

	// Store：真库就绪则接 Postgres；否则 nil（查询明确报错，不伪造数据）
	var qStore query.Store
	if dp.dbReady {
		qStore = dp.store
	}

	orch := &query.Orchestrator{
		Store:  qStore,
		Cache:  nil,
		Kernel: kernel,
		Policy: query.Policy{
			AllowStaleBucket: false, // 默认 fail-closed
			CacheTTL:         60 * time.Second,
			BucketAlgoMap: map[string]map[string]string{
				"pnl_month": {"gp": "algo.gp", "cogs": "algo.cogs", "net_contrib": "algo.net_contrib"},
			},
			// ★ 可回溯来源（G4 第十四侧）：桶 → 字段 → 算法引用（含依赖槽与 trace），
			//   从**真实注册表**派生，而非手抄（手抄必然与 algorithms/*.yaml 分叉）。
			BucketAlgoRefs: buildBucketAlgoRefs(dp),
		},
	}

	srv := &api.Server{
		Orch:    orch,
		Resolve: func(account string) *authz.EntitlementView { return resolveFor(resolver, ents, account) },
		// ★ 出站契约守卫（G2 默认收起 / G3 不补 0）—— 让两条判定函数获得真实生产调用点。
		//   zeroWhitelist 收纳「真实计算为 0」的字段；当前无此类字段（成本/毛利为 0 属异常），
		//   故留空 map —— 宁可多拦一次人工确认，也不放行一个静默补零。
		ContractGuard: slot.NewContractGuard(map[string]bool{}).MustValidContract,
		Policy: api.FieldPolicy{
			FieldLevel: map[string]authz.Level{
				"gp":          authz.L3,
				"cogs":        authz.L3,
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
	mux.HandleFunc("/api/admin/overview", adminOverviewHandler(dp))
	mux.HandleFunc("/api/admin/slots", adminSlotHandler(dp))
	mux.HandleFunc("/api/admin/algorithms", adminAlgoHandler(dp))
	mux.HandleFunc("/api/admin/modules", adminModulesHandler(dp))

	// ── M-PNL：口径元数据与切换 ──
	//
	// ★ 刻意**不放在 `if dp.dbReady` 里**：
	//   口径清单与元数据是**静态契约**（来自 contracts/pnl.ts 与 docs/03 §4.3），
	//   不依赖数据库。降级模式下仍应能回答「有哪些口径、默认是哪个」——
	//   否则用户在库还没起来时连「为什么报表是这个口径」都查不到。
	//   切换类请求在无库时**照样成功**，但会如实回报 audited=false
	//   （口径是展示偏好，不是安全边界；不因审计不可用就剥夺用户能力）。
	{
		var auditor api.CaliberAuditor
		if dp.caliberAudit != nil {
			auditor = dp.caliberAudit
		}
		pnlH := &api.PnlHandlers{Auditor: auditor}
		pnlH.Routes(mux)
	}

	// ── M-GROUP / M-REQ（组授权 + 申请流）与 M-TEMPLATE（模板中心）──
	//
	// ★ 二者共用同一套「管理员判定」，注入同一个 isAdmin 函数 ——
	//   若两处各写一份判断，迟早出现「组管理认他是管理员、模板管理不认」的错位。
	//
	// ★ 库未就绪时不注册这些路由 ⇒ 由兜底路由返回 404/503，
	//   而不是挂一个「查不到库所以返回空」的假实现（那才是真正危险）。
	if dp.dbReady {
		isAdmin := dp.isAdminFunc()
		groupsH := &api.GroupHandlers{
			Svc:     dp.groups,
			Req:     reqSvc,
			IsAdmin: isAdmin,
		}
		groupsH.Routes(mux)

		tplH := &api.TemplateHandlers{
			Svc:          dp.templates,
			IsAdmin:      isAdmin,
			IsSupervisor: dp.isSupervisorFunc(),
			// ★ 多租户：按请求头解析租户，取绑定租户的 store 实例。
			//   库未就绪时 dp.tenants==nil ⇒ 返回 nil ⇒ 403，绝不回退无租户通道。
			ForTenant: dp.templatesForTenant,
		}
		tplH.Routes(mux)

		// ── M-STRATEGY：策略实验室 ──
		//
		// ★ 放在 `if dp.dbReady` 里（与 M-PNL 相反）：策略变更会写规则集、
		//   让预计算桶过期 —— 是有副作用的状态改动，不能「无库也照做」。
		//   库未就绪 ⇒ 不注册路由（404），而不是挂一个假实现返回空列表。
		//
		// ★ ScopeGuard 用 isManagementFunc（D6：仅 T1–T2），**不用** isAdmin：
		//   isAdmin 描述的是平台治理者（管账号/管槽），而策略决策是业务判断。
		//   两者重合度低，合成一个会让「开管理台权限」意外授予改口径的能力。
		stratH := &api.StrategyHandlers{
			Svc:          dp.strategy,
			Auditor:      dp.strategy,
			ScopeGuard:   dp.isManagementFunc(),
			SnapshotHash: dp.snapshotHashFunc(),
		}
		stratH.Routes(mux)
	}

	// ── M-DR：备份下载 / 主备切换 / 回滚（G12）──
	//
	// ★ 为什么必须注册这些路由：`internal/dr` 把 G12 四条判定链路实现了，
	//   但若没有 HTTP 入口，`gate.CheckMonthlyQuota` / `CheckNoCredentialsInURL`
	//   / `CheckFailoverFencing` / `CheckRollbackKeepsAudit` 仍然只在测试里被调到
	//   = 仍然恒真。本层是那条「真实生产入口」。
	//
	// ★ 高风险操作（隔离 / 见证 / 切换 / 回滚）的门禁用 isT1Func —— 判定走
	//   **服务端**判据，接口层再把 Tier 写死为 "T1"，绝不透传客户端声明。
	drH := &api.DrHandlers{
		Downloads: dp.buildDownloadService(),
		Failover:  dp.buildFailoverController(),
		IsT1:      dp.isT1Func(),
	}
	drH.Routes(mux)

	// ── M-AUTH：上级代授（D13）──
	//
	// ★ 补的是「代授不溢出」这条断言的生产调用点：此前 `authz.Resolver.Delegate`
	//   全仓没有任何非测试调用点，于是「代授不溢出自身范围」在实现侧恒真。
	//   Sink 目前为 nil（尚无 grants 的持久化层）⇒ 响应如实回报 persisted=false，
	//   不假装权限已生效（登记 docs/06 F18）。
	delH := &api.DelegationHandlers{
		Resolver:     resolver,
		Entitlements: dp.ents,
	}
	delH.Routes(mux)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		v, err := kernel.Health(r.Context())
		if err != nil {
			http.Error(w, "kernel unhealthy: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{
			"ok":     true,
			"kernel": v,
			"db":     dp.dbReady, // 显式暴露数据面状态（降级时可观测）
			// ★ 降级时必须说明原因，否则「没配库」与「配了但坏了」
			//   在 healthz 上无法区分 —— 后者是部署事故，应被立刻发现。
			"dbDegradeReason": dp.dbDegradeReason,
			// ★ 规格定义文件（slots/algorithms/buckets/rules）的装载结果。
			//   这些目录是 docs/01 §0.3 的「单一事实源」，此前**从没被任何生产代码读过**；
			//   现在 sparkd 装载它们，问题不再静默 —— 列表为空即全部通过。
			"specLoaded": dp.rules != nil && dp.buckets != nil,
			"specIssues": dp.specIssues,
		})
	})
	// ── 前端静态托管（兜底路由，必须最后注册）──
	// 单进程交付整个平台：/api/* 与 /healthz 优先，其余交给前端产物 + SPA fallback。
	webDir := envOr("SPARK_WEB_DIR", "")
	static := staticHandler(webDir)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		static.ServeHTTP(w, r)
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

	log.Printf("sparkd listening on %s (compute kernel: %s, db: %v, web: %q)", *addr, *computeBin, dp.dbReady, webDir)
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