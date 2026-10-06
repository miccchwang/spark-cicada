// integration_test.go —— M-GROUP / M-REQ 真库端到端测试。
//
// 运行方式：
//
//	SPARK_TEST_DB_DSN='postgres://user:pass@host:5432/spark_test?sslmode=disable' \
//	  go test ./internal/groupstore/ -run Integration -v
//
// ★ 跳过 vs 失败的纪律（与 internal/store 一致）：
//   - 本地未设 DSN ⇒ **跳过**（t.Skip），不阻塞日常开发。
//   - CI 设 SPARK_REQUIRE_DB=1 ⇒ 未设 DSN 时**直接失败**，绝不静默跳过。
//
// 为什么必须这样：若 CI 也静默 skip，这些真库断言就**从未被执行过**，
// 而 CI 依然全绿 —— 0006 的约束、DENY 的往返、
// 「未表态 vs 已同意」的语义位持久化，全成了「没人验证过却显示通过」的假绿。
//
// ★ 本文件还承担一个特殊职责：它是「表结构来自 0001」这一结论的**活证据**。
//   初版 groupstore 用的是自创表名（dim_user_group 等），单测全绿、
//   一跑真库立刻 SQLSTATE 42703 —— 因为真库里根本没有那些表。
//   下面 TestIntegration_SchemaIsFromMigration0001 把这个事实钉死。
package groupstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/db"
	"github.com/miccchwang/spark-cicada/backend/internal/group"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

func requireDB() bool {
	v := strings.TrimSpace(os.Getenv("SPARK_REQUIRE_DB"))
	return v == "1" || strings.EqualFold(v, "true")
}

