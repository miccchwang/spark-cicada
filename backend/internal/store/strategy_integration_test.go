// strategy_integration_test.go —— M-STRATEGY 真库端到端测试。
//
// 运行方式：
//
//	SPARK_TEST_DB_DSN='postgres://user:pass@host:5432/spark_test?sslmode=disable' \
//	  go test ./internal/store/ -run Strategy -v
//
// ★ 本文件覆盖三条**只在真库里才成立**的断言：
//  1. 迁移 0009 的选型卡能被读出，且 current_key 与 items 自洽；
//  2. 决策后 current_key 变了、受影响桶变 STALE，且**同一事务**生效；
//  3. 「保持现状」不标记桶（这个只有真跑 SQL 才能验，替身测不出来）。
package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/strategy"
)

// runNonce 为**每次 go test 进程**生成一个唯一后缀。
//
// ★ 为什么仅靠 t.Name() 不够（这是第二轮才暴露的问题）：
//
//	audit_log 是 append-only，跨**多次 go test 运行**持续累积。
//	t.Name() 在同一次运行内唯一，但**跨运行完全相同** ——
//	所以「历史条数」断言在第一次跑绿、第二次跑红，且每次多加一条。
//
//	用进程启动时间 + PID 做 nonce，让每一轮测试都住进自己的
//	命名空间：无论跑多少次、无论库里积了多少历史，断言只看本轮的记录。
//	这是 audit_log「不可清空」前提下的唯一正解 ——
//	不能靠 DELETE 清理（触发器会拒），就必须靠**读侧隔离**。
var runNonce = fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())

// cardID 为每个测试生成**本轮运行唯一**的选型卡 id。
func cardID(t *testing.T) string {
	t.Helper()
	// 测试名可能含中文与斜杠（子测试），替换掉以保证是合法 id
	safe := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	return fmt.Sprintf("strategy.choice.%s.%s", safe, runNonce)
}

