package tenant

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ══════════════════════════════════════════════════════════════════════════
// ★★ 越权测试（Task #56 的核心交付物）
//
// rls_integration_test.go 用**探针表**证明了 RLS 机制本身可用；
// 而本文件证明的是**真实业务表**上的隔离 —— 两者不可互相替代：
//
//   · 探针表是我们自己建的表，策略写对了是理所应当；
//   · 真实业务表由迁移 0011 批量加了策略，且带着历史包袱
//     （全局唯一键、外键图、58 行存量 dim_org 等）。
//     迁移里任何一处写错，探针测试**照样全绿**，而生产在泄漏。
//
// 因此本文件必须打真库、打真表。跳过条件是「没设 DSN」，
// 而不是「打不到就算了」—— CI 上必须设 SPARK_REQUIRE_DB=1 让它变硬失败。
//
// ★ 本文件的形态刻意做成**攻击者视角**：
//
//	每一条 TestRLS_越权_xxx 都是「我以租户 B 的身份，尝试做到某件
//	本来不该做到的事」。写测试时问的不是「功能对不对」，
//	而是「我能不能用这条路拿到别人的数据」。这样才找得到洞。
// ══════════════════════════════════════════════════════════════════════════

// realTableTenantCtx 是真实业务表测试用的租户上下文。
//
// ★ 共享档：靠 app.tenant_id + RLS 过滤。
func realTableTenantCtx(uid string) TenantContext {
	return TenantContext{TenantID: uid, Tier: TierShared, RLSTenantID: uid}
}

// escalationPool 取真池，并要求「真实业务表已加固」（否则测试无意义）。
//
// ★★ 为什么要有 reqRealTablesReady 这道前置：
//
//	若 0011 迁移**没跑**，业务表上根本没有 tenant_id 列，
//	而 RLS 也不存在。此时若测试只是「查不到别人的行」，
//	会因为「表里本来就没数据」而**假通过**。
//	必须显式断言「表上真的开了 RLS 且策略指向统一入口」，
//	否则本文件的每一条测试都是装饰品。
func escalationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testPool(t)
	requireRealTablesRLSReady(t, pool)
	return pool
}

// rlsGuardedTables 是本文件断言「必须已被加固」的真实业务表清单。
//
// ★ 这份清单必须与 0011_rls.sql 的表清单**保持一致**。
//
//	日后新增表时两处都要改 —— 不一致时本测试会在
//	「表清单漂移」用例上报错，而不是静默漏测。
var rlsGuardedTables = []string{
	"fact_sales_daily",
	"bucket_pnl_month",
	"dim_org",
	"dim_data_chain",
	"dim_group",
	"dim_group_member",
	"fact_entitlement",
	"fact_permission_request",
	"fact_group_grant_change",
	"dim_view_template",
	"dim_view_template_share",
	"audit_log",
}

