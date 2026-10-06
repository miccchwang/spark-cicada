// integration_test.go —— M-COLLECT 真库端到端测试（临时仓库 / 断点续传 / 过闸）。
//
// 运行方式：
//
//	SPARK_TEST_DB_DSN='postgres://...' SPARK_REQUIRE_DB=1 \
//	  go test ./internal/collectstore/ -run Integration -v
//
// ★ 跳过 vs 失败纪律：与 store 包同口径 ——
//   本地无 DSN 跳过；CI（SPARK_REQUIRE_DB=1）无 DSN **直接失败**，绝不假绿。
//
// 本文件验证的是**只有在真库上才能证明**的保证：
//   * 幂等去重是 **UNIQUE 索引** 在生效（不是应用层「先查后插」）
//   * 数据行与游标**同事务**提交（游标推进与数据落库不可分离）
//   * 未过闸的数据**确实留在 staging 且为 PENDING**（真的没进投影层）
//   * 过闸后放行才删除 staging（用户已定的「过闸即清理」）
package collectstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/collect"
	"github.com/miccchwang/spark-cicada/backend/internal/db"
)

// openTestDB 连接测试库并应用迁移（含 0005 staging）。
func openTestDB(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" {
		if requireDB() {
			t.Fatal("★ 真库集成测试被要求必须运行（SPARK_REQUIRE_DB=1），但未设置 SPARK_TEST_DB_DSN。" +
				"闸门不允许静默跳过 —— 请起 Postgres 并注入 DSN。")
		}
		t.Skip("未配置 SPARK_TEST_DB_DSN —— 跳过真库集成测试（本地开发；CI 会设置）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
	return New(m.Pool()), m.Close
}

func requireDB() bool {
	v := strings.TrimSpace(os.Getenv("SPARK_REQUIRE_DB"))
	return v == "1" || strings.EqualFold(v, "true")
}

// newJob 建一个测试作业（唯一 id，避免多次运行互相污染）。
func newJob(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO collect_job (id, source_kind, source_ref, scope)
		VALUES ($1, 'api', 'test-connector', 'month=2026-09')
		ON CONFLICT (id) DO UPDATE SET
			cursor = NULL, pages_done = 0, rows_staged = 0,
			gear_index = 0, ok_streak = 0, status = 'PENDING'`, id)
	if err != nil {
		t.Fatalf("建测试作业失败：%v", err)
	}
	// 清理旧数据，保证断言从干净状态开始
	if _, err := s.pool.Exec(ctx, `DELETE FROM staging_record WHERE job_id = $1`, id); err != nil {
		t.Fatalf("清理 staging 失败：%v", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM collect_page WHERE job_id = $1`, id); err != nil {
		t.Fatalf("清理分页台账失败：%v", err)
	}
}

func rows(n int, prefix string) []StagedRow {
	out := make([]StagedRow, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, StagedRow{
			NaturalKey: prefix + "-" + string(rune('a'+i)),
			Payload:    []byte(`{"k":"v"}`),
		})
	}
	return out
}

// TestIntegration_CommitPagePersistsDataAndCursorAtomically 断言
// 「数据行 + 游标」同事务落地：提交后两者**同时**可见。
func TestIntegration_CommitPagePersistsDataAndCursorAtomically(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()

	const jobID = "job.it.atomic"
	newJob(t, s, jobID)

	st, err := s.LoadJob(ctx, jobID)
	if err != nil {
		t.Fatalf("LoadJob: %v", err)
	}
	n, err := s.CommitPage(ctx, st, 0, rows(3, "a"), "cursor-1")
	if err != nil {
		t.Fatalf("CommitPage: %v", err)
	}
	if n != 3 {
		t.Fatalf("首次应插入 3 行，实际 %d", n)
	}

	// 重新读回：游标与计数必须已持久化（这就是「断点」）
	back, err := s.LoadJob(ctx, jobID)
	if err != nil {
		t.Fatalf("LoadJob(回读): %v", err)
	}
	if back.Cursor != "cursor-1" {
		t.Errorf("游标未持久化：期望 cursor-1，实际 %q", back.Cursor)
	}
	if back.PagesDone != 1 || back.RowsStaged != 3 {
		t.Errorf("计数未持久化：期望 1 页/3 行，实际 %d 页/%d 行",
			back.PagesDone, back.RowsStaged)
	}
	if back.Status != collect.StatusRunning {
		t.Errorf("仍有下一页应保持 RUNNING，实际 %s", back.Status)
	}
}

