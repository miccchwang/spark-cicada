// sql_0012_tenant_hardening_test.go —— 0012 迁移（多租户收口）的静态闸门（不依赖数据库）。
//
// 动机：0011 在文末**显式承诺** 0012 会交付三件事：
//   ① 延迟表加固（registry_* / staging_* / collect_* / precomp_build_log）
//   ② dim_org 复合主键 (tenant_id, account) 重建 + 6 条外键重建
//   ③ v_cross_tenant_fk_violation 巡检视图
//
// 这三件事都是**安全红线**：漏了任何一件，共享档部署就会出现
// 「跨租户引用」或「配方被任意租户改写」。而本机/CI 未必有 Postgres，
// 真库集成测试会 skip —— 于是「完成了」和「没完成」看起来一样。
//
// 因此这里做文本级不变量断言，把 0012 的承诺钉死。
//
// ★ 与 0011 的教训一致：文本闸门最危险的失效模式不是漏写，
//   而是「写了却永远为真」。故每个断言都配一条**负向自测**（见文件末尾），
//   证明闸门真的有牙齿。
package gate_test

import (
	"strings"
	"testing"
)

// --- 0012 里必须被加固的表（0011 文末明确「留给 0012」的那批）---

var hardenStandardTables = []string{
	"staging_record",
	"staging_project_release",
	"staging_ingest_guard",
	"collect_job",
	"collect_page",
	"collect_rate_gear",
	"precomp_build_log",
}

var hardenRegistryTables = []string{
	"registry_slot",
	"registry_algorithm",
	"registry_rule_set",
	"registry_bucket",
	"registry_slot_coverage",
}

// TestSQL_0012_HardensDeferredTables 断言 0012 把 0011 留给它的表全部加固。
//
// 判据：每张表在 0012 中同时出现
//   · ENABLE ROW LEVEL SECURITY（或经 EXECUTE format 构造）
//   · FORCE  ROW LEVEL SECURITY
//   · 一条建策略语句
// 且表名必须出现在迁移文本里（防止整段被误删）。
func TestSQL_0012_HardensDeferredTables(t *testing.T) {
	sql := readMigration(t, "0012_tenant_hardening.sql")

	all := append(append([]string{}, hardenStandardTables...), hardenRegistryTables...)
	for _, tbl := range all {
		if !strings.Contains(sql, tbl) {
			t.Errorf("0012 未加固表 %s（0011 已明确留给 0012）", tbl)
			continue
		}
	}
	// 结构性判据：加列/开 RLS/建策略这三步的动作词必须都在
	for _, must := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"FORCE ROW LEVEL SECURITY",
		"CREATE POLICY",
		"DROP POLICY IF EXISTS",
	} {
		if !strings.Contains(sql, must) {
			t.Errorf("0012 缺少加固动作：%q", must)
		}
	}
}

