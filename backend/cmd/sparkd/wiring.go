// 装配辅助 —— 把内置的组织/模板/授权数据接到服务上。
//
// 生产环境这些数据来自 DB（dim_org / fact_entitlement / dim_group）；
// 未配置 DB 时回退到**内置最小夹具**，让 sparkd 仍可启动被探活。
//
// ★ 纪律：DB 缺席时**绝不**伪造业务数据 —— 查询路径会 fail-closed 报错，
//   而不是返回编造的 0 或空行。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/admin"
	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/db"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/groupstore"
	"github.com/miccchwang/spark-cicada/backend/internal/store"
	"github.com/miccchwang/spark-cicada/backend/internal/templatestore"
)

// dataPlane 打包运行时数据面依赖（可为部分降级）。
type dataPlane struct {
	pool     *pgxpool.Pool
	store    *store.Postgres
	admin    *admin.Plane
	registry *gate.Registry
	org      *chain.OrgDirectory
	resolver *authz.Resolver
	ents     func(string) (*authz.Entitlement, []*authz.Entitlement)
	// groups / templates：M-GROUP/M-REQ 与 M-TEMPLATE 的持久化入口。
	// nil 表示库未就绪（对应接口按 503 fail-closed，不返回编造数据）。
	groups    *groupstore.Store
	templates *templatestore.Store
	// dbReady 表示真库已就绪（迁移已应用、注册表已加载）。
	dbReady bool
	// dbDegradeReason 在 dbReady=false 时说明**为什么**降级。
	//
	// ★ 必要性（真实事故）：原先只暴露 `db:false`，无法区分两种截然不同的状态：
	//   ① 压根没配 DSN（正常降级，符合预期）
	//   ② 配了 DSN，但迁移目录路径写错/连不上库（**部署事故**）
	//   两者 healthz 长得一模一样。事故现场是一个路径
	//   `../sql/migrations` 相对 cwd 解析失败 —— sparkd 只打了一行 [warn]
	//   就「成功地」以降级模式跑起来，看起来一切正常。
	//   「静默降级」比「启动失败」危险：它把一个坏的部署伪装成好的部署。
	dbDegradeReason string
}

// buildDataPlane 尝试连接 DB；失败时**降级**到内置夹具（不阻断启动）。
//
// 降级语义：dbReady=false ⇒ 查询接口返回明确错误（fail-closed），
// /api/me 与 /healthz 仍可用（便于排障）。
func buildDataPlane(ctx context.Context) *dataPlane {
	p := &dataPlane{
		org:      buildOrgDirectory(),
		resolver: authz.NewResolver(builtinTemplates()),
		ents:     buildEntitlementSource(),
		// 模块注册表：初始可用能力（后续可由控制面动态调整）
		registry: gate.NewRegistry([]string{
			"cap.core", "slot.revenue", "slot.cogs", "slot.platform_fee", "slot.inventory_snap",
		}),
	}

	dsn, err := db.DSNFromEnv()
	if err != nil {
		p.dbDegradeReason = "未配置数据库连接串（" + err.Error() + "）"
		log.Printf("[warn] 未配置数据库（%v）—— 数据面降级：/api/query 将 fail-closed", err)
		return p
	}

	m, err := db.NewMigrator(ctx, dsn)
	if err != nil {
		p.dbDegradeReason = "已配置 DSN 但数据库连接失败：" + err.Error()
		log.Printf("[warn] 数据库连接失败（%s）：%v —— 降级运行",
			db.RedactDSN(dsn), err)
		return p
	}
	pool := m.Pool()
	p.pool = pool

	// ── 应用迁移（幂等）──
	migDir := envOr("SPARK_MIGRATIONS_DIR", "../sql/migrations")
	migs, err := db.LoadMigrations(migDir)
	if err != nil {
		// ★ 这是最隐蔽的一种：DSN 配好了、库也连上了，仅仅因为迁移目录
		//   是相对路径且 cwd 不对，就整体降级。必须把路径与 cwd 一起报出来。
		cwd, _ := os.Getwd()
		p.dbDegradeReason = fmt.Sprintf(
			"已配置 DSN 且已连上库，但加载迁移目录失败：dir=%q cwd=%q err=%v"+
				"（提示：该目录按**相对 cwd** 解析，请设 SPARK_MIGRATIONS_DIR 为绝对路径）",
			migDir, cwd, err)
		log.Printf("[warn] 加载迁移失败（%s）：%v —— 降级运行", migDir, err)
		return p
	}
	res, err := m.Up(ctx, migs)
	if err != nil {
		p.dbDegradeReason = "已配置 DSN 且已连上库，但应用迁移失败：" + err.Error()
		log.Printf("[warn] 应用迁移失败：%v —— 降级运行", err)
		return p
	}
	if len(res.Applied) > 0 {
		log.Printf("[db] 本次应用迁移 %d 个：%v", len(res.Applied), res.Applied)
	}
	log.Printf("[db] 数据库就绪（%s）", db.RedactDSN(dsn))

	// ── 构造数据面组件 ──
	p.store = store.New(pool)
	adminStore := store.NewAdmin(pool)
	p.admin = admin.New(adminStore, adminStore.AuditSink())
	p.admin.Registry = p.registry
	// M-GROUP/M-REQ 与 M-TEMPLATE 的持久化（与 store 同池）
	p.groups = groupstore.New(pool)
	p.templates = templatestore.New(pool)
	p.dbReady = true
	return p
}

