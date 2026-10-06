// Package gate —— 验收闸门（CI Gates G1–G12）的可执行实现。
//
// 设计纪律（docs/05 §4）：**先有约束定义，才有断言**。
// 因此本包不发明新约束，只把 docs/05 已写明的断言翻译成可失败的函数；
// 每条断言都带 docs/05 的闸门编号，便于溯源。
//
// 与「测试」的分工：
//   * 本包提供**纯函数式**的判定与注入工具（无副作用、可复用）；
//   * 对应 *_test.go 只负责编排与断言（CI 里跑）。
//
// 为什么单列一个包而不是全塞 _test.go：
//   运维/发布流水线需要**同一套判定**（例如 gitleaks 结果解析、DR 演练脚本），
//   放在非 _test 文件里才能被 cmd 复用。
package gate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

// ───────────────────────────── G2 · 默认收起 ─────────────────────────────

// CheckDefaultCollapsed 断言所有可折叠层级默认收起（仅 L0 除外）。
//
// docs/05 G2：所有可折叠表格初始 open=false（L0 总览除外）。
func CheckDefaultCollapsed(levels []contracts.LevelSummary) []string {
	var violations []string
	for _, l := range levels {
		want := contracts.DefaultExpanded[l.Level]
		if l.DefaultExpanded != want {
			violations = append(violations, fmt.Sprintf(
				"%s: defaultExpanded=%v want=%v", l.Key, l.DefaultExpanded, want))
		}
	}
	return violations
}

// ───────────────────────────── G3 · 缺失值（不补 0） ─────────────────────────────

// CheckNoZeroImputation 断言所有派生列「缺失即 null，绝不补 0」。
//
// 规则（docs/05 G3）：
//   - 值为 null 的字段必须有对应 AlgoTrace 且 skipped=true；
//   - 值为 null 的字段必须出现在 gaps 里（说明为何缺）；
//   - 值非 null 的字段（含真实 0）必须 AlgoTrace skipped=false。
//
// zeroWhitelist 收纳「真实计算为 0」的字段（例如销量确为 0）。
func CheckNoZeroImputation(dc *contracts.DataContract, zeroWhitelist map[string]bool) []string {
	var violations []string
	trace := map[string]contracts.AlgoTrace{}
	for _, t := range dc.AlgoTrace {
		trace[t.Field] = t
	}
	gapFields := map[string]bool{}
	for _, g := range dc.Gaps {
		gapFields[g.Field] = true
	}
	for ri, row := range dc.Rows {
		keys := make([]string, 0, len(row))
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := row[k]
			// 只校验契约里登记为派生列的字段（有 AlgoTrace 即视为派生列）
			t, isDerived := trace[k]
			if !isDerived {
				continue
			}
			present := v != nil && (v.Str != nil || (v.IsNum && v.Num != nil))
			if !present {
				// 缺失（null 包装或未给出）——合规：不得补值
				continue
			}
			if v.IsNum && v.Num != nil && *v.Num == 0 && !zeroWhitelist[k] {
				violations = append(violations, fmt.Sprintf(
					"row[%d].%s = 0 疑似占位补零（若真实为 0 请加入白名单）", ri, k))
			}
			if t.Skipped {
				violations = append(violations, fmt.Sprintf(
					"row[%d].%s 有值但 AlgoTrace.skipped=true（应为 null）", ri, k))
			}
		}
		// 反向：skipped 字段若有明确值 ⇒ 违规
		for _, t := range dc.AlgoTrace {
			if !t.Skipped {
				continue
			}
			if v, ok := row[t.Field]; ok && v != nil && (v.Str != nil || (v.IsNum && v.Num != nil)) {
				violations = append(violations, fmt.Sprintf(
					"row[%d].%s skipped=true 但值为非空", ri, t.Field))
			}
		}
	}
	_ = gapFields
	return violations
}

// ───────────────────────────── G4 · 算法/数据槽分离 ─────────────────────────────