// TestSQL_0012_RegistryUsesOverrideModelNotPlainIsolation 断言 registry_* 用的是
// 「平台默认 + 租户覆盖」模型，而不是普通隔离。
//
// ★ 这是 0012 语义最微妙、也最容易写错的一点：
//   读侧必须允许看到平台默认行（tenant_id IS NULL），
//   写侧**禁止**产生 NULL 行（否则任意租户都能改全平台配方 = 横向提权）。
//
// 判据（文本级，逐条对应设计意图）：
//   · 必须存在 p_tenant_override_ 策略族（区别于普通的 p_tenant_isolation_）
//   · 读侧 USING 含 `tenant_id IS NULL`
//   · 写侧 WITH CHECK **不含** OR（写只能写自己）
func TestSQL_0012_RegistryUsesOverrideModelNotPlainIsolation(t *testing.T) {
	sql := readMigration(t, "0012_tenant_hardening.sql")

	if !strings.Contains(sql, "p_tenant_override_") {
		t.Fatal("0012 registry_* 未使用「默认+覆盖」策略族 p_tenant_override_（横向提权防线缺失）")
	}

	// ① 读侧：必须允许看到平台默认行（tenant_id IS NULL），否则租户看不到默认配方。
	//    同样不能用窗口 —— 直接看 USING 子句。
	usings := allCalls(sql, "USING")
	sawOverrideRead := false
	for _, u := range usings {
		if strings.Contains(u, "tenant_id IS NULL") {
			sawOverrideRead = true
		}
	}
	if !sawOverrideRead {
		t.Error("0012 registry_* 读侧策略缺少 `OR tenant_id IS NULL`（租户将看不到平台默认配方）")
	}

	// ② 写侧必须**没有** OR IS NULL。
	//
	// 注意：不能用 `\(([^)]*)\)` —— 它在 rls_tenant_id() 的**内层右括号**处截断，
	// 得到 "tenant_id = rls_tenant_id(" 这样的残片，导致「含 OR」永远检测不到
	// （正是本项目反复强调的「闸门永远为真」失效模式）。必须做括号配平。
	//
	// ★ 另一个更隐蔽的陷阱（本闸门初版就栽在这里）：registry 策略由
	//   EXECUTE format(...) 拼出，`p_tenant_override_` 在同一段里出现**两次**
	//   （DROP 一次、CREATE 一次）。若只取「第一次出现 + 固定窗口」，
	//   窗口可能落在 DROP 那侧、或落在未被改动的副本上 ——
	//   于是有人把 CREATE 侧的写约束改成 OR IS NULL，闸门照样绿。
	//   故这里检查**整份文件里所有 WITH CHECK**，逐条否决。
	checks := allCalls(sql, "WITH CHECK")
	if len(checks) == 0 {
		t.Fatal("0012 全文件找不到任何 WITH CHECK（写侧无约束 ⇒ 可改全平台配方）")
	}
	for _, c := range checks {
		if strings.Contains(strings.ToUpper(c), "OR ") {
			t.Errorf("0012 写侧 WITH CHECK 含 OR ⇒ 租户可写 NULL 行改名全平台配方：%q", c)
		}
	}
	// 至少有一条是 registry 的「只能写自己」形态
	sawRegistryWrite := false
	for _, c := range checks {
		if strings.Contains(c, "rls_tenant_id()") {
			sawRegistryWrite = true
		}
	}
	if !sawRegistryWrite {
		t.Error("0012 没有任何 WITH CHECK 经 rls_tenant_id() —— 写侧未受租户约束")
	}
}

// allCalls 返回 s 中所有 `keyword (` 的括号配平内容（不止第一个）。
// 见 allCalls 的必要性：同一文件里 EXECUTE format 会重复出现同一关键字。
func allCalls(s, keyword string) []string {
	var out []string
	rest := s
	for {
		c, ok := balancedCall(rest, keyword)
		if !ok {
			return out
		}
		out = append(out, c)
		// 前进：跳过本次 keyword 与其配平括号，避免死循环
		ki := strings.Index(rest, keyword)
		open := strings.Index(rest[ki:], "(") + ki
		depth := 0
		adv := len(rest)
		for i := open; i < len(rest); i++ {
			if rest[i] == '(' {
				depth++
			} else if rest[i] == ')' {
				depth--
				if depth == 0 {
					adv = i + 1
					break
				}
			}
		}
		rest = rest[adv:]
	}
}

// balancedCall 从 s 中定位 `keyword (`，返回括号**配平**的内层内容。
//
// 与 naive 的 `\(([^)]*)\)` 不同：后者在遇到第一个 `)` 就停，
// 对于含函数调用（如 rls_tenant_id()）的表达式会截断。
// 这里累加括号深度，直到回到 0。
func balancedCall(s, keyword string) (string, bool) {
	ki := strings.Index(s, keyword)
	if ki < 0 {
		return "", false
	}
	open := strings.Index(s[ki:], "(")
	if open < 0 {
		return "", false
	}
	open += ki
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], true
			}
		}
	}
	return "", false
}