// startupGateChecks 真库的启动自检（G6 漂移 + 注册表一致性）。
//
// 只有真库就绪时才执行；返回错误 ⇒ 拒绝启动（fail-closed）。
func (p *dataPlane) startupGateChecks(ctx context.Context) error {
	if !p.dbReady {
		return nil // 降级模式不阻断启动
	}
	// 桶版本 vs 注册表算法版本（G6）
	meta, err := p.store.BucketState(ctx, "pnl_month")
	if err != nil {
		return fmt.Errorf("读桶状态失败：%w", err)
	}
	if meta.State == "UNREGISTERED" {
		// 未注册的桶是**配置缺陷**，但不应阻止进程启动（可能正在初始化）；
		// 查询侧已 fail-closed，此处仅告警。
		log.Printf("[warn] 桶 pnl_month 未注册 —— 查询将被拒绝（请检查迁移 0004）")
		return nil
	}
	// 从注册表读当前算法版本，与桶内记录比对
	current, err := p.currentAlgoVersions(ctx, "pnl_month")
	if err != nil {
		return err
	}
	drift := gate.VersionDrift(meta.AlgoVersions, current)
	if len(drift) > 0 {
		log.Printf("[warn] 桶版本漂移（G6）：%v —— 该桶应重算后使用（查询侧会拒绝 STALE 桶）", drift)
	}
	if meta.State != "FRESH" {
		log.Printf("[warn] 桶 pnl_month 状态=%s（非 FRESH）—— 查询将被 fail-closed 拒绝", meta.State)
	}
	return nil
}

// currentAlgoVersions 汇总注册表中产出该桶的算法版本。
func (p *dataPlane) currentAlgoVersions(ctx context.Context, bucket string) (map[string]int, error) {
	if p.admin == nil {
		return map[string]int{}, nil
	}
	ov, err := p.admin.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("读注册表失败：%w", err)
	}
	out := map[string]int{}
	for _, a := range ov.Algorithms {
		if a.WritesBucket == bucket {
			out[a.ID] = a.Version
		}
	}
	return out, nil
}

// buildOrgDirectory 构造演示组织架构（F9=A 单主属）。
func buildOrgDirectory() *chain.OrgDirectory {
	d := chain.NewOrgDirectory()
	put := func(n *chain.OrgNode) { d.Put(n) }

	put(&chain.OrgNode{Account: "ceo", Tier: "T1", PrimaryDept: "HQ", CanApprove: true, Active: true})
	put(&chain.OrgNode{Account: "vp.sea", Supervisor: "ceo", Tier: "T2", PrimaryDept: "SEA",
		CanApprove: true, Active: true})
	put(&chain.OrgNode{Account: "vp.us", Supervisor: "ceo", Tier: "T2", PrimaryDept: "US",
		CanApprove: true, Active: true})
	put(&chain.OrgNode{Account: "lead.sea", Supervisor: "vp.sea", Tier: "T3", PrimaryDept: "SEA",
		CanApprove: true, Active: true})
	put(&chain.OrgNode{Account: "ops.sea", Supervisor: "lead.sea", Tier: "T4", PrimaryDept: "SEA",
		CanApprove: true, Active: true})
	// IT 账号：虚线汇报到 ceo（仅抄送，不入审批）
	put(&chain.OrgNode{Account: "it.ops", Supervisor: "lead.sea", Tier: "T4", PrimaryDept: "SEA",
		DottedLineSupervisors: []string{"ceo"}, CanApprove: false, Active: true})
	return d
}

// builtinTemplates 内置账号模板（DB 缺席时的兜底）。
//
// 注意：模板**不含** canViewBusinessValues 的置真逻辑 —— D7 由 Resolver 兜底。
func builtinTemplates() map[string]*authz.Entitlement {
	allBusinessGroups := []authz.GroupScopeGrant{
		{Group: string(authz.GrpOps), MaxLevel: authz.L4},
		{Group: string(authz.GrpCostProfit), MaxLevel: authz.L4},
		{Group: string(authz.GrpInventory), MaxLevel: authz.L4},
		{Group: string(authz.GrpROI), MaxLevel: authz.L4},
	}
	return map[string]*authz.Entitlement{
		// 业务负责人：可见业务数值
		"tpl.lead": {
			Account:               "tpl.lead",
			Modules:               authz.ModuleGrant{Enabled: []string{"m.ops", "m.cost", "m.inventory", "m.roi"}},
			MaxLevel:              authz.L4,
			Dimensions:            []authz.DimensionGrant{{Dim: "brand"}},
			DataUseGroups:         allBusinessGroups,
			CanViewBusinessValues: true,
		},
		// IT：只给运维模块，**不给**业务数值（D7）
		"tpl.it": {
			Account:    "tpl.it",
			Modules:    authz.ModuleGrant{Enabled: []string{"m.ops"}},
			MaxLevel:   authz.L2,
			Dimensions: []authz.DimensionGrant{{Dim: "brand"}},
			// 不设 CanViewBusinessValues
		},
	}
}