// openTestDB 连接测试库、应用全部迁移，返回 Store。
func openTestDB(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		if requireDB() {
			t.Fatal("★ 真库集成测试被要求必须运行（SPARK_REQUIRE_DB=1），但未设置 SPARK_TEST_DB_DSN。" +
				"闸门不允许静默跳过 —— 请起 Postgres 并注入 DSN，否则移除 SPARK_REQUIRE_DB。")
		}
		t.Skip("未配置 SPARK_TEST_DB_DSN —— 跳过真库集成测试（本地开发；CI 会设置）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 迁移目录：backend/internal/groupstore → 仓库根/sql/migrations
	dir := filepath.Join("..", "..", "..", "sql", "migrations")
	migs, err := db.LoadMigrations(dir)
	if err != nil {
		t.Fatalf("加载迁移失败：%v", err)
	}
	m, err := db.NewMigrator(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败（%s）：%v", db.RedactDSN(dsn), err)
	}
	if _, err := m.Up(ctx, migs); err != nil {
		m.Close()
		t.Fatalf("应用迁移失败：%v", err)
	}
	return New(m.Pool()), func() { m.Close() }
}

// TestIntegration_SchemaIsFromMigration0001 断言本包用的表**确实来自 0001/0006**。
//
// ★ 这是从一次真实事故里长出来的断言。
//   初版 groupstore 自创了 dim_user_group / fact_group_grant /
//   dim_account_entitlement / fact_request_approval / fact_request_cc 五张表，
//   而迁移里根本没有它们 —— 真库一跑就是 SQLSTATE 42703。
//   更糟的是：这类错误**只在真库暴露**，纯逻辑单测永远是绿的。
//
//   所以这里正面断言「该存在的表存在」，并**反向断言自创的表不存在**，
//   让「重新引入一套平行表」这个错误在 CI 里立刻变红。
func TestIntegration_SchemaIsFromMigration0001(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	// 必须存在（0001 建立 + 0006 增量补强）
	mustExist := []string{
		"dim_group", "dim_group_member", "fact_entitlement",
		"fact_permission_request",
		"fact_group_grant_change", // 0006 新增
	}
	for _, tbl := range mustExist {
		var n int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			 WHERE table_schema='public' AND table_name=$1`, tbl).Scan(&n); err != nil {
			t.Fatalf("查询表 %s 失败：%v", tbl, err)
		}
		if n != 1 {
			t.Fatalf("★ 表 %s 不存在 —— 迁移未建出它", tbl)
		}
	}

	// 必须**不**存在：这些是初版自创的平行结构，重新引入即「两套真相」
	mustNotExist := []string{
		"dim_user_group", "fact_group_membership", "fact_group_grant",
		"dim_account_entitlement", "fact_request_approval", "fact_request_cc",
	}
	for _, tbl := range mustNotExist {
		var n int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			 WHERE table_schema='public' AND table_name=$1`, tbl).Scan(&n); err != nil {
			t.Fatalf("查询表 %s 失败：%v", tbl, err)
		}
		if n != 0 {
			t.Fatalf("★ 表 %s 不该存在 —— 它会让同一事实有两处真相（见 store.go 顶部复盘）", tbl)
		}
	}

	// 0001 的 fact_permission_request 必须**没有** region 列：
	// 契约（contracts/permission-request.ts）里没有这个字段，
	// 加它就是悄悄扩契约。
	var hasRegion bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name='fact_permission_request' AND column_name='region'
		)`).Scan(&hasRegion); err != nil {
		t.Fatalf("查询列失败：%v", err)
	}
	if hasRegion {
		t.Fatal("★ fact_permission_request 不该有 region 列（契约里没有该字段）")
	}
}

// TestIntegration_GroupRoundTrip 组 + 成员 + 授权 的写入/读回闭环。
func TestIntegration_GroupRoundTrip(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	gid := "grp_it_" + time.Now().Format("150405.000000")
	acct := "it_member_" + time.Now().Format("150405.000000")
	seedOrg(t, ctx, s, acct)
	t.Cleanup(func() { cleanupGroup(ctx, s, gid); cleanupOrg(ctx, s, acct) })

	if err := s.UpsertGroup(ctx, &group.Group{
		ID: gid, Name: "集成测试组", Description: "round-trip",
		Owners: []string{"u.owner"}, Restricted: false,
	}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
	if err := s.ReplaceGrants(ctx, gid, []group.Grant{
		{Kind: group.KindModule, Key: "module.report"},
		{Kind: group.KindDimension, Key: "dimension.channel", Values: []string{"TK-TH"}},
		{Kind: group.KindLevel, Key: "level.L3"},
	}, "u.admin", "集成测试"); err != nil {
		t.Fatalf("ReplaceGrants: %v", err)
	}
	if err := s.SetMembership(ctx, group.Membership{
		GroupID: gid, Account: acct, Role: "member", InheritsGrants: true,
	}); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}

	g, err := s.LoadGroup(ctx, gid)
	if err != nil {
		t.Fatalf("LoadGroup: %v", err)
	}
	if g.Name != "集成测试组" {
		t.Fatalf("组名读回不符：%+v", g)
	}
	if len(g.Grants) != 3 {
		t.Fatalf("应有 3 条授权，实际 %d：%+v", len(g.Grants), g.Grants)
	}

	groups, ms, err := s.LoadGroupsFor(ctx, acct)
	if err != nil {
		t.Fatalf("LoadGroupsFor: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != gid {
		t.Fatalf("应读回 1 个组（%s），实际 %+v", gid, groups)
	}
	if len(ms) != 1 || !ms[0].InheritsGrants {
		t.Fatalf("成员关系应继承授权，实际 %+v", ms)
	}
}

// TestIntegration_ReplaceGrantsRemovesDropped 整体替换必须能**撤销**授权。
//
// ★ 这正是「逐条 upsert」会漏掉的场景：界面上取消勾选 module.pnl 后，
//   若用 upsert 而非整体覆盖，那一项会永久留在 grants 里 ⇒ 权限依然生效。
func TestIntegration_ReplaceGrantsRemovesDropped(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	gid := "grp_drop_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { cleanupGroup(ctx, s, gid) })

	mustGroup(t, ctx, s, gid)
	if err := s.ReplaceGrants(ctx, gid, []group.Grant{
		{Kind: group.KindModule, Key: "module.report"},
		{Kind: group.KindModule, Key: "module.pnl"},
	}, "u.admin", ""); err != nil {
		t.Fatalf("首次 ReplaceGrants: %v", err)
	}
	// 取消勾选 module.pnl
	if err := s.ReplaceGrants(ctx, gid, []group.Grant{
		{Kind: group.KindModule, Key: "module.report"},
	}, "u.admin", ""); err != nil {
		t.Fatalf("二次 ReplaceGrants: %v", err)
	}

	g, err := s.LoadGroup(ctx, gid)
	if err != nil {
		t.Fatalf("LoadGroup: %v", err)
	}
	for _, gr := range g.Grants {
		if gr.Key == "module.pnl" {
			t.Fatal("★ 取消勾选后 module.pnl 仍在库中 —— 权限未真正撤销")
		}
	}
	if len(g.Grants) != 1 {
		t.Fatalf("应只剩 1 条授权，实际 %d：%+v", len(g.Grants), g.Grants)
	}
}

// TestIntegration_GrantChangeLogIsWritten 授权变更必须留下可检索的流水。
//
// ★ 这条防的是「改了权限但没留痕」——M-REQ/M-GROUP 的价值之一就是可追溯。
//   流水与主表同事务，所以这里断言的是「一次成功替换 = 恰好一组流水」。
func TestIntegration_GrantChangeLogIsWritten(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	gid := "grp_log_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { cleanupGroup(ctx, s, gid) })

	mustGroup(t, ctx, s, gid)
	// 首轮：授予 report + pnl → 2 条 GRANT
	if err := s.ReplaceGrants(ctx, gid, []group.Grant{
		{Kind: group.KindModule, Key: "module.report"},
		{Kind: group.KindModule, Key: "module.pnl"},
	}, "u.admin", "首轮"); err != nil {
		t.Fatalf("首轮 ReplaceGrants: %v", err)
	}
	// 次轮：去掉 pnl、加上 admin → 1 GRANT + 1 REVOKE
	if err := s.ReplaceGrants(ctx, gid, []group.Grant{
		{Kind: group.KindModule, Key: "module.report"},
		{Kind: group.KindModule, Key: "module.admin"},
	}, "u.admin", "调整"); err != nil {
		t.Fatalf("次轮 ReplaceGrants: %v", err)
	}

	var grants, revokes int
	if err := s.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE action='GRANT'),
			COUNT(*) FILTER (WHERE action='REVOKE')
		FROM fact_group_grant_change WHERE group_id=$1`, gid).Scan(&grants, &revokes); err != nil {
		t.Fatalf("统计流水：%v", err)
	}
	if grants != 3 {
		t.Fatalf("GRANT 流水应 3 条（首轮 2 + 次轮 1），实际 %d", grants)
	}
	if revokes != 1 {
		t.Fatalf("REVOKE 流水应 1 条（pnl），实际 %d", revokes)
	}

	// 按 key 检索：还能查到「module.pnl 曾被授予过」（限定本组，避免受其他测试残留影响）
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM fact_group_grant_change
		 WHERE group_id=$1 AND kind='module' AND key='module.pnl'`, gid).
		Scan(&n); err != nil {
		t.Fatalf("按 key 检索流水：%v", err)
	}
	if n != 2 {
		t.Fatalf("module.pnl 应有 2 条流水（授予 + 收回），实际 %d", n)
	}
}

// TestIntegration_DenyViolatesConstraint IT 组带业务数值必须被库层拦住。
func TestIntegration_ITGroupCannotGrantBusinessValues(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	gid := "grp_itbad_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { cleanupGroup(ctx, s, gid) })

	// Restricted=true ⇒ isIT=true；再配 canViewBusinessValues=true ⇒ 必须失败
	err := s.UpsertGroup(ctx, &group.Group{
		ID: gid, Name: "IT 组", Restricted: true,
	})
	if err != nil {
		t.Fatalf("空授权写入不该失败：%v", err)
	}
	// 直接写库模拟「绕过应用层」的尝试
	_, err = s.pool.Exec(ctx, `
		UPDATE dim_group SET grants = '{"isIT":true,"canViewBusinessValues":true}'::jsonb
		WHERE id=$1`, gid)
	if err == nil {
		t.Fatal("★ D7 违反：IT 组带 canViewBusinessValues=true 竟被写进去了")
	}
	if !strings.Contains(err.Error(), "ck_dim_group_spark_cicada_it_no_business") {
		t.Fatalf("应由 0006 的约束拦下，实际错误 %v", err)
	}
}

// TestIntegration_IllegalMaxLevelRejected 非法密级必须被库层拦住。
//
// ★ 为什么要在库层拦：非法密级（如 "L9"）到了求值器里，
//   行为取决于实现细节（当 L1 还是当 L4），而其中一种选择就是静默扩权。
func TestIntegration_IllegalMaxLevelRejected(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	gid := "grp_l9_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { cleanupGroup(ctx, s, gid) })

	mustGroup(t, ctx, s, gid)
	_, err := s.pool.Exec(ctx,
		`UPDATE dim_group SET grants = '{"maxLevel":"L9"}'::jsonb WHERE id=$1`, gid)
	if err == nil {
		t.Fatal("★ 非法密级 L9 竟被写进去了")
	}
	if !strings.Contains(err.Error(), "ck_dim_group_spark_cicada_max_level") {
		t.Fatalf("应由 0006 的密级约束拦下，实际错误 %v", err)
	}
}

// TestIntegration_EvaluateUnionAndDeny 组并集 + DENY 优先（真库往返后仍成立）。
func TestIntegration_EvaluateUnionAndDeny(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	acct := "it_union_" + time.Now().Format("150405.000000")
	g1 := "grp_u1_" + time.Now().Format("150405.000000")
	g2 := "grp_u2_" + time.Now().Format("150405.000000")
	seedOrg(t, ctx, s, acct)
	t.Cleanup(func() {
		cleanupGroup(ctx, s, g1)
		cleanupGroup(ctx, s, g2)
		cleanupOrg(ctx, s, acct)
	})

	// g1 给 report + channel[TK-TH]，g2 给 pnl（并集应含两者）
	mustGroup(t, ctx, s, g1)
	mustGroup(t, ctx, s, g2)
	if err := s.ReplaceGrants(ctx, g1, []group.Grant{
		{Kind: group.KindModule, Key: "module.report"},
		{Kind: group.KindDimension, Key: "dimension.channel", Values: []string{"TK-TH"}},
	}, "u.admin", ""); err != nil {
		t.Fatalf("ReplaceGrants g1: %v", err)
	}
	if err := s.ReplaceGrants(ctx, g2, []group.Grant{
		{Kind: group.KindModule, Key: "module.pnl"},
	}, "u.admin", ""); err != nil {
		t.Fatalf("ReplaceGrants g2: %v", err)
	}
	for _, gid := range []string{g1, g2} {
		if err := s.SetMembership(ctx, group.Membership{
			GroupID: gid, Account: acct, Role: "member", InheritsGrants: true,
		}); err != nil {
			t.Fatalf("SetMembership %s: %v", gid, err)
		}
	}

	groups, ms, err := s.LoadGroupsFor(ctx, acct)
	if err != nil {
		t.Fatalf("LoadGroupsFor: %v", err)
	}
	mptrs := make([]*group.Membership, len(ms))
	for i := range ms {
		mptrs[i] = &ms[i]
	}
	ev := group.Evaluate(&group.Personal{Account: acct, MaxLevel: group.L1}, groups, mptrs)

	mods := map[string]bool{}
	for _, m := range ev.Modules {
		mods[m] = true
	}
	if !mods["module.report"] || !mods["module.pnl"] {
		t.Fatalf("★ 多组并集应同时含 report 与 pnl，实际 %v", ev.Modules)
	}
	if len(ev.Dimensions["channel"]) != 1 || ev.Dimensions["channel"][0] != "TK-TH" {
		t.Fatalf("维度应并集继承，实际 %v", ev.Dimensions)
	}
}

// TestIntegration_PersonalDenyBeatsGroupAllow 个人 DENY 压过组允许（真库往返）。
func TestIntegration_PersonalDenyBeatsGroupAllow(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	acct := "it_deny_" + time.Now().Format("150405.000000")
	gid := "grp_dn_" + time.Now().Format("150405.000000")
	seedOrg(t, ctx, s, acct)
	t.Cleanup(func() { cleanupGroup(ctx, s, gid); cleanupOrg(ctx, s, acct) })

	// 个人启用 report，同时在 disabled 里放 report ⇒ 个人 DENY
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO fact_entitlement (account, base_template, modules, dimensions, max_level)
		VALUES ($1, 'tpl.base', '{"enabled":["module.report"],"disabled":["module.report"]}'::jsonb,
		        '[]'::jsonb, 'L1')`, acct); err != nil {
		t.Fatalf("插入个人权威：%v", err)
	}
	mustGroup(t, ctx, s, gid)
	if err := s.ReplaceGrants(ctx, gid, []group.Grant{
		{Kind: group.KindModule, Key: "module.report"},
	}, "u.admin", ""); err != nil {
		t.Fatalf("ReplaceGrants: %v", err)
	}
	if err := s.SetMembership(ctx, group.Membership{
		GroupID: gid, Account: acct, Role: "member", InheritsGrants: true,
	}); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}

	p, err := s.LoadPersonal(ctx, acct)
	if err != nil {
		t.Fatalf("LoadPersonal: %v", err)
	}
	if len(p.Denies) == 0 {
		t.Fatal("disabled 模块应被读成个人 DENY")
	}
	groups, ms, err := s.LoadGroupsFor(ctx, acct)
	if err != nil {
		t.Fatalf("LoadGroupsFor: %v", err)
	}
	mptrs := make([]*group.Membership, len(ms))
	for i := range ms {
		mptrs[i] = &ms[i]
	}
	ev := group.Evaluate(p, groups, mptrs)
	for _, m := range ev.Modules {
		if m == "module.report" {
			t.Fatalf("★ 个人 DENY 未压过组允许：report 仍在 %v", ev.Modules)
		}
	}
}