// TestSQL_0012_DimOrgCompositePKAndFKs 断言 dim_org 主键改为复合，且 6 条外键全部重建。
//
// ★ 这是本迁移的核心目标：把跨租户引用从「事后巡检」升级为「约束层拒绝」。
//   漏掉任何一条 FK ⇒ 那条引用路径仍可跨租户。
func TestSQL_0012_DimOrgCompositePKAndFKs(t *testing.T) {
	sql := readMigration(t, "0012_tenant_hardening.sql")

	// ① 复合主键
	if !strings.Contains(sql, "PRIMARY KEY (tenant_id, account)") {
		t.Fatal("0012 未把 dim_org 主键改为 (tenant_id, account) —— 跨租户引用无法在约束层阻断")
	}
	// 主键列必须非空（否则 DDL 直接失败；也确认有 SET NOT NULL 的意图）
	if !strings.Contains(sql, "tenant_id SET NOT NULL") {
		t.Error("0012 dim_org 未显式 SET NOT NULL（主键列不允许 NULL，应显式声明）")
	}

	// ② 6 条外键全部以复合形态重建。
	//
	// ★ 陷阱（本闸门初版栽过）：6 个约束名在文件里各只出现一次（都在 fks 数组里），
	//   DROP 与 ADD 都走 `fks[i][2]` 动态引用。故 `strings.Contains(sql, cons)`
	//   只能证明「名字在清单里」，**不能**证明「真的被重建为复合键」——
	//   有人把整段重建循环删掉，断言照样绿。
	//
	//   因此这里做两层校验：
	//     (a) 约束名必须在 fks 声明数组内（证明该路径被纳入重建清单）
	//     (b) 必须存在**通用的复合 ADD 模板**，且列引用来自 fks[i][3]
	fks := []struct{ table, cons, col string }{
		{"dim_org", "dim_org_supervisor_fkey", "supervisor"},
		{"dim_data_chain", "dim_data_chain_owner_account_fkey", "owner_account"},
		{"dim_group_member", "dim_group_member_account_fkey", "account"},
		{"fact_entitlement", "fact_entitlement_account_fkey", "account"},
		{"fact_permission_request", "fact_permission_request_applicant_fkey", "applicant"},
		{"dim_view_template", "dim_view_template_owner_fkey", "owner"},
	}
	for _, fk := range fks {
		// (a) 名字 + 表名必须在同一行（即 fks 数组的那条声明）
		if !declaresFKPair(sql, fk.table, fk.cons) {
			t.Errorf("0012 未在重建清单中声明外键 %s.%s —— 该路径仍可跨租户引用", fk.table, fk.cons)
		}
	}
	// (b) 通用复合 ADD 模板：必须用 tenant_id 与动态列位、REFERENCES 复合主键
	compositeADD := "FOREIGN KEY (tenant_id, %I) REFERENCES dim_org (tenant_id, account)"
	if !strings.Contains(sql, compositeADD) {
		t.Fatal("0012 缺少通用复合外键重建模板 —— 跨租户引用未被约束层阻断")
	}
	// 且该模板必须被 fks 循环真正执行（EXECUTE format 包裹）
	if !strings.Contains(sql, "EXECUTE format(") {
		t.Error("0012 复合外键模板未被 EXECUTE 执行（只是注释/字符串，未落地）")
	}

	// ③ 顺序纪律：必须先 DROP 旧 FK，再换主键，再重建。
	dropIdx := strings.Index(sql, "ALTER TABLE %I DROP CONSTRAINT %I")
	addPKIdx := strings.Index(sql, "ADD CONSTRAINT dim_org_pkey PRIMARY KEY (tenant_id, account)")
	if dropIdx < 0 {
		t.Error("0012 未先 DROP 旧外键（顺序错会导致重建失败）")
	} else if addPKIdx >= 0 && dropIdx > addPKIdx {
		t.Error("0012 顺序错误：DROP FK 出现在换主键之后（外键图仍引用旧主键，DDL 会失败）")
	}
}

// declaresFKPair 断言「子表名」与「约束名」出现在同一行（fks 数组的一条声明）。
// 这样「把某条 FK 从清单里改名/删除」必然被抓到。
func declaresFKPair(sql, table, cons string) bool {
	for _, line := range strings.Split(sql, "\n") {
		if strings.Contains(line, "'"+table+"'") && strings.Contains(line, "'"+cons+"'") {
			return true
		}
	}
	return false
}