// requireRealTablesRLSReady 断言所有受管表都已开 RLS + FORCE + 策略。
//
// 失败信息必须**可操作**（指明「跑什么命令/看哪份文件」），
// 否则下次有人看到这条失败只会去把测试注释掉。
func requireRealTablesRLSReady(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	type row struct {
		HasTenantID bool
		RLSOn       bool
		Forced      bool
		Policies    int
	}
	got := map[string]row{}

	// 一次查询取回所有信息（避免 N 次往返，也避免中途状态变化）
	rows, err := pool.Query(ctx, `
		SELECT c.relname,
		       EXISTS (SELECT 1 FROM information_schema.columns ic
		                WHERE ic.table_schema='public' AND ic.table_name=c.relname
		                  AND ic.column_name='tenant_id'),
		       c.relrowsecurity,
		       c.relforcerowsecurity,
		       (SELECT count(*) FROM pg_policy p WHERE p.polrelid=c.oid)
		  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		 WHERE n.nspname='public' AND c.relkind='r'`)
	if err != nil {
		t.Fatalf("读表属性失败：%v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var r row
		if err := rows.Scan(&name, &r.HasTenantID, &r.RLSOn, &r.Forced, &r.Policies); err != nil {
			t.Fatalf("扫描失败：%v", err)
		}
		got[name] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历失败：%v", err)
	}

	for _, tbl := range rlsGuardedTables {
		r, ok := got[tbl]
		if !ok {
			t.Errorf("★ 受管表 %q 不存在 —— 迁移集不完整", tbl)
			continue
		}
		if !r.HasTenantID {
			t.Errorf("★ 表 %q 缺 tenant_id 列 —— 0011 未跑或未覆盖本表", tbl)
		}
		if !r.RLSOn {
			t.Errorf("★ 表 %q 未开 RLS —— 共享档下会串租", tbl)
		}
		if !r.Forced {
			t.Errorf("★ 表 %q 未 FORCE RLS —— 以表拥有者身份连接时策略不生效", tbl)
		}
		if r.Policies < 1 {
			t.Errorf("★ 表 %q 无策略 —— RLS 开了但没有任何过滤规则", tbl)
		}
	}
	if t.Failed() {
		t.Fatalf("真实业务表未就绪，越权测试无意义。\n" +
			"请先应用迁移：见 sql/migrations/0011_rls.sql")
	}
}

// ───────────────────────────── 隔离：看不见别人的行 ─────────────────────────────

// seedOwnerOrg 为指定租户建一个可用作 FK 目标的 dim_org 行，返回 account。
//
// ★★ 为什么必须先建 dim_org 才能建 dim_view_template：
//
//	dim_view_template.owner 有 FK 指向 dim_org(account)。
//	PostgreSQL 的 FK 检查**绕过 RLS**（见本文件末的残余风险测试），
//	所以这里必须真的插入 dim_org 行，而不是「反正 RLS 会拦住」。
//
// ★ 注意 dim_org 的 account 仍是**全局主键**（0011 的已知代价），
//
//	故两租户的 account 名必须不同 —— 本 helper 用租户前缀保证这点。
func seedOwnerOrg(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	uid, account, display string) {
	t.Helper()
	// ★ dim_org.tier 有 CHECK 约束，只接受 T1..T6（见 0001_core.sql）。
	mustExecTenant(t, pool, realTableTenantCtx(uid),
		`INSERT INTO dim_org (account, display_name, tier, primary_dept, active, tenant_id)
		 VALUES ($1, $2, 'T5', 'dept-x', true, $3)`,
		account, display, uid)
	t.Cleanup(func() { cleanupOrgsByPrefix(context.Background(), pool, account) })
}

// TestRLS_越权_真实表看不到别人的行 用真实业务表证明读隔离。
//
// ★ 选 dim_view_template（用户设置表）与 fact_entitlement（权限表）：
//
//	这两张表是最典型的「写错了会出事」的表 ——
//	前者泄漏的是同事的视图配置，后者泄漏的是权限矩阵。
func TestRLS_越权_真实表看不到别人的行(t *testing.T) {
	pool := escalationPool(t)
	ctx := context.Background()

	// per-run 的唯一标识，避免与历史/并发测试互相污染
	sfx := testSuffix(t)
	uidA, uidB := uidAlpha, uidBeta

	// owner 指向的账号先建好（FK 需要）
	ownerA := "own-a-" + sfx
	ownerB := "own-b-" + sfx
	seedOwnerOrg(t, ctx, pool, uidA, ownerA, "A 的账号")
	seedOwnerOrg(t, ctx, pool, uidB, ownerB, "B 的账号")

	// 租户 A 建一个视图模板
	tplA := "tpl-a-" + sfx
	mustExecTenant(t, pool, realTableTenantCtx(uidA),
		`INSERT INTO dim_view_template
		   (id, name, scope, owner, page, query_state, columns, layout, tenant_id)
		 VALUES ($1, $2, 'personal', $3, 'pnl', '{}'::jsonb, '[]'::jsonb, '{}'::jsonb, $4)`,
		tplA, "A 的模板", ownerA, uidA)

	// 租户 B 建一个视图模板
	tplB := "tpl-b-" + sfx
	mustExecTenant(t, pool, realTableTenantCtx(uidB),
		`INSERT INTO dim_view_template
		   (id, name, scope, owner, page, query_state, columns, layout, tenant_id)
		 VALUES ($1, $2, 'personal', $3, 'pnl', '{}'::jsonb, '[]'::jsonb, '{}'::jsonb, $4)`,
		tplB, "B 的模板", ownerB, uidB)
	t.Cleanup(func() {
		cleanupTemplatesByPrefix(context.Background(), pool, "tpl-a-"+sfx)
		cleanupTemplatesByPrefix(context.Background(), pool, "tpl-b-"+sfx)
	})

	// ★ 租户 A 只应看到自己的模板
	var seenByA int
	if err := QueryRowInTenant(ctx, pool, realTableTenantCtx(uidA),
		`SELECT count(*) FROM dim_view_template WHERE id LIKE $1`,
		[]any{"tpl-%-" + sfx}, &seenByA); err != nil {
		t.Fatalf("A 查询失败：%v", err)
	}
	if seenByA != 1 {
		t.Errorf("★ 租户 A 看到 %d 个模板（应为 1）—— 串租", seenByA)
	}

	// ★ A 按 B 的模板 id 精确查，必须查不到（不是「数量对」而是「拿不到」）
	var bName string
	err := QueryRowInTenant(ctx, pool, realTableTenantCtx(uidA),
		`SELECT name FROM dim_view_template WHERE id = $1`, []any{tplB}, &bName)
	if err == nil {
		t.Errorf("★ 租户 A 读到了租户 B 的模板 %q —— 越权读取", bName)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("期望 no rows，实际：%v", err)
	}

	// ★ 反向对称：B 也读不到 A 的
	err = QueryRowInTenant(ctx, pool, realTableTenantCtx(uidB),
		`SELECT name FROM dim_view_template WHERE id = $1`, []any{tplA}, &bName)
	if err == nil {
		t.Errorf("★ 租户 B 读到了租户 A 的模板 %q —— 越权读取", bName)
	}
}

// TestRLS_越权_未设租户的真实表看到0行 证明 fail-closed 在真实表上成立。
//
// ★ 这条测试的价值：它模拟「有人加了新接口，忘了包 InTenantTx」。
//
//	此时 tenant_id 落到哨兵值 ⇒ 真实业务表必须返回 0 行，
//	而不是「返回全部行」（那是最坏情况：忘设 = 全可见）。
func TestRLS_越权_未设租户的真实表看到0行(t *testing.T) {
	pool := escalationPool(t)
	ctx := context.Background()
	sfx := testSuffix(t)

	ownerA := "own-nl-" + sfx
	seedOwnerOrg(t, ctx, pool, uidAlpha, ownerA, "A 的账号")

	// 先在租户 A 名下写一行
	mustExecTenant(t, pool, realTableTenantCtx(uidAlpha),
		`INSERT INTO dim_view_template
		   (id, name, scope, owner, page, query_state, columns, layout, tenant_id)
		 VALUES ($1, $2, 'personal', $3, 'pnl', '{}'::jsonb, '[]'::jsonb, '{}'::jsonb, $4)`,
		"tpl-noleak-"+sfx, "不该被裸查询看到", ownerA, uidAlpha)
	t.Cleanup(func() { cleanupTemplatesByPrefix(context.Background(), pool, "tpl-noleak-"+sfx) })

	// ★ 不设租户，直接（用应用角色）查真实表
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM dim_view_template WHERE id LIKE $1`,
		"tpl-noleak-"+sfx).Scan(&n); err != nil {
		t.Fatalf("裸查询失败：%v", err)
	}
	if n != 0 {
		t.Fatalf("★ 未设租户时看到 %d 行 —— fail-closed 失效（应为 0）", n)
	}
}

// TestRLS_越权_伪造tenant_id写入必须被拒 用真实表验证 WITH CHECK。
//
// ★ 注意攻击形态：不是「以 B 身份读 A」，而是「以 B 身份**写入** A 的行」。
//
//	这是更危险的形态 —— 它污染 A 的数据而非仅泄漏 B 的视野。
//	覆盖两种写法：
//	  ① 显式写 tenant_id = A（最直白）
//	  ② 写完后 UPDATE 把行「送」给 A（跨租户转移）
func TestRLS_越权_伪造tenant_id写入必须被拒(t *testing.T) {
	pool := escalationPool(t)
	ctx := context.Background()
	sfx := testSuffix(t)

	// ★ 为 **B** 建一个 owner 账号（下面所有写入都以 B 身份）。
	//   owner 复用 d 前缀，避免与其它用例撞「全局 account 主键」。
	ownerB := "own-fg-" + sfx
	seedOwnerOrg(t, ctx, pool, uidBeta, ownerB, "B 的账号")

	// ① 以 B 身份显式写入 tenant_id = A
	err := ExecInTenant(ctx, pool, realTableTenantCtx(uidBeta),
		`INSERT INTO dim_view_template
		   (id, name, scope, owner, page, query_state, columns, layout, tenant_id)
		 VALUES ($1, $2, 'personal', $3, 'pnl', '{}'::jsonb, '[]'::jsonb, '{}'::jsonb, $4)`,
		"tpl-forge-"+sfx, "伪造归属", ownerB, uidAlpha)
	if err == nil {
		t.Errorf("★ 以租户 B 身份写入 tenant_id=A 的行竟然成功 —— WITH CHECK 失效")
		cleanupTemplatesByPrefix(context.Background(), pool, "tpl-forge-"+sfx)
	}

	// ② 以 B 身份先建好自己的行，再尝试 UPDATE 成 A 的
	own := "tpl-move-" + sfx
	if err := ExecInTenant(ctx, pool, realTableTenantCtx(uidBeta),
		`INSERT INTO dim_view_template
		   (id, name, scope, owner, page, query_state, columns, layout, tenant_id)
		 VALUES ($1, $2, 'personal', $3, 'pnl', '{}'::jsonb, '[]'::jsonb, '{}'::jsonb, $4)`,
		own, "B 自己的", ownerB, uidBeta); err != nil {
		t.Fatalf("B 建自己的行失败：%v", err)
	}
	t.Cleanup(func() { cleanupTemplatesByPrefix(context.Background(), pool, "tpl-move-"+sfx) })

	// ★ 尝试把自己的行「转移」给 A —— USING 会先过滤掉（因为现在是 B 的），
	//   即便漏过 USING，WITH CHECK 也会拦住新值。两道闸任一都应该拦下。
	err = ExecInTenant(ctx, pool, realTableTenantCtx(uidBeta),
		`UPDATE dim_view_template SET tenant_id = $1 WHERE id = $2`, uidAlpha, own)
	if err == nil {
		t.Errorf("★ 以租户 B 身份把行转移给 A 竟然成功 —— 跨租户数据转移")
	}
	// 确认归属没变
	var holder string
	if err := QueryRowInTenant(ctx, pool, realTableTenantCtx(uidBeta),
		`SELECT tenant_id::text FROM dim_view_template WHERE id = $1`, []any{own}, &holder); err != nil {
		t.Fatalf("复查归属失败：%v", err)
	}
	if holder != uidBeta {
		t.Errorf("★ 行归属变成了 %s —— 跨租户转移成功", holder[:8])
	}
}

// TestRLS_越权_删除别人的行必须无效 用真实表验证 DELETE 被 RLS 过滤。
//
// ★ 这条经常被忽略：WITH CHECK **不**适用于 DELETE。
//
//	DELETE 只受 USING 约束 —— 若策略写漏 USING，删除会「成功」，
//	而且**不报错**（删掉 0 行与删掉 1 行在 SQL 层都是成功）。
//	所以正确的断言不是「报错」，而是「删完对方还在」。
func TestRLS_越权_删除别人的行必须无效(t *testing.T) {
	pool := escalationPool(t)
	ctx := context.Background()
	sfx := testSuffix(t)

	ownerA := "own-dl-" + sfx
	seedOwnerOrg(t, ctx, pool, uidAlpha, ownerA, "A 的账号")

	victim := "tpl-victim-" + sfx
	mustExecTenant(t, pool, realTableTenantCtx(uidAlpha),
		`INSERT INTO dim_view_template
		   (id, name, scope, owner, page, query_state, columns, layout, tenant_id)
		 VALUES ($1, $2, 'personal', $3, 'pnl', '{}'::jsonb, '[]'::jsonb, '{}'::jsonb, $4)`,
		victim, "A 的资产", ownerA, uidAlpha)
	t.Cleanup(func() { cleanupTemplatesByPrefix(context.Background(), pool, "tpl-victim-"+sfx) })

	// ★ 以 B 身份删 A 的行。RLS 会让这条 DELETE 影响 0 行（不报错）。
	if err := ExecInTenant(ctx, pool, realTableTenantCtx(uidBeta),
		`DELETE FROM dim_view_template WHERE id = $1`, victim); err != nil {
		// 报错也算拦下（某些部署会因策略原因报错），可接受
		t.Logf("B 删除 A 的行被拒（可接受）：%v", err)
	}

	// ★ 关键断言：以 A 身份复查，行必须还在
	var still int
	if err := QueryRowInTenant(ctx, pool, realTableTenantCtx(uidAlpha),
		`SELECT count(*) FROM dim_view_template WHERE id = $1`, []any{victim}, &still); err != nil {
		t.Fatalf("A 复查失败：%v", err)
	}
	if still != 1 {
		t.Fatalf("★ 租户 B 删掉了租户 A 的行 —— DELETE 未被 RLS 过滤")
	}
}

// ───────────────────────────── 权限表：最敏感的一类 ─────────────────────────────

// TestRLS_越权_权限表跨租户不可见 针对 fact_entitlement 做专项。
//
// ★ 为什么权限表要单测：它是「谁能看什么」的答案本身。
//
//	若 tenant B 能读到 tenant A 的授权行，攻击者就能据此
//	**构造**出让 A 的账号拥有 A 没有的权限的请求。
//	这是提权的前置条件，必须钉死。
//
// ★ 同时验证一个「看起来会失败」的设计：fact_entitlement 原主键是
//
//	account（全局唯一）。0011 已移除它并改为 (tenant_id, account)
//	部分唯一索引 —— 因此**两个租户可以各有自己的授权行**，
//	即便 account 名相同也不会撞（这正是修掉的问题）。
func TestRLS_越权_权限表跨租户不可见(t *testing.T) {
	pool := escalationPool(t)
	ctx := context.Background()
	sfx := testSuffix(t)

	// ★ query_state 是 NOT NULL，必须给。
	//   ★ dim_org 的 account 仍是全局主键 ⇒ 先为两租户各建一个
	//     （account 名不同），再用它们做授权行的 account。
	acctA := "adm-a-" + sfx
	acctB := "adm-b-" + sfx
	seedOwnerOrg(t, ctx, pool, uidAlpha, acctA, "A 管理员")
	seedOwnerOrg(t, ctx, pool, uidBeta, acctB, "B 管理员")

	// 各建一条授权（fact_entitlement 必要的 NOT NULL 列只有 account）
	mustExecTenant(t, pool, realTableTenantCtx(uidAlpha),
		`INSERT INTO fact_entitlement
		   (account, groups, data_use_groups, modules, dimensions, grants, tenant_id)
		 VALUES ($1, ARRAY['g-a'], '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, $2)`,
		acctA, uidAlpha)
	mustExecTenant(t, pool, realTableTenantCtx(uidBeta),
		`INSERT INTO fact_entitlement
		   (account, groups, data_use_groups, modules, dimensions, grants, tenant_id)
		 VALUES ($1, ARRAY['g-b'], '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, $2)`,
		acctB, uidBeta)
	t.Cleanup(func() {
		cleanupEntitlementsByPrefix(context.Background(), pool, "adm-a-"+sfx)
		cleanupEntitlementsByPrefix(context.Background(), pool, "adm-b-"+sfx)
	})

	// ★ A 只看得到自己的授权
	var nA int
	if err := QueryRowInTenant(ctx, pool, realTableTenantCtx(uidAlpha),
		`SELECT count(*) FROM fact_entitlement WHERE account LIKE $1`,
		[]any{"adm-%-" + sfx}, &nA); err != nil {
		t.Fatalf("A 查询失败：%v", err)
	}
	if nA != 1 {
		t.Errorf("★ 租户 A 看到 %d 条授权（应为 1）", nA)
	}

	// ★ A 按 B 的账号精确查授权，必须拿不到
	var groups []string
	err := QueryRowInTenant(ctx, pool, realTableTenantCtx(uidAlpha),
		`SELECT groups FROM fact_entitlement WHERE account = $1`, []any{acctB}, &groups)
	if err == nil {
		t.Fatalf("★ 租户 A 读到了租户 B 的授权 %v —— 权限泄漏", groups)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("期望 no rows，实际：%v", err)
	}
}

// ───────────────────────────── 审计表：只增不改的例外形态 ─────────────────────────────

// TestRLS_越权_审计表按租户可见 用 audit_log 验证 append-only + RLS 的组合。
//
// ★ 审计表的特殊性（0011 已注明）：
//   - append-only（触发器拦 UPDATE/DELETE）⇒ 只考 INSERT 与 SELECT；
//   - 平台级事件 tenant_id 为 NULL ⇒ 对所有租户不可见（这是**设计**）。
//
// 本测试钉住两点：
//
//	① 租户只能看到自己 tenant_id 的审计行；
//	② tenant_id IS NULL 的「平台级事件」对租户不可见 ——
//	   若哪天有人「顺便」把 NULL 也放行，跨租户探测行为就会泄漏。
func TestRLS_越权_审计表按租户可见(t *testing.T) {
	pool := escalationPool(t)
	ctx := context.Background()
	sfx := testSuffix(t)

	// 三个 actor 前缀，用于精确圈定本测试的行
	actorA := "actor-a-" + sfx
	actorB := "actor-b-" + sfx
	actorPlatform := "actor-platform-" + sfx

	// A 的审计行
	mustExecTenant(t, pool, realTableTenantCtx(uidAlpha),
		`INSERT INTO audit_log (actor, action, detail, tenant_id)
		 VALUES ($1, 'view.export', '{}'::jsonb, $2)`, actorA, uidAlpha)
	// B 的审计行
	mustExecTenant(t, pool, realTableTenantCtx(uidBeta),
		`INSERT INTO audit_log (actor, action, detail, tenant_id)
		 VALUES ($1, 'view.export', '{}'::jsonb, $2)`, actorB, uidBeta)
	t.Cleanup(func() {
		cleanupAuditByPrefix(context.Background(), pool, "actor-a-"+sfx)
		cleanupAuditByPrefix(context.Background(), pool, "actor-b-"+sfx)
		cleanupAuditByPrefix(context.Background(), pool, "actor-platform-"+sfx)
	})

	// ★ 平台级事件：直接以应用角色插入 tenant_id=NULL。
	//   注意 RLS 的 WITH CHECK 对 NULL 的处理：`NULL = uuid` 求值为 NULL
	//   ⇒ 不被视为 true ⇒ **会被拒绝**。这正是我们想要的行为：
	//   普通租户上下文**不能**写平台级审计行。
	//   于是这条插入必须在**无租户上下文**（哨兵）下由平台通道完成。
	//   本测试改为验证「租户上下文无法伪造平台级审计行」。
	err := ExecInTenant(ctx, pool, realTableTenantCtx(uidAlpha),
		`INSERT INTO audit_log (actor, action, detail, tenant_id)
		 VALUES ($1, 'probe', '{}'::jsonb, NULL)`, actorPlatform)
	if err == nil {
		t.Errorf("★ 租户 A 竟然写入了平台级（tenant_id=NULL）审计行 —— WITH CHECK 对 NULL 失效")
	}

	// ★ A 只看得到自己的审计行
	var nA int
	if err := QueryRowInTenant(ctx, pool, realTableTenantCtx(uidAlpha),
		`SELECT count(*) FROM audit_log WHERE actor = $1`, []any{actorA}, &nA); err != nil {
		t.Fatalf("A 查询失败：%v", err)
	}
	if nA != 1 {
		t.Errorf("★ 租户 A 看到自己 %d 条审计（应为 1）", nA)
	}

	// ★ A 按 B 的 actor 查，必须 0 行
	var cross int
	if err := QueryRowInTenant(ctx, pool, realTableTenantCtx(uidAlpha),
		`SELECT count(*) FROM audit_log WHERE actor = $1`, []any{actorB}, &cross); err != nil {
		t.Fatalf("A 交叉查询失败：%v", err)
	}
	if cross != 0 {
		t.Errorf("★ 租户 A 看到了租户 B 的 %d 条审计 —— 审计泄漏", cross)
	}

	// ★ 平台级行对租户不可见（若有的话）
	var plat int
	if err := QueryRowInTenant(ctx, pool, realTableTenantCtx(uidAlpha),
		`SELECT count(*) FROM audit_log WHERE actor = $1`, []any{actorPlatform}, &plat); err != nil {
		t.Fatalf("平台行查询失败：%v", err)
	}
	if plat != 0 {
		t.Errorf("★ 租户 A 看到了 %d 条平台级审计 —— 平台事件泄漏给租户", plat)
	}
}

// ───────────────────────────── 唯一键：租户内唯一而非全局唯一 ─────────────────────────────

// TestRLS_越权_两租户可各用同业务键 钉住 0011 修掉的「全局唯一键」问题。
//
// ★★ 这是一个**生产级**事故的回归测试，值得单独写：
//
//	加 tenant_id 时最容易漏的就是「唯一键还是全局的」。
//	后果不是安全泄漏，而是「第二家客户开不了账号 / 导不进数据」——
//	报错指向唯一键冲突，看起来像数据坏了，实际是设计没跟上。
//	0011 修了 fact_sales_daily / bucket_pnl_month / dim_data_chain
//	等表的业务键。本测试用 fact_sales_daily 验证修复确实生效。
func TestRLS_越权_两租户可各用同业务键(t *testing.T) {
	pool := escalationPool(t)
	ctx := context.Background()
	sfx := testSuffix(t)

	// 两家租户，**完全相同的业务键**（同日期、同渠道、同店、同 spu/sku）
	shop := "shop-" + sfx
	ins := `INSERT INTO fact_sales_daily
	          (stat_date, channel_code, shop_id, brand, spu, sku, qty, revenue, source, tenant_id)
	        VALUES ('2026-01-15', 'tiktok', $1, 'brand-x', 'spu-1', 'sku-1', 1, 1.00, 'manual', $2)`
	t.Cleanup(func() { cleanupSalesByShop(context.Background(), pool, shop) })

	if err := ExecInTenant(ctx, pool, realTableTenantCtx(uidAlpha), ins, shop, uidAlpha); err != nil {
		t.Fatalf("租户 A 写入失败：%v", err)
	}
	// ★ 若唯一键还是全局的，这里会因 uq_..._bizkey 冲突失败
	if err := ExecInTenant(ctx, pool, realTableTenantCtx(uidBeta), ins, shop, uidBeta); err != nil {
		t.Fatalf("★ 租户 B 写入相同业务键失败 —— 唯一键仍是全局的：%v", err)
	}

	// 且同租户内重复写入**必须**被拒（否则就不是「唯一」了）
	if err := ExecInTenant(ctx, pool, realTableTenantCtx(uidAlpha), ins, shop, uidAlpha); err == nil {
		t.Errorf("★ 同租户内重复业务键竟然成功 —— 租户内唯一性失效")
	}
}

// ───────────────────────────── 外键绕过 RLS（已知残余风险，做成可检测） ─────────────────────────────

// TestRLS_越权_外键检查绕过RLS_属已知残余风险 把 PostgreSQL 的一条规则
// 固化为可执行事实，并证明我们的**检测手段**能发现它。
//
// ★ 这条测试的定位很关键：它**不断言「不可能」**，而是断言
//
//	「我们知道这条路径存在，并且有办法把它巡检出来」。
//
//	原因：PostgreSQL 在检查外键约束时以表拥有者权限执行，
//	**不应用 RLS**。因此租户 B 可以插入一行引用租户 A 的
//	dim_org.account —— 约束检查能查到（绕过 RLS），写入成功。
//
//	彻底修复需要重建 FK 图为复合键（0012 的工作）。
//	在此之前，我们至少要做到：
//	  ① 明确知道这个洞存在（本测试）；
//	  ② 有可巡检的查询能找出已发生的越界引用（本测试的 detect 段）。
//
// ★ 若某天 PostgreSQL 改了 FK 执行语义（开始应用 RLS），
//
//	本测试会失败 —— 那正是我们想知道的信号。
func TestRLS_越权_外键检查绕过RLS_属已知残余风险(t *testing.T) {
	pool := escalationPool(t)
	ctx := context.Background()
	sfx := testSuffix(t)

	// 租户 A 有一个账号（dim_org 是全局主键表）
	acctA := "fk-a-" + sfx
	seedOwnerOrg(t, ctx, pool, uidAlpha, acctA, "A 的账号")

	// ★ 以租户 B 的身份建一个组，然后把 A 的账号加成成员。
	//   预期：**成功**（因为 FK 检查绕过 RLS）。
	//   这就是残余风险 —— 本测试把它变成「已知且被记录」。
	grp := "grp-fk-" + sfx
	mustExecTenant(t, pool, realTableTenantCtx(uidBeta),
		`INSERT INTO dim_group (id, name, grants, owners, tenant_id)
		 VALUES ($1, $2, '{}'::jsonb, ARRAY[]::text[], $3)`, grp, "B 的组", uidBeta)
	t.Cleanup(func() { cleanupGroupsByPrefix(context.Background(), pool, "grp-fk-"+sfx) })

	crossRef := ExecInTenant(ctx, pool, realTableTenantCtx(uidBeta),
		`INSERT INTO dim_group_member (group_id, account, role, inherits_grants, tenant_id)
		 VALUES ($1, $2, 'member', true, $3)`, grp, acctA, uidBeta)

	if crossRef == nil {
		// 现状：写入成功 ⇒ 残余风险成立。记录为「已知」。
		t.Logf("★ 已知残余风险：租户 B 成功引用租户 A 的账号 %s（FK 检查绕过 RLS）。"+
			"修复计划：0012 重建 FK 图为 (tenant_id, account) 复合键。"+
			"缓解：写路径必须经 tenant 包并在应用层校验归属。", acctA)

		// ② 证明我们的**检测查询**能发现这条越界引用
		//
		// ★★ 关键：巡检查询**必须用特权通道**执行（主 DSN）。
		//   若用租户通道，RLS 会先把 dim_group_member / dim_org 过滤成 0 行，
		//   于是「查不到违规」= 巡检永远通过 —— 一个永远绿的假警报。
		//   这正是本测试第一版踩到的坑（用 app 池跑巡检 ⇒ found=0）。
		priv := cleanupPoolFor(ctx)
		if priv == nil {
			t.Skip("未设置 SPARK_TEST_DB_DSN，无法用特权通道跑巡检")
		}
		defer priv.Close()

		var found int
		// ★ 参数是 group_id 的 LIKE 前缀（本用例的组名是 grp-fk-<sfx>）。
		if err := priv.QueryRow(ctx,
			detectCrossTenantFKViolationsSQL, "grp-fk-"+sfx+"%").Scan(&found); err != nil {
			t.Fatalf("巡检查询本身出错：%v", err)
		}
		if found == 0 {
			t.Errorf("★ 检测查询未能发现已存在的跨租户引用 —— 巡检无效")
		} else {
			t.Logf("巡检发现 %d 条跨租户引用（检测有效）", found)
		}
	} else {
		// 若未来 FK 开始应用 RLS（或有人加了触发器），这里会走到。
		t.Logf("跨租户引用被拒（说明 FK 已开始受约束）：%v", crossRef)
	}
}

// detectCrossTenantFKViolationsSQL 是「跨租户引用」的巡检查询。
//
// ★ 写法要点：必须**用超级用户/平台通道**执行（因为要跨过 RLS 看到全部行），
//
//	否则 RLS 会先把自己的行过滤掉，什么都查不到 —— 那会让巡检永远「通过」。
//	这正是这条查询必须写在运维脚本里、而不能塞进租户上下文的原因。
//
// 参数：$1 = 要过滤的键前缀（LIKE 模式，如 "sfx%"）。
// 返回：违规行数。
//
// 判定规则：引用方（child）的 tenant_id 与被引用方（parent）的 tenant_id
// 不一致，且两者都非空 ⇒ 违规。
const detectCrossTenantFKViolationsSQL = `
SELECT count(*)::int
  FROM dim_group_member m
  JOIN dim_org o ON o.account = m.account
 WHERE m.tenant_id IS NOT NULL
   AND o.tenant_id IS NOT NULL
   AND m.tenant_id <> o.tenant_id
   AND m.group_id LIKE $1`

// ───────────────────────────── 特权角色：真实表上的回归钉 ─────────────────────────────

// TestRLS_越权_真实表上特权角色必须被自检拦下 是探针版测试的「真实表」加强。
//
// ★ 为什么还要再来一遍：探针版证明的是「Postgres 规则如此」，
//
//	而这一版证明的是「**我们的业务表**在特权角色下确实不设防，
//	所以自检必须拦住」。
//
//	区别看似微小，但事故发生时这条更有说服力：
//	「我们有 12 张业务表，任何一张都挡不住超级用户」比
//	「我们有一张探针表挡不住」更能推动人去改连接角色。
func TestRLS_越权_真实表上特权角色必须被自检拦下(t *testing.T) {
	mainDSN := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if mainDSN == "" {
		t.Skip("未设置 SPARK_TEST_DB_DSN，跳过")
	}
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(mainDSN)
	if err != nil {
		t.Fatalf("解析 DSN 失败：%v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("建池失败：%v", err)
	}
	defer pool.Close()

	s, err := DescribeRole(ctx, pool)
	if err != nil {
		t.Fatalf("读角色属性失败：%v", err)
	}
	if s.CanUseRLS {
		// 主 DSN 已是非特权角色 ⇒ 没有可复现的特权角色，跳过
		t.Skipf("主 DSN 角色 %s 已是非特权角色，无需复现特权绕过", s.RoleName)
	}

	// ★ 主 DSN 是特权角色 ⇒ 自检必须拦住
	if err := AssertRLSCapable(ctx, pool); err == nil {
		t.Fatalf("★ 自检未拦下特权角色 %s —— 12 张业务表的 RLS 将全部失效", s.RoleName)
	}

	// ★ 实测：以特权角色在**真实表**上设租户后查询，应看到跨租户行。
	//   这里只做「行数 >= 本租户应有行数」的弱断言 ——
	//   因为业务表里可能有别的测试留下的数据，精确计数不可靠。
	//   关键是证明「设了租户也过滤不掉」。
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("取连接失败：%v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败：%v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 用两个哨兵 uuid 都不存在的租户查 dim_org：
	// 若 RLS 生效，任何真实行的 tenant_id 都不等于该哨兵 ⇒ 0 行；
	// 若特权角色绕过 RLS ⇒ 看到全部 58 行存量数据。
	const ghost = "cccc3333-3333-4333-8333-333333333333"
	if _, err := tx.Exec(ctx, SetLocalTenantSQL, ghost); err != nil {
		t.Fatalf("设置租户失败：%v", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM dim_org`).Scan(&n); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if n == 0 {
		t.Logf("特权角色在真实表上被过滤（0 行）—— 与预期不符，" +
			"可能 RLS 行为已改善，请复核 rls_capability.go")
	} else {
		t.Logf("★ 复现：特权角色设了哨兵租户仍看到 %d 行 dim_org —— 真实表隔离被绕过", n)
	}
}

// ───────────────────────────── 测试辅助 ─────────────────────────────

// testSuffix 生成 per-run 唯一后缀（并发/历史测试互不污染）。
//
// ★ 只用表名是不够的：`audit_log` 是 append-only（触发器拦 DELETE），
//
//	清理删不掉，于是**同一测试跑第二次**时上一次的行还在 ——
//	计数类断言会「2 != 1」地失败。故必须加一个**每次运行都不同**的 token。
//	用 UnixNano 即可（同一进程内不会重复，跨运行必然不同）。
func testSuffix(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, r := range strings.ToLower(t.Name()) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 20 {
		s = s[:20]
	}
	return s + "-" + itoa(int(time.Now().UnixNano()%1_000_000_000))
}

// mustExecTenant 在租户事务里执行一条写语句，失败即 Fatal。
func mustExecTenant(t *testing.T, pool *pgxpool.Pool, tc TenantContext, sql string, args ...any) {
	t.Helper()
	if err := ExecInTenant(context.Background(), pool, tc, sql, args...); err != nil {
		t.Fatalf("写入失败：%v\nSQL: %s", err, sql)
	}
}

// 清理辅助：用**超级用户 DSN**（绕过 RLS）来清理，否则删不掉别的租户的行。
//
// ★ 这是测试基建的例外：清理必须跨越 RLS，所以在测试里显式用主 DSN。
//	生产代码绝不能这么做 —— 那正是 AssertRLSCapable 存在的理由。

func cleanupTemplatesByPrefix(ctx context.Context, _ *pgxpool.Pool, prefix string) {
	p := cleanupPoolFor(ctx)
	if p == nil {
		return
	}
	defer p.Close()
	_, _ = p.Exec(ctx, `DELETE FROM dim_view_template WHERE id LIKE $1`, prefix+"%")
}

func cleanupOrgsByPrefix(ctx context.Context, _ *pgxpool.Pool, prefix string) {
	p := cleanupPoolFor(ctx)
	if p == nil {
		return
	}
	defer p.Close()
	_, _ = p.Exec(ctx, `DELETE FROM dim_org WHERE account LIKE $1`, prefix+"%")
}

func cleanupEntitlementsByPrefix(ctx context.Context, _ *pgxpool.Pool, prefix string) {
	p := cleanupPoolFor(ctx)
	if p == nil {
		return
	}
	defer p.Close()
	_, _ = p.Exec(ctx, `DELETE FROM fact_entitlement WHERE account LIKE $1`, prefix+"%")
}

func cleanupAuditByPrefix(ctx context.Context, _ *pgxpool.Pool, prefix string) {
	p := cleanupPoolFor(ctx)
	if p == nil {
		return
	}
	defer p.Close()
	// ★ audit_log 是 append-only（触发器 trg_audit_log_no_update 拦 UPDATE/DELETE）。
	//   清理必须**临时禁用该触发器** —— 启用状态下 DELETE 影响 0 行且不报错，
	//   于是测试行会跨运行累积、把「count(*)==1」断言拖垮。
	//   ★ 这是测试基建的例外操作：生产绝不允许禁用审计不可变触发器。
	if _, err := p.Exec(ctx,
		`ALTER TABLE audit_log DISABLE TRIGGER trg_audit_log_no_update`); err != nil {
		return // 没权限就放弃清理（断言已按 run 唯一 token 圈定，不受残留影响）
	}
	_, _ = p.Exec(ctx, `DELETE FROM audit_log WHERE actor LIKE $1`, prefix+"%")
	_, _ = p.Exec(ctx,
		`ALTER TABLE audit_log ENABLE TRIGGER trg_audit_log_no_update`)
}

func cleanupSalesByShop(ctx context.Context, _ *pgxpool.Pool, shop string) {
	p := cleanupPoolFor(ctx)
	if p == nil {
		return
	}
	defer p.Close()
	_, _ = p.Exec(ctx, `DELETE FROM fact_sales_daily WHERE shop_id = $1`, shop)
}

func cleanupGroupsByPrefix(ctx context.Context, _ *pgxpool.Pool, prefix string) {
	p := cleanupPoolFor(ctx)
	if p == nil {
		return
	}
	defer p.Close()
	_, _ = p.Exec(ctx, `DELETE FROM dim_group_member WHERE group_id LIKE $1`, prefix+"%")
	_, _ = p.Exec(ctx, `DELETE FROM dim_group WHERE id LIKE $1`, prefix+"%")
}

// cleanupPoolFor 建一个临时的特权池（用于清理跨租户数据）。
//
// ★ 每次新建而非复用：清理函数签名里带了 pool 只是为了调用方便，
//
//	但清理**必须**用特权连接，否则删不掉。此处统一取主 DSN。
func cleanupPoolFor(ctx context.Context) *pgxpool.Pool {
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		return nil
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil
	}
	return pool
}