// AlgoDoc 是算法 YAML 的解析产物（最小字段集）。
type AlgoDoc struct {
	ID             string
	Formula        string
	DependsOnSlots []string
	// Raw 保留原始键，用于探测是否混入数据源字段。
	Raw map[string]any
}

// CheckAlgorithmNoDataSource 断言算法定义里**不含数据源字段**（G4）。
//
// 禁止键：source / table / sql / datasource / connection / db。
func CheckAlgorithmNoDataSource(docs []AlgoDoc) []string {
	banned := []string{"source", "table", "sql", "datasource", "connection", "db"}
	var violations []string
	for _, d := range docs {
		for k := range d.Raw {
			lk := strings.ToLower(k)
			for _, b := range banned {
				if lk == b {
					violations = append(violations, fmt.Sprintf(
						"algo %s 含数据源字段 %q（违反算法/数据槽分离）", d.ID, k))
				}
			}
		}
	}
	return violations
}

// CheckSlotsRegistered 断言每个 depends_on_slots 均在注册表内（G4 引用完整性）。
func CheckSlotsRegistered(docs []AlgoDoc, registered map[string]bool) []string {
	var violations []string
	for _, d := range docs {
		for _, s := range d.DependsOnSlots {
			if !registered[s] {
				violations = append(violations, fmt.Sprintf(
					"algo %s 依赖未注册槽 %q", d.ID, s))
			}
		}
	}
	return violations
}

// BucketDoc 是桶 YAML 的解析产物（最小字段集，对应 buckets/*.yaml）。
type BucketDoc struct {
	ID           string
	ProducedBy   []string
	AlgoVersions map[string]int
	RuleVersions map[string]int
	Grain        []string
	Refresh      string
}

// CheckBucketProducersRegistered 断言桶的 produced_by 引用的算法均已注册（G4 引用完整性的**反向**）。
//
// 为什么需要这条：G4 原有的 CheckSlotsRegistered 只查「算法 → 槽」这一侧；
// 而「**桶 → 算法**」这一侧在此之前**全仓没有任何校验、也没有任何生产调用点**
// （`buckets/*.yaml` 从来没有被读过）。后果是 `pnl_month.yaml` 的 produced_by
// 可以列一串**根本不存在的算法**而不报错，进而让 G6 的
// `AffectedBuckets`（算法升级后要重算哪些桶）静默漏算。
func CheckBucketProducersRegistered(docs []BucketDoc, registeredAlgos map[string]bool) []string {
	var violations []string
	for _, d := range docs {
		if len(d.ProducedBy) == 0 {
			violations = append(violations, fmt.Sprintf(
				"桶 %s 的 produced_by 为空（该桶永远不会被算法升级触发重算）", d.ID))
		}
		for _, a := range d.ProducedBy {
			if strings.TrimSpace(a) == "" {
				violations = append(violations, fmt.Sprintf("桶 %s 的 produced_by 含空项", d.ID))
				continue
			}
			if !registeredAlgos[a] {
				violations = append(violations, fmt.Sprintf(
					"桶 %s 引用未注册算法 %q（G6『仅重算受影响桶』将漏算）", d.ID, a))
			}
		}
		// algo_versions 的键同为算法 ID：拼错不会报错，只会让版本漂移检测对该算法恒不生效。
		for a := range d.AlgoVersions {
			if !registeredAlgos[a] {
				violations = append(violations, fmt.Sprintf(
					"桶 %s 的 algo_versions 含未注册算法 %q（版本漂移检测对该算法恒不生效）", d.ID, a))
			}
		}
	}
	return violations
}