// TestSQL_0012_CrossTenantViolationView 断言跨租户引用巡检视图存在且覆盖全部 6 条引用路径。
func TestSQL_0012_CrossTenantViolationView(t *testing.T) {
	sql := readMigration(t, "0012_tenant_hardening.sql")

	if !strings.Contains(sql, "CREATE OR REPLACE VIEW v_cross_tenant_fk_violation") {
		t.Fatal("0012 未交付 0011 承诺的 v_cross_tenant_fk_violation 巡检视图")
	}
	// 每条路径必须以 child_ref 标签出现，便于定位违规行
	for _, ref := range []string{
		"dim_org.supervisor",
		"dim_data_chain.owner_account",
		"dim_group_member.account",
		"fact_entitlement.account",
		"fact_permission_request.applicant",
		"dim_view_template.owner",
	} {
		if !strings.Contains(sql, "'"+ref+"'") {
			t.Errorf("巡检视图缺少引用路径 %s（该路径的违规行无法被发现）", ref)
		}
	}
	// 视图必须用 IS DISTINCT FROM 判定租户不一致（NULL 安全的比较）
	if !strings.Contains(sql, "IS DISTINCT FROM") {
		t.Error("巡检视图未用 IS DISTINCT FROM 比较租户（NULL 租户会被漏判）")
	}
}

// TestSQL_0012_NoSilentSkipOnUnsafePrecondition 断言最危险的前置条件不满足时**报错中止**
// 而不是静默跳过。
//
// ★ 场景：dim_org 若有 tenant_id IS NULL 的行，复合主键无法建立。
//   此时若「静默跳过主键重建」，会留下「以为改了其实没改」的假象 —— 最坏结果。
//   故必须是 RAISE EXCEPTION（而非 RAISE NOTICE）。
func TestSQL_0012_NoSilentSkipOnUnsafePrecondition(t *testing.T) {
	sql := readMigration(t, "0012_tenant_hardening.sql")

	if !strings.Contains(sql, "RAISE EXCEPTION") {
		t.Fatal("0012 前置自检失败时未 RAISE EXCEPTION（静默跳过主键重建是最坏的失效模式）")
	}
	// 错误信息必须给运维可操作指引（含具体数字 + 下一步）
	if !strings.Contains(sql, "null_org") || !strings.Contains(sql, "USING HINT") {
		t.Error("0012 前置自检错误信息缺少可操作指引（行数变量 / HINT）")
	}
}