// TestIntegration_NonInheritingMemberGetsNothing 退出继承后不得获得组授权。
func TestIntegration_NonInheritingMemberGetsNothing(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	acct := "it_ninh_" + time.Now().Format("150405.000000")
	gid := "grp_nh_" + time.Now().Format("150405.000000")
	seedOrg(t, ctx, s, acct)
	t.Cleanup(func() { cleanupGroup(ctx, s, gid); cleanupOrg(ctx, s, acct) })

	mustGroup(t, ctx, s, gid)
	if err := s.ReplaceGrants(ctx, gid, []group.Grant{
		{Kind: group.KindLevel, Key: "level.L3"},
	}, "u.admin", ""); err != nil {
		t.Fatalf("ReplaceGrants: %v", err)
	}
	if err := s.SetMembership(ctx, group.Membership{
		GroupID: gid, Account: acct, Role: "member", InheritsGrants: false,
	}); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}

	groups, ms, err := s.LoadGroupsFor(ctx, acct)
	if err != nil {
		t.Fatalf("LoadGroupsFor: %v", err)
	}
	if len(ms) != 1 || ms[0].InheritsGrants {
		t.Fatalf("前置条件：成员应存在且 inherit=false，实际 %+v", ms)
	}
	mptrs := make([]*group.Membership, len(ms))
	for i := range ms {
		mptrs[i] = &ms[i]
	}
	ev := group.Evaluate(&group.Personal{Account: acct, MaxLevel: group.L1}, groups, mptrs)
	if ev.MaxLevel != group.L1 {
		t.Fatalf("★ 退出继承的成员不得获得组密级，实际 %s", ev.MaxLevel)
	}
}