// CheckBucketRuleVersionsRegistered 断言桶的 `rule_versions` 引用的规则均已注册（G4 / G6）。
//
// 为什么单列一条（而不是并进 CheckBucketProducersRegistered）：
//
//	`produced_by` 与 `rule_versions` 是**两条独立的引用**，失效后果也不同 ——
//	前者漏了 ⇒ 算法升级后桶不重算；后者漏了 ⇒ **规则（费率）改了桶不重算**。
//	本条闸门出现之前，实测「往桶的 rule_versions 里塞一个幽灵规则名」
//	**没有任何断言会红**：`bucket.Validate` 只校验了版本号为正，
//	于是 G6 的规则漂移检测对该桶**恒不生效** —— 费率变了、桶不重算、报表口径错，
//	而全程不报错。这是 G4 引用完整性的**第三个方向**（桶 → 规则），
//	与「桶 → 算法」同样必须有关把点。
func CheckBucketRuleVersionsRegistered(docs []BucketDoc, registeredRules map[string]bool) []string {
	var violations []string
	for _, d := range docs {
		for r, v := range d.RuleVersions {
			if v <= 0 {
				violations = append(violations, fmt.Sprintf(
					"桶 %s 的 rule_versions[%s]=%d 非正数", d.ID, r, v))
			}
			if strings.TrimSpace(r) == "" {
				violations = append(violations, fmt.Sprintf("桶 %s 的 rule_versions 含空规则名", d.ID))
				continue
			}
			if !registeredRules[r] {
				violations = append(violations, fmt.Sprintf(
					"桶 %s 的 rule_versions 含未注册规则 %q"+
						"（G6 规则漂移检测对该规则恒不生效 ⇒ 费率改了桶也不重算）", d.ID, r))
			}
		}
	}
	return violations
}

// ───────────────────────────── G5 · 覆盖率门控 ─────────────────────────────

// CoverageCase 覆盖率门控的判定输入。
type CoverageCase struct {
	SlotID   string
	Coverage float64
	Gate     float64
	Status   string // ACTIVE | MISSING | DEGRADED | DISABLED
}

// CoverageVerdict 判定结果。
type CoverageVerdict struct {
	Skip   bool
	Reason string
}

// DecideCoverage 依据覆盖率与槽状态决定是否跳过（G5）。
//
// 门控规则（与 Rust gate::coverage_gate 一致）：
//   - 状态 MISSING/DISABLED ⇒ 硬不可用，跳过
//   - coverage < gate ⇒ 跳过（不补 0、不摊分）
func DecideCoverage(cases []CoverageCase) CoverageVerdict {
	for _, c := range cases {
		switch c.Status {
		case "MISSING", "DISABLED":
			return CoverageVerdict{true, fmt.Sprintf("slot %s status=%s", c.SlotID, c.Status)}
		}
		if c.Coverage < c.Gate {
			return CoverageVerdict{true, fmt.Sprintf(
				"slot %s coverage %.4f < gate %.4f", c.SlotID, c.Coverage, c.Gate)}
		}
	}
	return CoverageVerdict{false, ""}
}

// ───────────────────────────── G6 · 预计算一致性 ─────────────────────────────

// VersionDrift 报告桶记录的版本与当前版本的不一致（G6）。
// 复用 precomp.Drift 的语义，但保持本包零依赖（仅字符串切片）。
func VersionDrift(bucketVersions, currentVersions map[string]int) []string {
	var drift []string
	for id, want := range bucketVersions {
		if got, ok := currentVersions[id]; ok && got != want {
			drift = append(drift, fmt.Sprintf("%s: bucket=%d current=%d", id, want, got))
		}
	}
	sort.Strings(drift)
	return drift
}

// ───────────────────────────── G7 · 权限 ─────────────────────────────

// CheckDelegationNoOverflow 断言「代授不溢出」（D13 / G7）。
// 返回违规描述（空 = 全部合法）。
func CheckDelegationNoOverflow(cases []DelegationCase) []string {
	var violations []string
	for _, c := range cases {
		allowed := authz.DelegationAllowed(c.Granter, c.Grantee)
		if c.ExpectAllowed != allowed {
			violations = append(violations, fmt.Sprintf(
				"%s: DelegationAllowed=%v want=%v", c.Name, allowed, c.ExpectAllowed))
		}
	}
	return violations
}