// buildEntitlementSource 返回账号 → 授权 的查询函数（DB 缺席时的兜底）。
func buildEntitlementSource() func(string) (*authz.Entitlement, []*authz.Entitlement) {
	accounts := map[string]string{
		"ceo":      "tpl.lead",
		"vp.sea":   "tpl.lead",
		"vp.us":    "tpl.lead",
		"lead.sea": "tpl.lead",
		"ops.sea":  "tpl.lead",
		"it.ops":   "tpl.it",
	}
	return func(account string) (*authz.Entitlement, []*authz.Entitlement) {
		tpl, ok := accounts[account]
		if !ok {
			return nil, nil
		}
		return &authz.Entitlement{
			Account:      account,
			BaseTemplate: tpl,
		}, nil // 分组授权（groupGrants）由 DB 提供；此处为空
	}
}

// envOr 读取环境变量（带默认值）。
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ───────────────────────────── 管理员 / 主管判定 ─────────────────────────────

// isAdminFunc 返回「某账号是否具备全局管理权限」的判定函数。
//
// ★ 判定依据刻意做成**可注入的一处**（而不是散落在各 handler）：
//   M-GROUP 的组管理、M-TEMPLATE 的模板管理都要用它；
//   若两处各写一份，迟早出现「组管理认他是管理员、模板管理不认」的错位，
//   而这类错位在权限系统里就是权限漏洞。
//
// 依据（按优先级）：
//  1. 模板基座为 IT（tpl.it*）—— IT 承担平台治理，但**看不到业务数值**（D7）。
//  2. 授权里显式启用了 m.admin 模块。
//
// 数据面就绪时走真库（fact_entitlement），否则回退到内置夹具。
func (p *dataPlane) isAdminFunc() func(string) bool {
	return func(account string) bool {
		if account == "" {
			return false
		}
		// ① 基座为 IT ⇒ 平台治理者（D7：仍看不到业务数值）
		if p.baseTemplateOf(account) != "" && isITBase(p.baseTemplateOf(account)) {
			return true
		}
		// ② 授权里显式启用 m.admin 模块
		if p.dbReady && p.resolver != nil && p.ents != nil {
			view := resolveFor(p.resolver, p.ents, account)
			if view != nil {
				for _, m := range view.Modules {
					if m == "m.admin" {
						return true
					}
				}
			}
		}
		return false
	}
}

// baseTemplateOf 取某账号的基座模板 id。
//
// ★ 从**授权来源**取（authoritative），不从求值后的视图取 ——
//   视图里不保留 BaseTemplate 字段（它只有展开后的模块/维度）。
//   数据面就绪时查库，否则回退内置夹具。
func (p *dataPlane) baseTemplateOf(account string) string {
	if account == "" {
		return ""
	}
	if p.dbReady && p.pool != nil {
		var tpl *string
		err := p.pool.QueryRow(context.Background(),
			`SELECT base_template FROM fact_entitlement WHERE account = $1`, account).Scan(&tpl)
		if err == nil && tpl != nil {
			return *tpl
		}
		return ""
	}
	ent, _ := buildEntitlementSource()(account)
	if ent == nil {
		return ""
	}
	return ent.BaseTemplate
}

// isSupervisorFunc 返回「某账号是否主管及以上」的判定函数。
//
// ★ 「主管及以上」= 组织链路（D9）里**有人向他汇报**。
//   数据面就绪时按 dim_org.supervisor 反查（存在直接下属即为主管）。
//
// ★ 库未就绪时**保守返回 false**：
//   「不知道谁是主管」绝不能退化成「所有人都是主管」——
//   那会让任何用户都能创建团队档模板（对全部门可见），属于提权。
func (p *dataPlane) isSupervisorFunc() func(string) bool {
	return func(account string) bool {
		if account == "" || !p.dbReady || p.pool == nil {
			return false
		}
		var n int
		err := p.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM dim_org WHERE supervisor = $1 LIMIT 1`, account).Scan(&n)
		if err != nil {
			// 查询失败也保守拒绝（fail-closed）
			return false
		}
		return n > 0
	}
}

// isITBase 判断模板基座是否属于 IT（tpl.it 或 tpl.it.*）。
//
// ★ 用前缀 + 边界判断而非 `strings.HasPrefix(tpl, "tpl.it")`：
//   后者会误把 `tpl.item` 也当成 IT —— 这是历史上已修过一次的同类 bug
//   （见 internal/req/req.go 的 isIT 注释）。
func isITBase(tpl string) bool {
	return tpl == "tpl.it" || strings.HasPrefix(tpl, "tpl.it.")
}