// TestIntegration_RequestRoundTrip 申请单 + 审批步骤 + 抄送 的落库/读回闭环。
func TestIntegration_RequestRoundTrip(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	rid := "req_it_" + time.Now().Format("150405.000000")
	acct := seedApplicant(t, ctx, s, rid)
	t.Cleanup(func() { cleanupRequest(ctx, s, rid); cleanupOrg(ctx, s, acct) })

	exp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Microsecond)
	r := &req.Request{
		ID: rid, Applicant: acct,
		Draft: req.Draft{
			Modules:  []string{"module.pnl"},
			MaxLevel: authz.L3,
		},
		Purpose:         "集成测试",
		RequestedExpiry: &exp,
		Status:          req.StatusCosignPending,
		Approvals: []req.ApprovalStep{
			{Approver: "u.lead", Tier: "T1", Action: "PENDING"},
		},
		CCs: []chain.CcRecord{
			{Cc: "u.lead", Reason: "L4", Mode: chain.CcCosign,
				NotifiedAt: time.Now().UTC().Truncate(time.Microsecond)},
		},
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := s.SaveRequest(ctx, r); err != nil {
		t.Fatalf("SaveRequest: %v", err)
	}

	got, err := s.LoadRequest(ctx, rid)
	if err != nil {
		t.Fatalf("LoadRequest: %v", err)
	}
	if got.Applicant != acct || got.Status != req.StatusCosignPending {
		t.Fatalf("基本字段不符：%+v", got)
	}
	if len(got.Draft.Modules) != 1 || got.Draft.Modules[0] != "module.pnl" {
		t.Fatalf("草案未往返：%+v", got.Draft)
	}
	if len(got.Approvals) != 1 || got.Approvals[0].Approver != "u.lead" {
		t.Fatalf("审批步骤未往返：%+v", got.Approvals)
	}
	if len(got.CCs) != 1 || got.CCs[0].Mode != chain.CcCosign {
		t.Fatalf("抄送未往返：%+v", got.CCs)
	}
	if got.CCs[0].DecidedAt != nil {
		t.Fatalf("★ 尚未表态的会签，decidedAt 必须读回 nil（否则会签闸门失效），实际 %v",
			got.CCs[0].DecidedAt)
	}
}