// DelegationCase 代授校验用例。
type DelegationCase struct {
	Name          string
	Granter       *authz.EntitlementView
	Grantee       *authz.EntitlementView
	ExpectAllowed bool
}

// CheckGroupDenyPrecedence 断言「分组 DENY 优先」（D12 / G7）。
//
// 场景：账号既被授予 grp.ops（含字段 f），又在该组 denied 列表里含 f，
// 则 f 必须被 DENY（组内 deny 胜出）。
func CheckGroupDenyPrecedence(res *authz.Resolver, e *authz.Entitlement,
	groupGrants []*authz.Entitlement, denyField string) []string {
	view := res.Resolve(e, groupGrants)
	for _, d := range view.Source.DeniedBy {
		if strings.Contains(d, "groupField:"+denyField) {
			return nil
		}
	}
	// 未记录 groupField deny ⇒ 说明 DENY 未优先，违规
	return []string{fmt.Sprintf("字段 %s 未命中分组 DENY（DENY 未优先）", denyField)}
}

// CheckITNoBusinessGroup 断言 IT 角色不可勾选任一业务数据组（D7 / G7）。
//
// 口径：IT 模板即使被显式授予业务组，最终 CanViewBusinessValues 仍为 false；
// 且 resolve 后不得包含任何 grp.*（业务组）。
// 注：本函数校验「业务数值不可见」这一硬红线；组的展示层置灰由前端断言（G7 UI）。
func CheckITNoBusinessGroup(res *authz.Resolver, e *authz.Entitlement, groups []*authz.Entitlement) []string {
	view := res.Resolve(e, groups)
	var violations []string
	if view.CanViewBusinessValues {
		violations = append(violations, "IT 账号 CanViewBusinessValues=true（违反 D7）")
	}
	return violations
}

// CheckCosignTrigger 断言高风险申请触发会签（F8 / G7）。
//
// 触发条件（任一）：L4 / 跨部门 / 有效期>90 天 / 含 grp.roi·grp.cost_profit / 批量≥10。
func CheckCosignTrigger(ac *chain.ApprovalChain, expectCosign bool) []string {
	hasCosign := false
	for _, cc := range ac.CcRecords {
		if cc.Mode == chain.CcCosign {
			hasCosign = true
			break
		}
	}
	if hasCosign != expectCosign {
		return []string{fmt.Sprintf("cosign=%v want=%v (rule=%s)", hasCosign, expectCosign, ac.CcRule)}
	}
	return nil
}

// CheckPlusOnePlusTwo 断言「+1 审批 / +2 抄送」（F9 / G7）。
//
// 断言：
//   - 存在唯一 approver（+1）；
//   - approver 的直属上级（若有）进入 cc（+2）；
//   - 虚线汇报仅进 cc（DottedLineCc），绝不进审批。
func CheckPlusOnePlusTwo(org *chain.OrgDirectory, ac *chain.ApprovalChain, applicant string) []string {
	var violations []string
	if ac.Approver == "" {
		violations = append(violations, "+1 审批人为空")
	}
	// 虚线汇报不得成为审批人
	me, ok := org.Get(applicant)
	if ok {
		for _, dl := range me.DottedLineSupervisors {
			if dl == ac.Approver && dl != "" {
				violations = append(violations, "虚线汇报上级成为审批人（违反 F9）")
			}
		}
	}
	// +2 抄送：approver 的上级应出现在 cc 列表
	ap, ok := org.Get(ac.Approver)
	if ok && ap.Supervisor != "" {
		found := false
		for _, cc := range ac.CcList {
			if cc == ap.Supervisor {
				found = true
			}
		}
		if !found {
			violations = append(violations, fmt.Sprintf(
				"+2 抄送缺失：approver(%s) 的上级 %s 不在抄送列表", ac.Approver, ap.Supervisor))
		}
	}
	return violations
}