// TestIntegration_ResumeFromCursorWithoutDuplicateCalls 断言断点续传：
// 从持久化游标继续，且**重复提交同一页不产生新行**（不重复调用上游的等价保证）。
func TestIntegration_ResumeFromCursorWithoutDuplicateCalls(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()

	const jobID = "job.it.resume"
	newJob(t, s, jobID)

	st, _ := s.LoadJob(ctx, jobID)
	if _, err := s.CommitPage(ctx, st, 0, rows(2, "p0"), "cur-1"); err != nil {
		t.Fatalf("第 1 页: %v", err)
	}

	// 模拟进程重启：丢弃内存态，仅凭 DB 恢复
	resumed, err := s.LoadJob(ctx, jobID)
	if err != nil {
		t.Fatalf("重启后 LoadJob: %v", err)
	}
	if resumed.Cursor != "cur-1" {
		t.Fatalf("重启后未恢复游标：%q", resumed.Cursor)
	}

	// ★ 关键：重复提交第 0 页（模拟「落表失败后重试」）
	n, err := s.CommitPage(ctx, resumed, 0, rows(2, "p0"), "cur-1")
	if err != nil {
		t.Fatalf("重复提交第 0 页: %v", err)
	}
	if n != 0 {
		t.Errorf("重复提交应插入 0 行（幂等去重），实际 %d —— "+
			"这会导致重复数据进入投影层", n)
	}

	// 从断点继续第 1 页
	if _, err := s.CommitPage(ctx, resumed, 1, rows(2, "p1"), "cur-2"); err != nil {
		t.Fatalf("续跑第 1 页: %v", err)
	}

	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM staging_record WHERE job_id = $1`, jobID).Scan(&total); err != nil {
		t.Fatalf("统计行数: %v", err)
	}
	if total != 4 {
		t.Errorf("两页共应 4 行（去重后），实际 %d", total)
	}

	// 页数必须计为 2（COUNT DISTINCT），而非 3（重复写过第 0 页）
	stats, err := s.CollectStats(ctx, jobID)
	if err != nil {
		t.Fatalf("CollectStats: %v", err)
	}
	if stats.ActualPages != 2 {
		t.Errorf("页数应去重为 2，实际 %d —— 重复计数会让守卫误判页数已闭合", stats.ActualPages)
	}
}

// TestIntegration_UniqueIndexRejectsDuplicateIdemKey 断言幂等**由数据库强制**，
// 而非应用层约定：绕过 Store 直接 INSERT 同幂等键也必须失败。
//
// 为什么必须测这一层：应用层「先 SELECT 再 INSERT」在并发下有竞态窗口；
// 只有 UNIQUE 索引才是真正可靠的保证。若这个索引被误删，应用层代码看起来
// 一切正常，重复数据会静默累积 —— 必须由本条守住。
func TestIntegration_UniqueIndexRejectsDuplicateIdemKey(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()

	const jobID = "job.it.unique"
	newJob(t, s, jobID)

	insert := func() error {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO staging_record (job_id, page_no, idem_key, natural_key, payload)
			VALUES ($1, 0, $2, 'dup', '{}'::jsonb)`, jobID, collect.IdemKey(jobID, "dup"))
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("首次插入应成功：%v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("★ 同幂等键二次插入竟然成功 —— UNIQUE(job_id, idem_key) 索引缺失或失效，" +
			"重复数据将静默进入投影层")
	}
}