// TestIntegration_CosignDecidedAtSurvives 会签表态后 decidedAt 必须真的写进库。
//
// ★ 这是「未表态 vs 已同意」语义位持久化的另一半断言：
//   上一条测 nil 能保留，这一条测非 nil 能保留。两条合起来才闭环。
func TestIntegration_CosignDecidedAtSurvives(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	rid := "req_cc_" + time.Now().Format("150405.000000")
	acct := seedApplicant(t, ctx, s, rid)
	t.Cleanup(func() { cleanupRequest(ctx, s, rid); cleanupOrg(ctx, s, acct) })

	decided := time.Now().UTC().Truncate(time.Microsecond)
	r := &req.Request{
		ID: rid, Applicant: acct,
		Draft:   req.Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L3},
		Purpose: "会签持久化", Status: req.StatusApproving,
		CCs: []chain.CcRecord{
			{Cc: "u.lead", Reason: "L4", Mode: chain.CcCosign,
				NotifiedAt: decided, DecidedAt: &decided},
		},
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := s.SaveRequest(ctx, r); err != nil {
		t.Fatalf("SaveRequest: %v", err)
	}
	got, err := s.LoadRequest(ctx, rid)
	if err != nil {
		t.Fatalf("LoadRequest: %v", err)
	}
	if len(got.CCs) != 1 || got.CCs[0].DecidedAt == nil {
		t.Fatalf("★ 已表态的会签，decidedAt 必须读回非 nil，实际 %+v", got.CCs)
	}
}