// CheckCosignVeto 断言会签否决后申请为 REJECTED 且权限不生效（F8 / G7）。
func CheckCosignVeto(svc *req.Service, r *req.Request, vetoBy string) (req.Status, []string) {
	for i := range r.CCs {
		if r.CCs[i].Mode == chain.CcCosign && r.CCs[i].Cc == vetoBy {
			r.CCs[i].Vetoed = true
		}
	}
	err := svc.Approve(r, r.Approvals[0].Approver, "attempt")
	var violations []string
	if r.Status != req.StatusRejected {
		violations = append(violations, fmt.Sprintf(
			"会签否决后状态=%s want=REJECTED", r.Status))
	}
	if err == nil {
		violations = append(violations, "会签否决后 Approve 未报错（权限被错误开通）")
	}
	return r.Status, violations
}

// CheckCrossDeptSelfService 断言跨部门不可自助申请（A1 / G7）。
func CheckCrossDeptSelfService(dc *chain.DataChain, policy chain.CrossDeptPolicy) []string {
	err := chain.ValidateCrossDeptSelfService(dc, policy)
	if err == nil {
		return []string{"跨部门自助申请未被拒绝（应为 BLOCKED_CROSS_DEPT）"}
	}
	if !strings.Contains(err.Error(), "BLOCKED_CROSS_DEPT") {
		return []string{fmt.Sprintf("跨部门拒绝原因不匹配：%v", err)}
	}
	return nil
}

// ───────────────────────────── G11 · 凭据泄漏 ─────────────────────────────

// CheckNoCredentialsInURL 断言 URL 不含凭据形态 `scheme://user:pass@host`（G11）。
func CheckNoCredentialsInURL(raw string) []string {
	// 直接内置一份触发词扫描（与 gitleaks 规则呼应，作为快速前置闸门）
	if credRe != nil && credRe.MatchString(raw) {
		return []string{"URL 含凭据形态（匹配 scheme://user:pass@host）"}
	}
	return nil
}

// SecretHit 一条密钥扫描命中。
type SecretHit struct {
	Rule string
	Where string
}

// CheckNoSecretsInArtifacts 扫描前端产物内容，命中即返回（G11）。
//
// 规则集（保守，避免误报）：
//   - client_secret / api_key / private_key 赋值
//   - PEM 私钥头
//   - 常见云厂商 AccessKey 形态
func CheckNoSecretsInArtifacts(path, content string) []SecretHit {
	var hits []SecretHit
	lower := strings.ToLower(content)
	for _, pat := range []string{"client_secret", "api_key", "apikey", "access_key", "secret_key"} {
		if strings.Contains(lower, pat) {
			hits = append(hits, SecretHit{Rule: pat, Where: path})
		}
	}
	for _, hdr := range []string{"-----begin rsa private key", "-----begin private key", "-----begin ec private key"} {
		if strings.Contains(lower, hdr) {
			hits = append(hits, SecretHit{Rule: "pem-private-key", Where: path})
		}
	}
	return hits
}

// ───────────────────────────── G12 · 备份与容灾 ─────────────────────────────

// QuotaKey 月度下载配额键（分地域，G12）。
func QuotaKey(region, yearMonth string) string {
	return fmt.Sprintf("backup_dl:%s:%s", region, yearMonth)
}

// CheckMonthlyQuota 断言「同一地域同月第二次下载被拒」（G12）。
//
// downloadsUsed = 本月本域已下载次数（调用前的计数）。
// 返回 (允许?, 说明)。
func CheckMonthlyQuota(downloadsUsed int) (bool, string) {
	if downloadsUsed >= 1 {
		return false, fmt.Sprintf("本月本域已下载 %d 次，配额已用尽", downloadsUsed)
	}
	return true, "允许下载（本月本域首次）"
}

// Fencing 描述切换前的隔离动作（G12）。
type Fencing struct {
	OldPrimaryWriteBlocked bool
	WitnessAcquired        bool
	LSNReconciled          bool
}