// TestIntegration_GuardBlocksIncompleteFromProjection 断言：
// 数据不完整 ⇒ 守卫拒绝 ⇒ **放行被拒**（fail-closed），且数据留在 staging 标 PENDING。
//
// 这是用户要求「守卫验收数据完整性后过闸」的核心行为。
func TestIntegration_GuardBlocksIncompleteFromProjection(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()

	const jobID = "job.it.guardblock"
	newJob(t, s, jobID)

	st, _ := s.LoadJob(ctx, jobID)
	// 只取 1 页就停了（上游声明 3 页）—— 典型「中途断」
	if _, err := s.CommitPage(ctx, st, 0, rows(2, "x"), "cur-1"); err != nil {
		t.Fatalf("CommitPage: %v", err)
	}

	in := collect.GuardInput{
		ExpectedPages: 3, ActualPages: 1,
		ExpectedRows: 6, ActualRows: 2,
		CursorEnded: false, RequiredOK: true,
		Coverage: 1.0, HasCoverage: true,
	}
	v, guardID, err := s.EvaluateGuard(ctx, jobID, in)
	if err != nil {
		t.Fatalf("EvaluateGuard: %v", err)
	}
	if v.Passed {
		t.Fatal("★ 守卫放行了残缺数据 —— 残缺数据将进入经营报表")
	}

	// 未过闸的数据必须留在 staging，且状态为 PENDING（用户已定口径）
	var pending int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM staging_record WHERE job_id = $1 AND guard_state = 'PENDING'`,
		jobID).Scan(&pending); err != nil {
		t.Fatalf("统计 PENDING: %v", err)
	}
	if pending != 2 {
		t.Errorf("未过闸数据应留在 staging 且为 PENDING（2 行），实际 %d 行", pending)
	}

	// ★ 放行必须被拒（fail-closed）
	if _, err := s.ReleaseToProjection(ctx, jobID, guardID, "fact_sales_daily"); err == nil {
		t.Fatal("★ 守卫未通过却放行成功 —— fail-closed 失效，残缺数据会进投影层")
	}

	// 确认数据**没有**被误删（未过闸就删除等于丢数据）
	var left int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM staging_record WHERE job_id = $1`, jobID).Scan(&left); err != nil {
		t.Fatalf("统计剩余: %v", err)
	}
	if left != 2 {
		t.Errorf("放行被拒后数据应原样保留（2 行等待补齐），实际 %d 行", left)
	}
}