// seedChoice 往 registry_rule_set 插一张测试用选型卡。
//
// ★ 元数据（title/context/blocking/affected_buckets）写在 scope 里，
//
//	与 0009 迁移的种子形状完全一致 —— 测试用的夹具必须与真实种子同形，
//	否则「测试通过但迁移出来的卡用不了」这类问题测不出来。
func seedChoice(t *testing.T, s *StrategyStore, id, curKey string, items string, scopeExtra string) {
	t.Helper()
	ctx := context.Background()
	scope := `{"global":true,"current_key":"` + curKey + `","module":"module.report",` +
		`"blocking":true,"title":"测试卡","context":"用于集成测试的决策背景说明",` +
		`"affected_buckets":["pnl_month"]` + scopeExtra + `}`
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO registry_rule_set (id, version, scope, items)
		VALUES ($1, 1, $2::jsonb, $3::jsonb)
		ON CONFLICT (id, version) WHERE tenant_id IS NULL
		DO UPDATE SET scope=EXCLUDED.scope, items=EXCLUDED.items`,
		id, scope, items); err != nil {
		t.Fatalf("种子选型卡 %s 失败：%v", id, err)
	}
}

const testItems = `[
  {"key":"A","label":"A","description":"d","expected_effect":"不变","risk":"low","reversible":true},
  {"key":"B","label":"B","description":"d","expected_effect":"34.45% → 36.14%","risk":"medium","reversible":true}
]`

func TestStrategyIntegration_LoadChoice(t *testing.T) {
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()
	cid := cardID(t)

	seedChoice(t, s, cid, "A", testItems, "")

	c, err := s.LoadChoice(ctx, cid, false)
	if err != nil {
		t.Fatalf("读卡失败：%v", err)
	}
	if c.CurrentKey != "A" {
		t.Fatalf("current_key 应为 A，得 %q", c.CurrentKey)
	}
	if len(c.Options) != 2 {
		t.Fatalf("应有 2 个选项，得 %d", len(c.Options))
	}
	if c.Options[1].Risk != strategy.RiskMedium {
		t.Fatalf("risk 未正确解析：%q", c.Options[1].Risk)
	}
	if !c.Blocking {
		t.Fatal("blocking 应从 scope 读为 true")
	}
	if len(c.Impact.AffectedBuckets) != 1 || c.Impact.AffectedBuckets[0] != "pnl_month" {
		t.Fatalf("affected_buckets 未读出：%v", c.Impact.AffectedBuckets)
	}
	// ★ 读出来的卡必须自洽，否则 Resolve 会给出误导性错误
	if problems := c.Validate(); len(problems) > 0 {
		t.Fatalf("库里的卡不自洽：%v", problems)
	}
}

func TestStrategyIntegration_LoadChoice不存在(t *testing.T) {
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	if _, err := s.LoadChoice(context.Background(), "strategy.choice.nope", false); err == nil {
		t.Fatal("不存在的卡应报错")
	}
}

func TestStrategyIntegration_Decide改值并标记桶STALE(t *testing.T) {
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()
	cid := cardID(t)

	seedChoice(t, s, cid, "A", testItems, "")
	c, err := s.LoadChoice(ctx, cid, false)
	if err != nil {
		t.Fatalf("读卡失败：%v", err)
	}

	entry, err := s.Decide(ctx, c, "B", strategy.ActionChoose, "ceo", "sha256:snap1")
	if err != nil {
		t.Fatalf("决策失败：%v", err)
	}
	if entry.FromOption != "A" || entry.ToOption != "B" {
		t.Fatalf("历史方向错误：%s→%s", entry.FromOption, entry.ToOption)
	}
	if len(entry.AffectedBuckets) == 0 {
		t.Fatal("实质变更必须带 affectedBuckets")
	}

	// ① current_key 已更新
	c2, err := s.LoadChoice(ctx, cid, false)
	if err != nil {
		t.Fatalf("重读失败：%v", err)
	}
	if c2.CurrentKey != "B" {
		t.Fatalf("current_key 应为 B，得 %q", c2.CurrentKey)
	}
	// ★ 关键：scope 的其他键不能被覆盖掉
	if !c2.Blocking {
		t.Fatal("scope 的 blocking 被覆盖丢失 —— 更新 current_key 必须用 jsonb || 合并")
	}
	if len(c2.Impact.AffectedBuckets) == 0 {
		t.Fatal("scope 的 affected_buckets 被覆盖丢失")
	}

	// ② 历史可读
	h, err := s.History(ctx, cid)
	if err != nil {
		t.Fatalf("读历史失败：%v", err)
	}
	if len(h) != 1 {
		t.Fatalf("应有 1 条历史，得 %d", len(h))
	}
	if h[0].SnapshotHash != "sha256:snap1" {
		t.Fatalf("快照哈希未落库：%q", h[0].SnapshotHash)
	}

	// ③ 桶被标记 STALE
	var state string
	if err := s.pool.QueryRow(ctx,
		`SELECT state FROM registry_bucket WHERE id='pnl_month'`).Scan(&state); err != nil {
		t.Fatalf("读桶状态失败：%v", err)
	}
	if state != "STALE" {
		t.Fatalf("受影响桶应被标记 STALE，得 %q", state)
	}
}

func TestStrategyIntegration_KeepCurrent不标记桶(t *testing.T) {
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()
	cid := cardID(t)

	seedChoice(t, s, cid, "A", testItems, "")
	// 先把桶置回 FRESH，才能验证「没被改回 STALE」
	if _, err := s.pool.Exec(ctx,
		`UPDATE registry_bucket SET state='FRESH' WHERE id='pnl_month'`); err != nil {
		t.Fatalf("重置桶状态失败：%v", err)
	}
	c, _ := s.LoadChoice(ctx, cid, false)

	entry, err := s.Decide(ctx, c, "A", strategy.ActionKeepCurrent, "ceo", "sha256:s")
	if err != nil {
		t.Fatalf("决策失败：%v", err)
	}
	if entry.IsChange() {
		t.Fatal("keep_current 不应是实质变更")
	}
	if len(entry.AffectedBuckets) != 0 {
		t.Fatalf("keep_current 不应带桶：%v", entry.AffectedBuckets)
	}
	var state string
	_ = s.pool.QueryRow(ctx, `SELECT state FROM registry_bucket WHERE id='pnl_month'`).Scan(&state)
	if state != "FRESH" {
		t.Fatalf("keep_current 不应改动桶状态（否则「什么都没改却全在重算」），得 %q", state)
	}
	// ★ 但历史必须留痕
	h, _ := s.History(ctx, cid)
	if len(h) != 1 {
		t.Fatal("keep_current 也必须落历史")
	}
	if h[0].FromOption != h[0].ToOption {
		t.Fatalf("keep_current 的 from/to 应相同：%+v", h[0])
	}
}

func TestStrategyIntegration_回滚走通(t *testing.T) {
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()
	cid := cardID(t)

	seedChoice(t, s, cid, "A", testItems, "")
	c, _ := s.LoadChoice(ctx, cid, false)
	if _, err := s.Decide(ctx, c, "B", strategy.ActionChoose, "ceo", "sha256:s"); err != nil {
		t.Fatalf("决策失败：%v", err)
	}

	plan, err := s.PlanRollback(ctx, cid, true)
	if err != nil {
		t.Fatalf("生成回滚计划失败：%v", err)
	}
	if plan.FromOption != "B" || plan.ToOption != "A" {
		t.Fatalf("计划方向错误：%s→%s", plan.FromOption, plan.ToOption)
	}

	// 置 FRESH 以便验证回滚确实又标了 STALE
	_, _ = s.pool.Exec(ctx, `UPDATE registry_bucket SET state='FRESH' WHERE id='pnl_month'`)
	if _, err := s.Rollback(ctx, plan, "ceo"); err != nil {
		t.Fatalf("回滚失败：%v", err)
	}

	c2, _ := s.LoadChoice(ctx, cid, false)
	if c2.CurrentKey != "A" {
		t.Fatalf("回滚后当前值应为 A，得 %q", c2.CurrentKey)
	}
	var state string
	_ = s.pool.QueryRow(ctx, `SELECT state FROM registry_bucket WHERE id='pnl_month'`).Scan(&state)
	if state != "STALE" {
		t.Fatalf("回滚必须重算桶（只改参数不重算 = 静默错数），得 %q", state)
	}

	// 历史应有两条（decide + rollback），且回滚记录可被识别
	h, _ := s.History(ctx, cid)
	if len(h) < 2 {
		t.Fatalf("应有 ≥2 条历史，得 %d", len(h))
	}
}

func TestStrategyIntegration_回滚后再回滚回到B(t *testing.T) {
	// ★ 关键语义：回滚是「再记一条」，所以历史里现在多了一条 A→B 的反向记录。
	//   再点回滚应当回到 B（撤销上次回滚），而不是「无版本可回」。
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()
	cid := cardID(t)

	seedChoice(t, s, cid, "A", testItems, "")
	c, _ := s.LoadChoice(ctx, cid, false)
	_, _ = s.Decide(ctx, c, "B", strategy.ActionChoose, "ceo", "s1")
	p1, err := s.PlanRollback(ctx, cid, true)
	if err != nil {
		t.Fatalf("计划1失败：%v", err)
	}
	_, _ = s.Rollback(ctx, p1, "ceo")

	p2, err := s.PlanRollback(ctx, cid, true)
	if err != nil {
		t.Fatalf("计划2失败（回滚后应仍可回滚回去）：%v", err)
	}
	if p2.ToOption != "B" {
		t.Fatalf("二次回滚目标应为 B，得 %q", p2.ToOption)
	}
}

// ★ 回归：接口层留痕**不得**污染策略历史。
//
// 真实故障复现路径（本测试就是它）：handleDecide 除了调 store.Decide
// （写历史）之外，还调 RecordStrategyChange 落一条接口层审计。
// 若两者用同一个 action 名，History() 会把接口层那条也算作历史 ——
// 而它不带 reversible，读出来是 false，LastStable() 遇第一条不可逆即停，
// 于是**决策之后回滚永久不可达**（rollback/plan 恒 409）。
//
// 这里刻意按「接口层的真实组合」调用：store.Decide + RecordStrategyChange，
// 再断言回滚仍然可达。旧的 store 单测直接调 s.Decide、不走接口层，
// 所以没能发现这个缺陷 —— 这是本用例存在的意义。
func TestStrategyIntegration_接口层留痕不污染历史(t *testing.T) {
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()
	cid := cardID(t)

	seedChoice(t, s, cid, "A", testItems, "")
	c, _ := s.LoadChoice(ctx, cid, false)
	if _, err := s.Decide(ctx, c, "B", strategy.ActionChoose, "ceo", "s1"); err != nil {
		t.Fatalf("决策失败：%v", err)
	}

	// 模拟接口层的留痕（必须用 *_attempt 动作名）
	if err := s.RecordStrategyChange(ctx, "ceo", strategy.AuditActionDecideAttempt, cid, map[string]any{
		"choiceId": cid,
		"from":     "A",
		"to":       "B",
		"kind":     "choose",
	}); err != nil {
		t.Fatalf("接口层留痕失败：%v", err)
	}

	// ★ 历史里只能有 store 事务写的那一条决策
	h, err := s.History(ctx, cid)
	if err != nil {
		t.Fatalf("读历史失败：%v", err)
	}
	if len(h) != 1 {
		t.Fatalf("接口层留痕不得进入策略历史，期望 1 条，得 %d 条：%+v", len(h), h)
	}
	if !h[0].Reversible {
		t.Fatal("历史条目应保留 reversible=true（B 选项可逆）")
	}

	// ★ 最关键的一条：决策之后回滚必须仍然可达
	plan, err := s.PlanRollback(ctx, cid, true)
	if err != nil {
		t.Fatalf("决策后回滚必须可达（接口层留痕污染了历史才会失败）：%v", err)
	}
	if plan.FromOption != "B" || plan.ToOption != "A" {
		t.Fatalf("回滚方向错误：%s→%s", plan.FromOption, plan.ToOption)
	}
}

func TestStrategyIntegration_历史按时间正序(t *testing.T) {
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()
	cid := cardID(t)

	seedChoice(t, s, cid, "A", testItems, "")
	c, _ := s.LoadChoice(ctx, cid, false)
	_, _ = s.Decide(ctx, c, "B", strategy.ActionChoose, "ceo", "s1")
	c2, _ := s.LoadChoice(ctx, cid, false)
	_, _ = s.Decide(ctx, c2, "A", strategy.ActionChoose, "ceo", "s2")

	h, err := s.History(ctx, cid)
	if err != nil {
		t.Fatalf("读历史失败：%v", err)
	}
	if len(h) != 2 {
		t.Fatalf("应有 2 条，得 %d", len(h))
	}
	// ★ 同一事务的 now() 相同 ⇒ 必须有 id 兜底，否则顺序不确定
	if h[0].ToOption != "B" || h[1].ToOption != "A" {
		t.Fatalf("历史顺序错乱：%+v", h)
	}
}

func TestStrategyIntegration_ListPending(t *testing.T) {
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()
	cidA := cardID(t) + ".a"
	cidB := cardID(t) + ".b"

	seedChoice(t, s, cidA, "A", testItems, "")
	// 一张 blocking=false 的卡（同样用运行唯一 id，避免跨轮污染）
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO registry_rule_set (id, version, scope, items)
		VALUES ($1, 1,
		        '{"global":true,"current_key":"A","blocking":false,"title":"非阻塞卡","context":"背景"}'::jsonb, $2::jsonb)
		ON CONFLICT (id, version) WHERE tenant_id IS NULL
		DO UPDATE SET scope=EXCLUDED.scope, items=EXCLUDED.items`,
		cidB, testItems); err != nil {
		t.Fatalf("种子失败：%v", err)
	}

	list, err := s.ListPending(ctx, false)
	if err != nil {
		t.Fatalf("列卡失败：%v", err)
	}
	if len(list) < 2 {
		t.Fatalf("应至少列出 2 张测试卡，得 %d", len(list))
	}
	// ★ 断言排序**不变量**而不是「第一张是谁」：
	//   库里还有 0009 迁移的种子卡与其他测试的卡，
	//   断言「首项 = 我的卡」是脆弱的（依赖其他测试的副作用）。
	//   真正要守的规则是「一旦出现非 blocking，其后不得再出现 blocking」。
	seenNonBlocking := false
	for i, c := range list {
		if !c.Blocking {
			seenNonBlocking = true
			continue
		}
		if seenNonBlocking {
			t.Fatalf("排序违反 blocking 优先：位置 %d 的 %s 是 blocking 但前面已有非 blocking 卡",
				i, c.ID)
		}
	}
	// 两张测试卡都应在列表里，且相对顺序为 A（blocking）在 B（非 blocking）之前
	ia, ib := -1, -1
	for i, c := range list {
		if c.ID == cidA {
			ia = i
		}
		if c.ID == cidB {
			ib = i
		}
	}
	if ia < 0 || ib < 0 {
		t.Fatalf("测试卡未出现在列表中（cidA=%d cidB=%d）", ia, ib)
	}
	if ia > ib {
		t.Fatalf("blocking 卡应排在非 blocking 卡之前：cidA@%d cidB@%d", ia, ib)
	}
	// 排序确定性
	list2, _ := s.ListPending(ctx, false)
	if len(list) != len(list2) {
		t.Fatal("两次调用长度不一致")
	}
	for i := range list {
		if list[i].ID != list2[i].ID {
			t.Fatalf("排序不确定：位置 %d 从 %s 变成 %s", i, list[i].ID, list2[i].ID)
		}
	}
}