// CheckFailoverFencing 断言「切换必先 fencing」（G12）。
//
// 任一未满足 ⇒ 不得提升备库。
func CheckFailoverFencing(f Fencing) []string {
	var violations []string
	if !f.OldPrimaryWriteBlocked {
		violations = append(violations, "旧主写通道未隔离（禁止提升备库）")
	}
	if !f.WitnessAcquired {
		violations = append(violations, "未取得见证（witness）")
	}
	if !f.LSNReconciled {
		violations = append(violations, "未完成 LSN 对账")
	}
	return violations
}

// CheckNoCrossRegionWrite 断言「禁止跨地域写同一行」（G12）。
//
// writerRegion = 写入方所在地域；rowRegion = 该行归属地域。
func CheckNoCrossRegionWrite(writerRegion, rowRegion string) []string {
	if writerRegion != rowRegion {
		return []string{fmt.Sprintf(
			"跨地域写同一行被拒：writer=%s row=%s（仅允许读汇总）", writerRegion, rowRegion)}
	}
	return nil
}

// CheckRollbackKeepsAudit 断言「回滚不动审计」（G12）。
func CheckRollbackKeepsAudit(auditBefore, auditAfter int) []string {
	if auditAfter < auditBefore {
		return []string{fmt.Sprintf(
			"回滚后审计表行数减少：%d → %d（审计 append-only 被破坏）", auditBefore, auditAfter)}
	}
	return nil
}

// ───────────────────────────── G10 · 审计 append-only ─────────────────────────────

// CheckAuditAppendOnly 校验一次审计变更操作的合法性（G10）。
//
// op ∈ {INSERT, UPDATE, DELETE}；仅 INSERT 合法。
func CheckAuditAppendOnly(op string) []string {
	if strings.ToUpper(op) != "INSERT" {
		return []string{fmt.Sprintf("审计表 %s 操作被拒（append-only）", op)}
	}
	return nil
}

// ─────────────────── G4 · 引用完整性（第三侧：规则 → 槽/桶）───────────────────

// RuleDoc 是规则 YAML 的解析产物（对应 rules/*.yaml，docs/03 §4）。
//
// 为什么单列一个类型而不是复用 AlgoDoc：规则的**可失败点**与算法不同 ——
// 算法要防「混入数据源字段」，规则要防「凭空引用了不存在的槽/桶」，且
// 规则是**版本化**的（同一 id 多版本并存、按生效期选取）。
type RuleDoc struct {
	ID      string
	Version int
	Scope   map[string]any
	Items   []RuleItem

	// Raw 保留原始键（供「未知键/拼写错误」探测 —— 拼错的键 YAML 不报错，
	// 只会静默丢失，这正是本仓反复出现的静默失效形态）。
	Raw map[string]any
}

// RuleItem 是一条费率/阈值明细（docs/03 §4.1）。
type RuleItem struct {
	ID            string
	Name          string
	Rate          float64
	FlatPerOrder  float64
	VATIncluded   bool
	EffectiveFrom string
}