// TestIntegration_GuardPassesAndReleaseCleansStaging 断言完整数据能过闸，
// 且过闸放行后 staging 被清理（用户已定「过闸即清理」）。
func TestIntegration_GuardPassesAndReleaseCleansStaging(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()

	const jobID = "job.it.guardpass"
	newJob(t, s, jobID)

	st, _ := s.LoadJob(ctx, jobID)
	// 走到底：cursor 空表示上游没有下一页 ⇒ 状态 COMPLETE
	if _, err := s.CommitPage(ctx, st, 0, rows(2, "y"), ""); err != nil {
		t.Fatalf("CommitPage: %v", err)
	}

	stats, err := s.CollectStats(ctx, jobID)
	if err != nil {
		t.Fatalf("CollectStats: %v", err)
	}
	if !stats.CursorEnded {
		t.Fatal("游标耗尽后 CollectStats 应报 CursorEnded=true")
	}
	if stats.ActualPages != 1 || stats.ActualRows != 2 {
		t.Fatalf("统计异常：%d 页 / %d 行", stats.ActualPages, stats.ActualRows)
	}

	in := collect.GuardInput{
		ExpectedPages: 1, ActualPages: stats.ActualPages,
		ExpectedRows: 2, ActualRows: stats.ActualRows,
		CursorEnded: stats.CursorEnded, RequiredOK: true,
		Coverage: 1.0, HasCoverage: true,
	}
	v, guardID, err := s.EvaluateGuard(ctx, jobID, in)
	if err != nil {
		t.Fatalf("EvaluateGuard: %v", err)
	}
	if !v.Passed {
		t.Fatalf("完整数据应过闸，却被拒：%v", v.Reasons)
	}

	released, err := s.ReleaseToProjection(ctx, jobID, guardID, "fact_sales_daily")
	if err != nil {
		t.Fatalf("放行失败：%v", err)
	}
	if released != 2 {
		t.Errorf("放行行数应为 2，实际 %d", released)
	}

	// 过闸即清理
	var left int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM staging_record WHERE job_id = $1`, jobID).Scan(&left); err != nil {
		t.Fatalf("统计剩余: %v", err)
	}
	if left != 0 {
		t.Errorf("过闸放行后 staging 应被清理为 0 行，实际 %d 行", left)
	}

	// 放行台账必须留痕（审计：当时凭什么放行）
	var releasedRows int64
	if err := s.pool.QueryRow(ctx, `
		SELECT released_rows FROM staging_project_release
		WHERE job_id = $1 ORDER BY id DESC LIMIT 1`, jobID).Scan(&releasedRows); err != nil {
		t.Fatalf("读放行台账失败（放行事件必须可审计）：%v", err)
	}
	if releasedRows != 2 {
		t.Errorf("台账应记 2 行，实际 %d", releasedRows)
	}
}

// TestIntegration_GearTableMatchesHardcoded 断言 DB 档位表与代码档位表一致。
//
// 两处定义（迁移种子 vs collect.GearTable）若漂移，会出现
// 「代码以为退到 4s、DB 记的是 8s」这类难查问题。这里用真库把它钉死。
func TestIntegration_GearTableMatchesHardcoded(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()

	rowsG, err := s.pool.Query(context.Background(),
		`SELECT gear_index, delay_ms, recover_after FROM collect_rate_gear ORDER BY gear_index`)
	if err != nil {
		t.Fatalf("读档位表: %v", err)
	}
	defer rowsG.Close()

	seen := 0
	for rowsG.Next() {
		var idx, delayMs, recoverAfter int
		if err := rowsG.Scan(&idx, &delayMs, &recoverAfter); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if idx >= len(collect.GearTable) {
			t.Fatalf("DB 档位 %d 超出代码档位表长度 %d", idx, len(collect.GearTable))
		}
		want := collect.GearTable[idx]
		if want.Delay.Milliseconds() != int64(delayMs) {
			t.Errorf("档位 %d 间隔不一致：DB %dms vs 代码 %v", idx, delayMs, want.Delay)
		}
		if want.RecoverAfter != recoverAfter {
			t.Errorf("档位 %d recover_after 不一致：DB %d vs 代码 %d",
				idx, recoverAfter, want.RecoverAfter)
		}
		seen++
	}
	if seen != len(collect.GearTable) {
		t.Errorf("档位数量不一致：DB %d 档 vs 代码 %d 档", seen, len(collect.GearTable))
	}
}

// TestIntegration_EmptyNaturalKeyRejected 断言空业务键被拒。
//
// 空键会让幂等键退化为 (jobID, "")，同页所有行互相覆盖 —— 静默丢数据。
func TestIntegration_EmptyNaturalKeyRejected(t *testing.T) {
	s, closeFn := openTestDB(t)
	defer closeFn()
	ctx := context.Background()

	const jobID = "job.it.emptykey"
	newJob(t, s, jobID)

	st, _ := s.LoadJob(ctx, jobID)
	_, err := s.CommitPage(ctx, st, 0, []StagedRow{{NaturalKey: "", Payload: []byte(`{}`)}}, "c")
	if err == nil {
		t.Fatal("★ 空 natural_key 竟然落库成功 —— 幂等去重会退化成互相覆盖，静默丢数据")
	}
}

// TestIntegration_SuiteIsNotSilentlySkipped 自检：防止整包被静默跳过。
func TestIntegration_SuiteIsNotSilentlySkipped(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("SPARK_TEST_DB_DSN"))
	if dsn == "" && !requireDB() {
		t.Skip("无 DSN 且未要求真库 —— 本包真库断言本次未执行（本地开发属预期）")
	}
	if dsn != "" {
		t.Log("真库 DSN 已注入 —— M-COLLECT 真库断言本次会真实执行")
	}
}