// TestIntegration_SaveRequestReplacesStaleRows 二次保存不得留下旧步骤/旧抄送。
func TestIntegration_SaveRequestReplacesStaleRows(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	rid := "req_repl_" + time.Now().Format("150405.000000")
	acct := seedApplicant(t, ctx, s, rid)
	t.Cleanup(func() { cleanupRequest(ctx, s, rid); cleanupOrg(ctx, s, acct) })

	r := &req.Request{
		ID: rid, Applicant: acct,
		Draft:   req.Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L1},
		Purpose: "替换语义", Status: req.StatusApproving,
		Approvals: []req.ApprovalStep{
			{Approver: "u.manager", Tier: "T2", Action: "PENDING"},
		},
		CCs: []chain.CcRecord{
			{Cc: "u.lead", Reason: "+2", Mode: chain.CcNotify,
				NotifiedAt: time.Now().UTC().Truncate(time.Microsecond)},
		},
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := s.SaveRequest(ctx, r); err != nil {
		t.Fatalf("首次 SaveRequest: %v", err)
	}
	// 驳回：步骤变 REJECT，抄送清空
	r.Status = req.StatusRejected
	r.Approvals = []req.ApprovalStep{
		{Approver: "u.manager", Tier: "T2", Action: "REJECT", Reason: "不必要"},
	}
	r.CCs = nil
	now := time.Now().UTC().Truncate(time.Microsecond)
	r.ResolvedAt = &now
	if err := s.SaveRequest(ctx, r); err != nil {
		t.Fatalf("二次 SaveRequest: %v", err)
	}

	got, err := s.LoadRequest(ctx, rid)
	if err != nil {
		t.Fatalf("LoadRequest: %v", err)
	}
	if got.Status != req.StatusRejected {
		t.Fatalf("状态应为 REJECTED，实际 %s", got.Status)
	}
	if len(got.Approvals) != 1 || got.Approvals[0].Action != "REJECT" {
		t.Fatalf("★ 旧步骤未被替换（应只剩 REJECT），实际 %+v", got.Approvals)
	}
	if len(got.CCs) != 0 {
		t.Fatalf("★ 旧抄送未被清空，实际 %+v", got.CCs)
	}
	if got.ResolvedAt == nil {
		t.Fatal("★ resolved_at 必须被写入（终结态的语义位）")
	}
}

// TestIntegration_ListReclaimable 到期回收检索必须准确。
//
// ★ 这条同时验证 0006 的索引条件与查询条件是否对齐 ——
//   若两边条件不一致，索引不会被用上（这里测不出性能，但能测出语义：
//   不该出现的（未到期/无 expiry/非 APPROVED）绝不能出现）。
func TestIntegration_ListReclaimable(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	stamp := time.Now().Format("150405.000000")
	past := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	future := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Microsecond)

	// 应被回收
	ridExpired := "req_exp_" + stamp
	acctA := seedApplicant(t, ctx, s, ridExpired)
	t.Cleanup(func() { cleanupRequest(ctx, s, ridExpired); cleanupOrg(ctx, s, acctA) })
	saveReq(t, ctx, s, &req.Request{
		ID: ridExpired, Applicant: acctA,
		Draft:   req.Draft{Modules: []string{"module.report"}, MaxLevel: authz.L1},
		Purpose: "已到期", Status: req.StatusApproved,
		RequestedExpiry: &past,
		CreatedAt:       time.Now().UTC().Truncate(time.Microsecond),
	})

	// 不应被回收：未到期
	ridFuture := "req_fut_" + stamp
	acctB := seedApplicant(t, ctx, s, ridFuture)
	t.Cleanup(func() { cleanupRequest(ctx, s, ridFuture); cleanupOrg(ctx, s, acctB) })
	saveReq(t, ctx, s, &req.Request{
		ID: ridFuture, Applicant: acctB,
		Draft:   req.Draft{Modules: []string{"module.report"}, MaxLevel: authz.L1},
		Purpose: "未到期", Status: req.StatusApproved,
		RequestedExpiry: &future,
		CreatedAt:       time.Now().UTC().Truncate(time.Microsecond),
	})

	// 不应被回收：长期（无 expiry）
	ridForever := "req_fvr_" + stamp
	acctC := seedApplicant(t, ctx, s, ridForever)
	t.Cleanup(func() { cleanupRequest(ctx, s, ridForever); cleanupOrg(ctx, s, acctC) })
	saveReq(t, ctx, s, &req.Request{
		ID: ridForever, Applicant: acctC,
		Draft:   req.Draft{Modules: []string{"module.report"}, MaxLevel: authz.L1},
		Purpose: "长期", Status: req.StatusApproved,
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	})

	// 不应被回收：状态不对
	ridDraft := "req_dft_" + stamp
	acctD := seedApplicant(t, ctx, s, ridDraft)
	t.Cleanup(func() { cleanupRequest(ctx, s, ridDraft); cleanupOrg(ctx, s, acctD) })
	saveReq(t, ctx, s, &req.Request{
		ID: ridDraft, Applicant: acctD,
		Draft:   req.Draft{Modules: []string{"module.report"}, MaxLevel: authz.L1},
		Purpose: "草稿", Status: req.StatusWithdrawn,
		RequestedExpiry: &past,
		CreatedAt:       time.Now().UTC().Truncate(time.Microsecond),
	})

	ids, err := s.ListReclaimable(ctx, time.Now(), 1000)
	if err != nil {
		t.Fatalf("ListReclaimable: %v", err)
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if !set[ridExpired] {
		t.Fatalf("★ 已到期的 %s 未被列出", ridExpired)
	}
	for _, bad := range []string{ridFuture, ridForever, ridDraft} {
		if set[bad] {
			t.Fatalf("★ %s 不该出现在待回收列表里", bad)
		}
	}
}