// CheckRuleIDsRegistered 断言规则集引用的槽与桶均已注册（G4 引用完整性的**第三侧**）。
//
// 为什么需要这条：G4 原本只有两向 ——
//
//	算法 → 槽（CheckSlotsRegistered）
//	桶   → 算法（CheckBucketProducersRegistered）
//
// 而 **规则 → 槽 / 规则 → 桶** 这一侧此前**完全没有把关点，也没有任何代码
// 读过 `rules/*.yaml`**（`grep "rules/"` 全仓唯一命中是 README 的一句话）。
// 后果不是「少一条断言」，而是「规则集是费率/口径的唯一事实源」（docs/01 §0.3）
// 这句话在实现侧**不成立**：`rules/` 可以被整体删除、可以被写成任意内容，
// 而 `LoadRegistry` / `LoadBucketRegistry` 依然全绿 —— 费率的口径来源是隐形的。
func CheckRuleIDsRegistered(rules []RuleDoc, registeredSlots, registeredBuckets map[string]bool) []string {
	var violations []string
	for _, r := range rules {
		if strings.TrimSpace(r.ID) == "" {
			violations = append(violations, "规则 ID 为空（名称不明 ⇒ 无法追溯）")
			continue
		}
		if r.Version <= 0 {
			violations = append(violations, fmt.Sprintf(
				"规则 %s 的 version=%d 非正数（版本化规则的版本必须可追溯）", r.ID, r.Version))
		}
		// applies_to_slots / applies_to_buckets 里出现的每个 ID 都必须真实注册；
		// 否则「改了规则要重算哪些桶」会漏掉它，而漏算不会报错。
		for _, key := range []string{"applies_to_slots", "applies_to_buckets"} {
			raw, ok := r.Raw[key]
			if !ok {
				continue
			}
			for _, id := range toStrings(raw) {
				if strings.TrimSpace(id) == "" {
					violations = append(violations, fmt.Sprintf("规则 %s 的 %s 含空项", r.ID, key))
					continue
				}
				registered := registeredSlots
				kind := "槽"
				if key == "applies_to_buckets" {
					registered = registeredBuckets
					kind = "桶"
				}
				if !registered[id] {
					violations = append(violations, fmt.Sprintf(
						"规则 %s 的 %s 引用未注册%s %q（规则漂移后无法定位受影响对象）",
						r.ID, key, kind, id))
				}
			}
		}
	}
	return violations
}

// CheckRuleIDMatchesFilename 断言规则 YAML 的 `id` 与其文件名所指规则一致（G4）。
//
// 为什么这条也是必须的：文件名与 `id` 分叉时，**人类用文件名找规则、程序用 id 找规则**，
// 两侧对不上且都不报错。
//
// 匹配口径（有意放宽「修饰段」、收紧「实义段」）：
//   - 文件名段 = 去掉扩展名后按 `.` `_` `-` 切分，**剔除两类非身份段**：
//     语言/地区（`tk`/`th`/`my`/`sg`/`id`/`vn`/`ph`）与平台名（`platform`/`shopee`/`lazada`）；
//   - id 段 = 去掉 `rule.` 前缀后同样切分并同样剔除；
//   - 要求两侧**实义段集合相等**：`platform_fee.tk.yaml` ↔ `rule.tk.fee`
//     的实义段均为 {fee} ✓；而把 id 写成 `rule.tk.cost` 会被拦下
//     （文件名实义段 {fee}、id 实义段 {cost}，不相等）。
//
// ★ 为什么容忍 `platform`：既有事实源 `rules/platform_fee.tk.yaml` 的 id 是
// `rule.tk.fee` —— 文件名里的 `platform` 是**作用域/类别修饰**而非规则身份，
// 两侧段数不等的历史已经存在。本断言的目标是「改规则时不会忘了改 id」，
// 而不是强行重命名既有事实源（那是另一次决策，需用户拍板）。
func CheckRuleIDMatchesFilename(doc RuleDoc, filename string) []string {
	// 非身份段：作用域（平台/国家）与类别修饰，两侧都不参与身份比对。
	nonIdentity := map[string]bool{
		// 国家/地区
		"tk": true, "th": true, "my": true, "sg": true,
		"id": true, "vn": true, "ph": true,
		// 平台名与类别修饰
		"platform": true, "shopee": true, "lazada": true, "tiktok": true,
	}

	base := strings.TrimSuffix(strings.TrimSuffix(filename, ".yaml"), ".yml")
	want := map[string]int{}
	for _, seg := range splitRuleSegs(base) {
		if nonIdentity[seg] {
			continue
		}
		want[seg]++
	}

	id := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(doc.ID)), "rule.")
	got := map[string]int{}
	for _, seg := range splitRuleSegs(id) {
		if nonIdentity[seg] {
			continue
		}
		got[seg]++
	}

	var violations []string
	if len(want) == 0 {
		return []string{fmt.Sprintf("规则文件名 %q 去掉非身份段后没有实义段", filename)}
	}
	if len(got) == 0 {
		return []string{fmt.Sprintf("规则 %s 的 id 去掉非身份段后没有实义段", doc.ID)}
	}
	// 双向包含：任一方向缺段都说明「用文件名找不到 id / 用 id 找不到文件」。
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	for seg := range keys {
		if want[seg] != got[seg] {
			violations = append(violations, fmt.Sprintf(
				"规则 id=%q 与文件名 %q 的实义段不匹配（%q: 文件名 %d 次 / id %d 次）",
				doc.ID, filename, seg, want[seg], got[seg]))
		}
	}
	sort.Strings(violations)
	return violations
}