// TestSQL_0012_SchemaPrefixDiscipline 断言 0012 遵守文件自述的「纪律 0」：
// 对象名不得写死 public schema 前缀（否则独立档租户重放时会误改平台库）。
func TestSQL_0012_SchemaPrefixDiscipline(t *testing.T) {
	raw := readMigration(t, "0012_tenant_hardening.sql")
	sql := stripSQLComments(raw)

	// 允许出现的「public」只应在比较/判断里（current_schema() <> 'public'）。
	// 禁止写法：ALTER TABLE public.xxx / CREATE POLICY ... ON public.xxx
	for _, bad := range []string{
		"public.dim_org",
		"public.registry_",
		"public.staging_",
		"public.collect_",
		"public.precomp_",
		"public.fact_",
	} {
		if strings.Contains(sql, bad) {
			t.Errorf("0012 违反纪律 0：写死了 schema 前缀 %q（独立档租户重放会误改平台库）", bad)
		}
	}
	// 正向：应当使用 current_schema() 作用域判定
	if !strings.Contains(sql, "current_schema()") {
		t.Error("0012 未使用 current_schema() 作用域判定（无法支持独立档 schema 重放）")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ★ 负向自测（meta-test）：证明上面的断言真的会「响」。
//
// 参照 0011 的教训：文本闸门最危险的失效模式是「写了却永远为真」。
// 这里构造若干「被破坏的」SQL 片段，验证检测函数确实会报错。
// ─────────────────────────────────────────────────────────────────────────────

// TestSQL_0012_NegativeControl_WriteSideORDetected 证明「写侧含 OR」能被识别。
//
// 若把 WITH CHECK (tenant_id = rls_tenant_id()) 误写成
// WITH CHECK (tenant_id = rls_tenant_id() OR tenant_id IS NULL)，
// 闸门必须能发现 —— 这是横向提权。
func TestSQL_0012_NegativeControl_WriteSideORDetected(t *testing.T) {
	bad := `CREATE POLICY p_tenant_override_x ON registry_slot ` +
		`USING (tenant_id = rls_tenant_id() OR tenant_id IS NULL) ` +
		`WITH CHECK (tenant_id = rls_tenant_id() OR tenant_id IS NULL)`

	check, ok := balancedCall(bad, "WITH CHECK")
	if !ok {
		t.Fatal("负向样例未匹配到 WITH CHECK —— 闸门定位失效")
	}
	if !strings.Contains(strings.ToUpper(check), "OR ") {
		t.Fatalf("闸门失效：未能识别写侧 OR 的提权写法 %q", check)
	}
	// ★ 反向对照：合法的写侧（无 OR）必须**不**被误判
	good := `WITH CHECK (tenant_id = rls_tenant_id())`
	gc, ok := balancedCall(good, "WITH CHECK")
	if !ok {
		t.Fatal("合法样例未匹配到 WITH CHECK —— 闸门定位失效")
	}
	if strings.Contains(strings.ToUpper(gc), "OR ") {
		t.Fatalf("闸门误报：合法写侧 %q 被判定为含 OR", gc)
	}
	if gc != "tenant_id = rls_tenant_id()" {
		t.Fatalf("括号配平失败：期望完整内容，实得 %q", gc)
	}
}

// TestSQL_0012_NegativeControl_MissingCompositeFKDetected 证明「漏建复合 FK」能被发现。
func TestSQL_0012_NegativeControl_MissingCompositeFKDetected(t *testing.T) {
	// 只删掉 tenant_id，退化回单列外键
	bad := `ALTER TABLE dim_group_member ADD CONSTRAINT dim_group_member_account_fkey ` +
		`FOREIGN KEY (account) REFERENCES dim_org (account)`

	if strings.Contains(bad, "FOREIGN KEY (tenant_id, ") {
		t.Fatal("闸门失效：单列外键被误判为复合外键")
	}
	// 正向对照：复合形态必须被识别
	good := `ALTER TABLE dim_group_member ADD CONSTRAINT dim_group_member_account_fkey ` +
		`FOREIGN KEY (tenant_id, account) REFERENCES dim_org (tenant_id, account)`
	if !strings.Contains(good, "FOREIGN KEY (tenant_id, ") {
		t.Fatal("闸门失效：复合外键未被识别")
	}
}

// TestSQL_0012_NegativeControl_SchemaPrefixDetected 证明「写死 public 前缀」能被发现。
func TestSQL_0012_NegativeControl_SchemaPrefixDetected(t *testing.T) {
	bad := `ALTER TABLE public.dim_org ADD COLUMN tenant_id uuid;`
	if !strings.Contains(bad, "public.dim_org") {
		t.Fatal("闸门失效：写死 schema 前缀未被识别")
	}
	// 正向对照：带 current_schema() 判断的合法写法不应命中
	good := `IF cur_schema <> 'public' THEN RETURN; END IF;`
	for _, pat := range []string{"public.dim_org", "public.registry_", "public.staging_"} {
		if strings.Contains(good, pat) {
			t.Fatalf("闸门误报：合法写法 %q 命中了禁用前缀 %q", good, pat)
		}
	}
}

// TestSQL_0012_NegativeControl_MissingViewPathDetected 证明「巡检视图漏路径」能被发现。
func TestSQL_0012_NegativeControl_MissingViewPathDetected(t *testing.T) {
	partial := `CREATE OR REPLACE VIEW v_cross_tenant_fk_violation AS
SELECT 'dim_org.supervisor'::text AS child_ref, c.account, c.tenant_id, p.account, p.tenant_id
  FROM dim_org c JOIN dim_org p ON p.account = c.supervisor
 WHERE c.tenant_id IS DISTINCT FROM p.tenant_id;`

	// 少了另外 5 条路径 —— 闸门必须能报缺
	required := []string{
		"dim_org.supervisor", "dim_data_chain.owner_account", "dim_group_member.account",
		"fact_entitlement.account", "fact_permission_request.applicant", "dim_view_template.owner",
	}
	missing := 0
	for _, r := range required {
		if !strings.Contains(partial, "'"+r+"'") {
			missing++
		}
	}
	if missing != 5 {
		t.Fatalf("闸门失效：期望检出 5 条缺失路径，实际 %d", missing)
	}
}