// TestIntegration_ListPendingFor 按审批人检索待批（走 jsonb containment）。
func TestIntegration_ListPendingFor(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	stamp := time.Now().Format("150405.000000")
	approver := "u_pend_" + stamp
	rid := "req_pend_" + stamp
	acct := seedApplicant(t, ctx, s, rid)
	t.Cleanup(func() { cleanupRequest(ctx, s, rid); cleanupOrg(ctx, s, acct) })

	saveReq(t, ctx, s, &req.Request{
		ID: rid, Applicant: acct,
		Draft:   req.Draft{Modules: []string{"module.report"}, MaxLevel: authz.L1},
		Purpose: "待批", Status: req.StatusApproving,
		Approvals: []req.ApprovalStep{
			{Approver: approver, Tier: "T2", Action: "PENDING"},
		},
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	})

	ids, err := s.ListPendingFor(ctx, approver, 100)
	if err != nil {
		t.Fatalf("ListPendingFor: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == rid {
			found = true
		}
	}
	if !found {
		t.Fatalf("★ 待批申请 %s（审批人 %s）未被检索到", rid, approver)
	}

	// 反向：换个审批人就不该看到
	other, err := s.ListPendingFor(ctx, "u_nobody_"+stamp, 100)
	if err != nil {
		t.Fatalf("ListPendingFor(other): %v", err)
	}
	for _, id := range other {
		if id == rid {
			t.Fatalf("★ 不相干审批人也检索到了 %s", rid)
		}
	}
}

// TestIntegration_CosignPendingView 会签待办视图必须只列未表态者。
func TestIntegration_CosignPendingView(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	stamp := time.Now().Format("150405.000000")
	rid := "req_csp_" + stamp
	acct := seedApplicant(t, ctx, s, rid)
	t.Cleanup(func() { cleanupRequest(ctx, s, rid); cleanupOrg(ctx, s, acct) })

	decided := time.Now().UTC().Truncate(time.Microsecond)
	saveReq(t, ctx, s, &req.Request{
		ID: rid, Applicant: acct,
		Draft:   req.Draft{Modules: []string{"module.pnl"}, MaxLevel: authz.L4},
		Purpose: "会签待办", Status: req.StatusCosignPending,
		CCs: []chain.CcRecord{
			// 已表态 —— 不该出现在待办里
			{Cc: "u_a_" + stamp, Reason: "L4", Mode: chain.CcCosign,
				NotifiedAt: decided, DecidedAt: &decided},
			// 未表态 —— 应出现在待办里
			{Cc: "u_b_" + stamp, Reason: "L4", Mode: chain.CcCosign,
				NotifiedAt: decided},
			// notify 模式 —— 不该出现在待办里
			{Cc: "u_c_" + stamp, Reason: "+2", Mode: chain.CcNotify,
				NotifiedAt: decided},
		},
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	})

	pend, err := s.ListCosignPending(ctx)
	if err != nil {
		t.Fatalf("ListCosignPending: %v", err)
	}
	got := pend[rid]
	if len(got) != 1 || got[0] != "u_b_"+stamp {
		t.Fatalf("★ 待办应只有 u_b（未表态的 cosign），实际 %v", got)
	}
}