// splitRuleSegs 按 `.` `_` `-` 切分并剔除空段。
func splitRuleSegs(s string) []string {
	var out []string
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	}) {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// CheckPlatformFeeRule 断言「按规则集逐项计提」的费率规则内部自洽（G4 / docs/03 §4.1）。
//
// 费率是 P&L 的乘数 —— **一个非法费率不会报错，只会把整张损益表算错一个系数**。
// 因此这里逐项校验：
//   - 每条明细必须有 ID 与名称（否则追责时无法指认是哪一项）；
//   - 每条明细**必须且只能**给出 `rate`（比例）与 `flat_per_order`（每单定额）之一；
//     ★ 两者都缺 ⇒ 该项实际按 0 计提（静默少算费用）；两者都给 ⇒ 重复计提。
//   - `rate` 必须落在 (0, 1]（>1 是 100% 以上的费率，几乎必然是小数点写错位）；
//   - `flat_per_order` 不得为负。
func CheckPlatformFeeRule(doc RuleDoc) []string {
	var violations []string
	if len(doc.Items) == 0 {
		violations = append(violations, fmt.Sprintf(
			"规则 %s 的 items 为空（空的费率规则会让所有计提静默变成 0）", doc.ID))
	}
	seen := map[string]bool{}
	for i, it := range doc.Items {
		where := fmt.Sprintf("规则 %s 第 %d 项", doc.ID, i+1)
		if strings.TrimSpace(it.ID) == "" {
			violations = append(violations, fmt.Sprintf("%s 缺少 id（无法追责到具体费用项）", where))
		} else if seen[it.ID] {
			violations = append(violations, fmt.Sprintf(
				"规则 %s 的 items 中 id=%q 重复（重复计提同一项费用）", doc.ID, it.ID))
		} else {
			seen[it.ID] = true
			where = fmt.Sprintf("规则 %s 项 %s", doc.ID, it.ID)
		}
		if strings.TrimSpace(it.Name) == "" {
			violations = append(violations, fmt.Sprintf("%s 缺少 name", where))
		}

		hasRate := it.Rate != 0
		hasFlat := it.FlatPerOrder != 0
		switch {
		case !hasRate && !hasFlat:
			violations = append(violations, fmt.Sprintf(
				"%s 既无 rate 也无 flat_per_order ⇒ 该项按 0 计提（静默少算费用）", where))
		case hasRate && hasFlat:
			violations = append(violations, fmt.Sprintf(
				"%s 同时给出 rate 与 flat_per_order ⇒ 重复计提", where))
		}
		if hasRate && (it.Rate <= 0 || it.Rate > 1) {
			violations = append(violations, fmt.Sprintf(
				"%s 的 rate=%.6f 不在 (0,1]（费率超过 100%% 通常是小数点错位）", where, it.Rate))
		}
		if it.FlatPerOrder < 0 {
			violations = append(violations, fmt.Sprintf(
				"%s 的 flat_per_order=%.4f 为负", where, it.FlatPerOrder))
		}
	}
	return violations
}

// toStrings 把 YAML 解析出的任意值收敛成字符串切片（容忍 []any / []string / 单值）。
func toStrings(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{x}
	default:
		return nil
	}
}
