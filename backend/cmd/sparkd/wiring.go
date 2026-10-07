// 装配辅助 —— 把内置的组织/模板/授权数据接到服务上。
//
// 生产环境这些数据来自 DB（dim_org / fact_entitlement / dim_group）；
// 未配置 DB 时回退到**内置最小夹具**，让 sparkd 仍可启动被探活。
//
// ★ 纪律：DB 缺席时**绝不**伪造业务数据 —— 查询路径会 fail-closed 报错，
//
//	而不是返回编造的 0 或空行。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/admin"
	"github.com/miccchwang/spark-cicada/backend/internal/api"
	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/db"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/groupstore"
	"github.com/miccchwang/spark-cicada/backend/internal/rule"
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
	"github.com/miccchwang/spark-cicada/backend/internal/store"
	"github.com/miccchwang/spark-cicada/backend/internal/templatestore"
	"github.com/miccchwang/spark-cicada/backend/internal/tenant"
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
	// tenants：租户注册表（dim_tenant 只读）。
	//
	// ★ 库未就绪时为 nil ⇒ 租户解析一律拒绝（fail-closed），
	//   退回单租户行为。绝不用"注册表不可用所以放行"。
	tenants *tenant.Registry
	// caliberAudit：M-PNL 口径切换的审计落库（append-only）。
	// nil 表示库未就绪 —— 此时口径仍可切换，但接口会如实回报「未留痕」。
	caliberAudit *store.CaliberAuditStore
	// strategy：M-STRATEGY 策略实验室的持久化入口（选型卡 + 决策历史）。
	//
	// ★ 与 caliberAudit 不同，本项**不做降级**：策略变更会写规则集、
	//   会让预计算桶过期，是有副作用的状态改动，不能「无库也照做」。
	//   nil ⇒ M-STRATEGY 路由不注册（404），而不是挂一个假实现。
	strategy *store.StrategyStore
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

	// rules 规则集注册表（M-RULE，G4 第三侧的真实生产链路）。
	//
	// ★ 必要性：`rules/*.yaml` 是 docs/01 §0.3 明写的「费率/口径的唯一事实源」，
	// 但在本字段出现之前**全仓没有任何代码读过它**（grep "rules/" 唯一命中是 README）。
	// 于是「唯一事实源」在实现侧不成立：该目录可被删空/写错而无人报错，
	// 且桶的 rule_versions（G6 规则漂移检测的输入）没有来源 ⇒ 规则改了桶永不重算。
	rules *rule.Registry
	// buckets 预计算桶注册表（G4 反向 + G6 映射来源）。
	buckets *slot.BucketRegistry
	// specIssues 记录「仓库定义文件」层面的问题（不阻断启动，但进 healthz 暴露）。
	// 与 dbDegradeReason 同源纪律：静默失败必须变成可观测的失败。
	specIssues []string
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

	// ── 规格注册表（无需数据库：读仓库里的定义文件）──
	//
	// ★ 这一步是「判定函数没有生产调用点」这个病的**第十个变种**的解药：
	//   `rules/*.yaml` 此前全仓没有任何代码读过，`slot.LoadRegistry` /
	//   `slot.LoadBucketRegistry` 也只有测试调用。现在 sparkd 启动时**真的装载**它们，
	//   于是 G2/G3/G4/G5 与 G4 第三侧（规则 → 槽/桶）第一次有了**进程内的生产调用点**。
	//
	// 失败不阻断启动（与 DB 降级同纪律），但**进 healthz**（specIssues），
	// 避免「静默降级伪装成部署正常」。
	specDir := envOr("SPARK_SPEC_DIR", "..")
	p.loadSpecs(specDir)

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
	// ★ 租户注册表（Task #57）。短 TTL 缓存：见 tenant.NewRegistryWithCache 注释。
	p.tenants = tenant.NewRegistryWithCache(pool, tenantRegistryCacheTTL)
	// 口径切换审计复用同一连接池（audit_log 是 append-only，只 INSERT）
	p.caliberAudit = store.NewCaliberAuditStore(pool)
	// M-STRATEGY：选型卡读 registry_rule_set，历史读/写 audit_log（同一池）
	p.strategy = store.NewStrategyStore(pool)
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