// TestIntegration_UnknownPersonalIsEmptyNotError 未登记的账号返回空基线，不是错误。
func TestIntegration_UnknownPersonalIsEmptyNotError(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	p, err := s.LoadPersonal(ctx, "u_never_registered_xyz")
	if err != nil {
		t.Fatalf("★ 未登记账号不应返回 error，实际 %v", err)
	}
	if p.Account != "u_never_registered_xyz" || p.MaxLevel != group.L1 {
		t.Fatalf("应为空基线（L1），实际 %+v", p)
	}
	if len(p.Modules) != 0 || len(p.Denies) != 0 {
		t.Fatalf("空基线的模块/拒绝列表应为空，实际 %+v", p)
	}
}

// TestIntegration_NilSlicesDoNotViolateNotNull nil 切片入库不得撞 NOT NULL。
//
// ★ 真实事故（M-COLLECT，SQLSTATE 23502）：`text[] NOT NULL DEFAULT '{}'`
//   的 DEFAULT 只在**不提供该列**时生效；显式传 NULL 会直接撞 NOT NULL，
//   而 Go 的 nil []string 经 pgx 正是被编码成 SQL NULL。
func TestIntegration_NilSlicesDoNotViolateNotNull(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	gid := "grp_nil_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { cleanupGroup(ctx, s, gid) })

	if err := s.UpsertGroup(ctx, &group.Group{
		ID: gid, Name: "nil 切片组", Owners: nil, Restricted: false,
	}); err != nil {
		t.Fatalf("★ nil owners 撞了 NOT NULL：%v", err)
	}
	if err := s.ReplaceGrants(ctx, gid, []group.Grant{
		{Kind: group.KindModule, Key: "module.report", Values: nil},
	}, "u.admin", ""); err != nil {
		t.Fatalf("★ nil values 撞了 NOT NULL：%v", err)
	}
	g, err := s.LoadGroup(ctx, gid)
	if err != nil {
		t.Fatalf("LoadGroup: %v", err)
	}
	if g.Owners == nil {
		t.Fatal("读回时 owners 应被规范化为空切片而非 nil（下游可安全 range）")
	}
}

// TestIntegration_SaveRequestNilApprovalsCCs 空审批/空抄送不得撞 NOT NULL。
func TestIntegration_SaveRequestNilApprovalsCCs(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	rid := "req_nil_" + time.Now().Format("150405.000000")
	acct := seedApplicant(t, ctx, s, rid)
	t.Cleanup(func() { cleanupRequest(ctx, s, rid); cleanupOrg(ctx, s, acct) })

	// Approvals / CCs 都是 nil
	if err := s.SaveRequest(ctx, &req.Request{
		ID: rid, Applicant: acct,
		Draft:   req.Draft{Modules: []string{"module.report"}, MaxLevel: authz.L1},
		Purpose: "nil 数组", Status: req.StatusDraft,
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}); err != nil {
		t.Fatalf("★ nil approvals/ccs 撞了 NOT NULL：%v", err)
	}
	got, err := s.LoadRequest(ctx, rid)
	if err != nil {
		t.Fatalf("LoadRequest: %v", err)
	}
	if got.Approvals == nil || got.CCs == nil {
		t.Fatalf("读回应是空切片而非 nil，实际 approvals=%v ccs=%v", got.Approvals, got.CCs)
	}
}

// ───────────────────────────── 清理帮手 ─────────────────────────────

func mustGroup(t *testing.T, ctx context.Context, s *Store, gid string) {
	t.Helper()
	if err := s.UpsertGroup(ctx, &group.Group{ID: gid, Name: "集成测试组"}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
}

// seedApplicant 建一个 dim_org 账号（fact_permission_request 有 FK 指向它）。
func seedApplicant(t *testing.T, ctx context.Context, s *Store, rid string) string {
	t.Helper()
	acct := "u_" + rid
	seedOrg(t, ctx, s, acct)
	return acct
}

func seedOrg(t *testing.T, ctx context.Context, s *Store, acct string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO dim_org (account, display_name, tier, primary_dept)
		VALUES ($1, $1, 'T3', 'd.ops')
		ON CONFLICT (account) DO NOTHING`, acct); err != nil {
		t.Fatalf("建 dim_org %s: %v", acct, err)
	}
}

func saveReq(t *testing.T, ctx context.Context, s *Store, r *req.Request) {
	t.Helper()
	if err := s.SaveRequest(ctx, r); err != nil {
		t.Fatalf("SaveRequest %s: %v", r.ID, err)
	}
}

func cleanupGroup(ctx context.Context, s *Store, gid string) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM dim_group WHERE id=$1`, gid)
}

func cleanupOrg(ctx context.Context, s *Store, acct string) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM fact_entitlement WHERE account=$1`, acct)
	_, _ = s.pool.Exec(ctx, `DELETE FROM dim_org WHERE account=$1`, acct)
}

func cleanupRequest(ctx context.Context, s *Store, rid string) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM fact_permission_request WHERE id=$1`, rid)
}