func TestStrategyIntegration_RecordStrategyChange(t *testing.T) {
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()

	if err := s.RecordStrategyChange(ctx, "ceo", strategy.AuditActionRollback,
		"strategy.choice.x", map[string]any{"from": "B", "to": "A"}); err != nil {
		t.Fatalf("写审计失败：%v", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE actor='ceo' AND action=$1`,
		strategy.AuditActionRollback).Scan(&n); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if n < 1 {
		t.Fatal("审计未落库")
	}
	// 缺 actor 必须拒绝（无 actor 的审计追不到人）
	if err := s.RecordStrategyChange(ctx, "", strategy.AuditActionDecide, "x", nil); err == nil {
		t.Fatal("缺 actor 的审计应被拒绝")
	}
}

func TestStrategyIntegration_坏卡不自洽时报错(t *testing.T) {
	// current_key 指向不存在的 item ⇒ toChoice 应产出不自洽的卡被拦下
	m, closeDB := openTestDB(t)
	defer closeDB()
	s := NewStrategyStore(m.Pool())
	ctx := context.Background()
	cid := cardID(t)

	seedChoice(t, s, cid, "ZZZ", testItems, "")
	c, err := s.LoadChoice(ctx, cid, false)
	if err != nil {
		t.Fatalf("读卡本身不应失败（交由 Validate 判定）：%v", err)
	}
	if problems := c.Validate(); len(problems) == 0 {
		t.Fatal("current_key 指向不存在的选项时，Validate 必须报错")
	}
	if _, err := c.Resolve(strategy.Action{Kind: strategy.ActionKeepCurrent}); err == nil {
		t.Fatal("坏卡必须无法解析决策")
	}
}