// loadSpecs 装载仓库里的**规格定义文件**（slots / algorithms / buckets / rules）。
//
// ★ 为什么 sparkd 必须做这件事（不必等数据库）：
//
//	docs/01 §0.3 规定「算法在 algorithms/、数据槽在 slots/、规则在 rules/」是
//	**单一事实源**。但在本方法出现之前，这些目录**全仓没有任何生产代码读过**
//	（`grep "buckets/"` 唯一命中是 store/admin.go 的一条 INSERT 列名，
//	 `grep "rules/"` 唯一命中是 README 的一句话）——
//	于是「单一事实源」在实现侧不成立：删掉某个目录、改错某个 ID，
//	服务照样启动、接口照样返回，而 G2/G3/G4/G5/G6 的判定函数**从没有真实调用点**
//	（这是本仓反复出现的「假闸门」形态，此处是第十个变种）。
//
// 纪律（与 dbDegradeReason 同源）：**失败不阻断启动，但绝不静默** ——
// 问题进 `specIssues` 并由 `/healthz` 暴露，避免「静默降级伪装成部署正常」。
func (p *dataPlane) loadSpecs(specDir string) {
	slotsDir := filepath.Join(specDir, "slots")
	algosDir := filepath.Join(specDir, "algorithms")
	bucketsDir := filepath.Join(specDir, "buckets")
	rulesDir := filepath.Join(specDir, "rules")

	reg, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		p.specIssues = append(p.specIssues, "data-slot registry: "+err.Error())
		log.Printf("[warn] 数据槽/算法注册表加载失败（%s）：%v", slotsDir, err)
		return
	}
	log.Printf("[spec] 数据槽 %d 个、算法 %d 个（G4 分离与引用完整性已过闸门）",
		len(reg.Slots()), len(reg.Algorithms()))

	buckets, err := slot.LoadBucketRegistry(bucketsDir, reg)
	if err != nil {
		p.specIssues = append(p.specIssues, "bucket registry: "+err.Error())
		log.Printf("[warn] 桶注册表加载失败（%s）：%v", bucketsDir, err)
		return
	}
	p.buckets = buckets
	log.Printf("[spec] 预计算桶 %d 个（produced_by 引用完整性已过闸门）", len(buckets.Buckets()))

	rules, err := rule.LoadRuleRegistry(rulesDir, reg.RegisteredSlotIDs(), buckets.RegisteredBucketIDs())
	if err != nil {
		p.specIssues = append(p.specIssues, "rule registry: "+err.Error())
		log.Printf("[warn] 规则注册表加载失败（%s）：%v", rulesDir, err)
		return
	}
	p.rules = rules
	log.Printf("[spec] 规则 %d 条（费率/口径唯一事实源已真读并过闸门）", len(rules.Rules()))

	// ★ 回填校验：桶的 `rule_versions` 引用的规则必须真实注册（G4 第三方向）。
	//
	// 为什么必须回填而不是并进上面的 LoadBucketRegistry：
	// 装载顺序是「桶在规则之前」（规则要按桶 ID 校验 applies_to_buckets），
	// 因此首次装载时 rules 尚不存在。若不回填，`rule_versions` 就永远只被
	// 「版本号为正」这种弱断言覆盖 —— 往里面塞幽灵规则名不会让任何东西变红，
	// 而后果是「费率改了桶不重算」全程静默。
	if err := buckets.ValidateWithRules(reg, rules.RegisteredRuleIDs()); err != nil {
		p.specIssues = append(p.specIssues, "bucket rule_versions: "+err.Error())
		log.Printf("[warn] 桶的 rule_versions 引用校验失败：%v", err)
		return
	}
	log.Printf("[spec] 桶的 rule_versions 引用完整性已过闸门")

	// ★ 规则 ⇒ 桶 的映射必须有内容：否则「规则升版本后要重算哪些桶」（G6）
	//   会永远返回空，桶永远不重算 —— 不报错，只是报表口径慢慢失真。
	if len(rules.ApplyToBuckets()) == 0 {
		p.specIssues = append(p.specIssues,
			"rule→bucket mapping is empty: G6 rule drift will never mark any bucket stale")
	}

	// ★ 槽的「数据来源」必须可解析（G4 第六侧 / docs/03 §2.3）。
	//
	// 为什么必须**在桶到位之后**复核：`source_kind`/`source_ref` 此前是
	// 「被读进来、写进 registry_slot、然后被彻底遗忘」的一对字段（全仓唯一校验是
	// `SourceKind == ""`）—— 于是 `source_kind: 随便写` 与 `api` 等价、
	// `source_ref` 删掉也照过；而 `source_kind` 决定采集层去哪取数、
	// 以及写进 DB 后能否满足 0005 迁移的 CHECK 约束。
	//
	// 其中 derived 槽的 `source_ref` 指向**桶**（物化来源，docs/03 §3.2），
	// 故必须等 `buckets` 加载完成才能核对 —— 否则 `source_ref: 幽灵桶` 永远无人发现
	// （同 `pnl_month.yaml` 引 4 个不存在算法那类缺陷）。
	if err := reg.ValidateWithBuckets(buckets); err != nil {
		p.specIssues = append(p.specIssues, "slot source: "+err.Error())
		log.Printf("[warn] 槽的数据来源校验失败：%v", err)
		return
	}
	log.Printf("[spec] 槽的数据来源全部可解析（G4 第六侧：kind 已知 + ref 非占位/非幽灵）")

	// ★ 槽的「采集时效」必须逐条可解析（G4 / docs/03 §2.3）。
	//
	// 为什么在**进程内**再核一次（注册表加载时已核过）：装载注册表一旦失败会
	// `return`，后面四个注册表都不再装载；而这里是一次**独立的、只读的**复核，
	// 确保「时效声明可解析」这一条在 sparkd 的 healthz 里**始终**有据可查，
	// 不依赖前面那条 return 路径的成败。
	//
	// ★ 同时这也是 `freshness` 判活（slot.CheckFreshness）的**前置条件**：
	//   时效不可解析 ⇒ 判活只能 fail-closed 把所有槽判过期。
	//   与其让运行期悄悄全量跳过，不如启动时就把问题摆到 healthz 上。
	if bad := gate.CheckFreshnessDeclared(reg.FreshnessDocs()); len(bad) > 0 {
		for _, b := range bad {
			p.specIssues = append(p.specIssues, "slot freshness: "+b)
		}
		log.Printf("[warn] 槽的 freshness 声明有问题（%d 条），已进 healthz", len(bad))
	} else {
		log.Printf("[spec] 槽的采集时效声明全部可解析（freshness 判活已就绪）")
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
//
//	M-GROUP 的组管理、M-TEMPLATE 的模板管理都要用它；
//	若两处各写一份，迟早出现「组管理认他是管理员、模板管理不认」的错位，
//	而这类错位在权限系统里就是权限漏洞。
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

// ───────────────────────────── 租户解析（Task #57）─────────────────────────────

// tenantRegistryCacheTTL 是租户元数据缓存的存活时间。
//
// ★ 取值权衡：这是「每请求都要解析一次租户」与「停用一家租户多久生效」之间的取舍。
//
//	30s 意味着：一个停用操作最多滞后 30s 生效。
//	对停用（anti-fraud / 欠费）来说，30s 是可接受的；对一个**必须要即时**的
//	动作（例如强制下线），应当改走 Invalidate() 主动失效或缩短 TTL。
//
//	绝不用"永久缓存"：那样停用只会在进程重启时生效，等于停用功能失效。
const tenantRegistryCacheTTL = 30 * time.Second

// tenantResolver 返回本进程的租户解析器（库未就绪时为 nil）。
func (p *dataPlane) tenantResolver() *tenant.Resolver {
	if p.tenants == nil {
		return nil
	}
	return p.tenants.ResolverFor()
}

// resolveTenantFromRequest 从一次请求解析租户上下文。
//
// ★ 返回 (Resolution, error)：
//   - error 非 nil ⇒ **基础设施故障**（注册表不可达），上层应回 503；
//   - Resolution.Allowed()==false ⇒ 租户层面拒绝，上层按原因映射 4xx；
//   - 否则 Resolution.Context() 给出租户上下文，调用方据此取绑定租户的 store。
//
// ★ 头来源：X-Spark-Tenant 与 X-Spark-Account 同层（请求身份）。
//
//	这里**只**认 header/claim，不认 host（DisallowHost=true，见 Resolver 注释）。
//
// ★ 单租户部署（tenants==nil）返回 Rejected(ReasonInternal) 且 error 为 nil ——
//
//	调用方必须自行区分「部署是单租户」与「解析失败」。注意不要把它当成
//	"解析失败所以回 503"：单租户部署下**所有**请求都会走到这里。
func (p *dataPlane) resolveTenantFromRequest(ctx context.Context, h tenant.Hint) (tenant.Resolution, error) {
	if p.tenants == nil {
		// 注册表未就绪（无库 / 单租户部署）：显式拒绝，绝不回退默认租户。
		return tenant.Rejected(tenant.ReasonInternal), nil
	}
	return p.tenants.ContextFor(ctx, h)
}

// templatesForTenant 返回**绑定到指定租户**的模板服务（供 api.TemplateHandlers.ForTenant）。
//
// ★ 关键不变量：返回的 store 持有固定 tc，**不随请求变**。
//
//	若这里返回的是共享实例并在 handler 里改它的租户，并发请求会串租户。
//	因此本方法每次调用都构造一个新实例（成本极低：只包一个结构体指针 + 池引用）。
//
// ★ 租户记录必须有效才绑定；无效 ⇒ 返回 nil，由 api 层回 503。
//
//	这里复用了注册表（带 TTL 缓存），因此热路径上不产生额外查询。
func (p *dataPlane) templatesForTenant(tenantID string) api.TemplateService {
	if p.templates == nil || p.tenants == nil {
		return nil
	}
	// 解析租户（用同一条注册表，保证档位/状态判据与请求解析完全一致）。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := p.tenants.ContextFor(ctx, tenant.Hint{Header: tenantID})
	if err != nil || !res.Allowed() {
		return nil
	}
	tc, err := res.Context()
	if err != nil {
		return nil
	}
	return templatestore.NewForTenant(p.pool, tc)
}

// baseTemplateOf 取某账号的基座模板 id。
//
// ★ 从**授权来源**取（authoritative），不从求值后的视图取 ——
//
//	视图里不保留 BaseTemplate 字段（它只有展开后的模块/维度）。
//	数据面就绪时查库，否则回退内置夹具。
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
//
//	数据面就绪时按 dim_org.supervisor 反查（存在直接下属即为主管）。
//
// ★★ 多租户：必须带 tenantID —— 且**不能**只靠 RLS。
//
//	本函数用的是特权 pool（p.pool），它**绕过 RLS**（超级用户/BYPASSRLS 角色
//	不受行级安全约束）。而 0012 把 dim_org 主键改成 (tenant_id, account) 之后，
//	同一个 account 可以同时存在于多家租户。
//	若查询只写 `WHERE supervisor = $1` 而不带 tenant_id：
//	  · 单租户部署「碰巧正确」；
//	  · 一旦同库承载多家租户，A 租户的账号会命中 B 租户的同名下属行
//	    ⇒ 判定结果错乱（提权：不该给团队档的人拿到了团队档）。
//	⇒ 因此这里**显式**用 tenant_id 过滤，把正确性建立在查询本身，
//
//	而不是「赌 RLS 会拦住」——特权通道根本不受 RLS 约束。
//
// ★ 库未就绪时**保守返回 false**：
//
//	「不知道谁是主管」绝不能退化成「所有人都是主管」——
//	那会让任何用户都能创建团队档模板（对全部门可见），属于提权。
func (p *dataPlane) isSupervisorFunc() func(tenantID, account string) bool {
	return func(tenantID, account string) bool {
		if account == "" || !p.dbReady || p.pool == nil {
			return false
		}
		// ★ 租户必须是合法 uuid；缺失即保守拒绝（fail-closed）。
		if !tenant.IsUUID(tenantID) {
			return false
		}
		var n int
		err := p.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM dim_org WHERE tenant_id = $1 AND supervisor = $2 LIMIT 1`,
			tenantID, account).Scan(&n)
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
//
//	后者会误把 `tpl.item` 也当成 IT —— 这是历史上已修过一次的同类 bug
//	（见 internal/req/req.go 的 isIT 注释）。
func isITBase(tpl string) bool {
	return tpl == "tpl.it" || strings.HasPrefix(tpl, "tpl.it.")
}

// ───────────────────────────── 管理层判定（D6） ─────────────────────────────

// isManagementFunc 返回「某账号是否属于管理层（T1–T2）」的判定函数。
//
// ★ 用途：M-STRATEGY 策略实验室的范围守卫（D6：仅管理层）。
//
//	策略实验室能改全公司的费率口径、触发全量重算 —— 它不是一个
//	「用户偏好」，而是一次经营决策。因此范围必须收得很紧。
//
// ★ 为什么不用 isAdminFunc：
//
//	isAdmin 的判据是「IT 基座 或 启用 m.admin 模块」，那描述的是
//	**平台治理者**（管账号、管槽）。而策略决策是**业务判断**，
//	应当由承担经营结果的人做。两者重合度低：IT 能管平台但不应决策
//	费率口径；业务负责人能决策但不能管账号。
//	合成一个函数会让「给某人开管理台权限」意外地授予他改全公司口径的能力。
//
// ★ 判据（按优先级）：
//  1. 组织链路里 tier ∈ {T1, T2}（数据面就绪时查 dim_org）。
//  2. 显式启用 m.strategy 模块（为将来放权留的显式开关）。
//
// ★★ 多租户：与 isSupervisorFunc 同理，查询**必须**带 tenant_id ——
// 本函数同样走特权 pool（绕过 RLS），而 0012 之后 dim_org 的账号不再全局唯一。
// 不带 tenant_id 会让「A 租户的管理层账号」命中「B 租户的同名 T1/T2 行」，
// 从而拿到改全公司费率口径的权限 —— 这是最严重的一类越权。
//
// ★ 库未就绪时**保守返回 false**：见下。
func (p *dataPlane) isManagementFunc() func(tenantID, account string) bool {
	return func(tenantID, account string) bool {
		if account == "" {
			return false
		}
		// ① 组织层级 T1/T2
		if p.dbReady && p.pool != nil && tenant.IsUUID(tenantID) {
			var tier *string
			err := p.pool.QueryRow(context.Background(),
				`SELECT tier FROM dim_org WHERE tenant_id = $1 AND account = $2`,
				tenantID, account).Scan(&tier)
			if err == nil && tier != nil {
				if *tier == "T1" || *tier == "T2" {
					return true
				}
			}
			// 查库失败或查不到：继续走 ② 的模块判定，不在此处放行
		}
		// ② 显式启用 m.strategy 模块
		if p.resolver != nil && p.ents != nil {
			if view := resolveFor(p.resolver, p.ents, account); view != nil {
				for _, m := range view.Modules {
					if m == "m.strategy" {
						return true
					}
				}
			}
		}
		return false
	}
}

// ───────────────────────────── 决策依据快照 ─────────────────────────────

// snapshotHashFunc 返回「当前数据快照哈希」的取值函数。
//
// ★ 用途：策略决策要落一条「决策依据」（docs/01 §7.5）。
//
//	日后有人问「为什么 3 月的净利率变了」，能顺着这个 hash 找到
//	当时依据的那份数据 —— 而不是只能回答「有人改过参数」。
//	KODP 的教训正是「口径变更不追溯，导致无法解释为什么变了」。
//
// ★ 哈希的构成：各预计算桶的「状态 + 算法版本 + 规则集版本」拼起来的
//
//	确定性指纹。选这个组合而不是「数据的哈希」，是因为：
//	① 桶的版本直接决定「报表会显示什么」，这才是决策真正依据的东西；
//	② 全量数据的哈希每次采数都会变（哪怕业务没变），
//	   那样哈希就失去了「可比对」的作用。
//
// ★ 取不到时返回空串而不是报错：快照哈希是**辅助证据**，
//
//	不应因为它拿不到就阻止一次决策（那会变成「因为观测不了所以不能决策」）。
//	接口层会如实告知「未取到快照」，但不失败。
func (p *dataPlane) snapshotHashFunc() func(context.Context) string {
	return func(ctx context.Context) string {
		if !p.dbReady || p.pool == nil {
			return ""
		}
		// 按桶 id 排序取，保证同样的库状态永远得到同样的哈希
		rows, err := p.pool.Query(ctx, `
			SELECT id, state, algo_versions::text, rule_versions::text
			  FROM registry_bucket
			 ORDER BY id`)
		if err != nil {
			return ""
		}
		defer rows.Close()

		h := sha256.New()
		for rows.Next() {
			var id, state, algo, rule string
			if err := rows.Scan(&id, &state, &algo, &rule); err != nil {
				return ""
			}
			// 分隔符用不可见字符，避免字段内容拼接产生歧义
			fmt.Fprintf(h, "%s\x1f%s\x1f%s\x1f%s\x1e", id, state, algo, rule)
		}
		if err := rows.Err(); err != nil {
			return ""
		}
		return "sha256:" + hex.EncodeToString(h.Sum(nil))
	}
}
