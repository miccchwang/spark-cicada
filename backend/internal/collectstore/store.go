// Package collectstore —— M-COLLECT 的 Postgres 实现（临时仓库读写）。
//
// 与 collect 包的分工：
//   * collect 包 = 纯逻辑（档位、断点、幂等键、守卫判定），可穷举单测。
//   * 本包 = IO 搬运，把纯逻辑的决策**原子地**落库。
//
// ★ 本文件承载三条铁律，全部来自用户要求与 KODP 实战教训：
//
//  1. **每页一次 COMMIT** —— 网络成果立即持久化，不等整批。
//     整批失败就整批重放的模式，会因一次 429 丢掉全部已取数据。
//
//  2. **数据行与游标必须同事务提交** ——
//     若先写数据再单独推游标，两件事之间崩溃会造成两种恶果：
//       - 数据落了、游标没推 ⇒ 重启后重复调用同一页（浪费配额，可能再撞限流）
//       - 游标推了、数据没落 ⇒ 永久丢页（更危险，且守卫未必发现）
//     CommitPage 把两者放进一个事务，消除这个窗口。
//
//  3. **落表失败重试不产生新的上游调用** ——
//     靠 staging_record 的 UNIQUE(job_id, idem_key) 在**数据库层**去重，
//     而非应用层「先查后插」（并发下会漏）。
package collectstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miccchwang/spark-cicada/backend/internal/collect"
)

// Store 临时仓库的读写入口。
type Store struct{ pool *pgxpool.Pool }

// New 用已有连接池构造。
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// StagedRow 一条待落库的原始行。
type StagedRow struct {
	// NaturalKey 上游对该行的业务标识（如订单号、SPU）。
	// 与 jobID 组成幂等键。**空值必须由调用方拒绝** —— 见 CommitPage。
	NaturalKey string
	// Payload 上游原始 JSON，原样保存（上游 schema 可变，不在落库时解释）。
	Payload []byte
}

// CommitPage 落**一页**数据并推进游标 —— 全程一个事务。
//
// 参数：
//   - st     当前作业状态（游标/计数/档位）；函数会就地更新它
//   - pageNo 页序号（0 起）
//   - rows   本页行
//   - nextCursor 上游返回的下一页游标；空串 = 已到底
//
// 返回本次**实际新插入**的行数。重复行（幂等键已存在）被静默跳过，
// 这正是「重试不产生新上游调用」落到 DB 层的体现。
//
// 失败语义：任一环节出错即整个事务回滚 ⇒ 游标不推进 ⇒ 下次从原处重试。
// **绝不会出现「游标推了但数据没落」**。
func (s *Store) CommitPage(ctx context.Context, st *collect.JobState, pageNo int, rows []StagedRow, nextCursor string) (int, error) {
	if st == nil {
		return 0, errors.New("collectstore: 作业状态为空")
	}
	// 空 natural_key 必须拒绝：它会让幂等键退化成 (jobID, "")，
	// 于是所有该页行互相覆盖 —— 静默丢数据。宁可整页失败并告警。
	for i, r := range rows {
		if r.NaturalKey == "" {
			return 0, fmt.Errorf("collectstore: 第 %d 行 natural_key 为空；"+
				"空键会让幂等去重退化为互相覆盖，拒绝落库", i)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("collectstore: 开启事务: %w", err)
	}
	// 回滚兜底：提交成功后 Rollback 是 no-op，因此这里可以无条件 defer。
	defer func() { _ = tx.Rollback(ctx) }()

	inserted := 0
	for _, r := range rows {
		idem := collect.IdemKey(st.ID, r.NaturalKey)
		// ON CONFLICT DO NOTHING 是**数据库级**幂等：
		// 并发/重试都不会产生第二行，比「先 SELECT 再 INSERT」可靠。
		tag, err := tx.Exec(ctx, `
			INSERT INTO staging_record (job_id, page_no, idem_key, natural_key, payload, region)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (job_id, idem_key) DO NOTHING`,
			st.ID, pageNo, idem, r.NaturalKey, r.Payload, st.Region)
		if err != nil {
			return 0, fmt.Errorf("collectstore: 落 staging（%s）: %w", r.NaturalKey, err)
		}
		inserted += int(tag.RowsAffected())
	}

	// 分页台账：同一页重复写不新增行（PK 是 (job_id, page_no)）
	if _, err := tx.Exec(ctx, `
		INSERT INTO collect_page (job_id, page_no, cursor_in, cursor_out, row_count)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (job_id, page_no) DO UPDATE
			SET cursor_out = EXCLUDED.cursor_out,
			    row_count  = EXCLUDED.row_count,
			    fetched_at = now()`,
		st.ID, pageNo, nullIfEmpty(st.Cursor), nullIfEmpty(nextCursor), len(rows)); err != nil {
		return 0, fmt.Errorf("collectstore: 写分页台账: %w", err)
	}

	// ★ 推进内存态后同事务落库 —— 数据与游标原子提交。
	st.Advance(nextCursor, int64(inserted))
	if err := updateJobState(ctx, tx, st); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("collectstore: 提交: %w", err)
	}
	return inserted, nil
}

// updateJobState 把作业状态写回 collect_job（在给定事务内）。
func updateJobState(ctx context.Context, tx pgx.Tx, st *collect.JobState) error {
	_, err := tx.Exec(ctx, `
		UPDATE collect_job SET
			cursor        = $2,
			pages_done    = $3,
			rows_staged   = $4,
			gear_index    = $5,
			ok_streak     = $6,
			status        = $7,
			finished_at   = CASE WHEN $7 = 'COMPLETE' THEN now() ELSE finished_at END,
			updated_at    = now()
		WHERE id = $1`,
		st.ID, nullIfEmpty(st.Cursor), st.PagesDone, st.RowsStaged,
		st.GearIndex, st.OkStreak, st.Status)
	if err != nil {
		return fmt.Errorf("collectstore: 更新作业状态: %w", err)
	}
	return nil
}

// SaveGovernor 持久化降速档位（限流后 / 降档后调用）。
//
// 必须持久化而非只留内存：进程重启后若从 0 档（0.5s）重来，
// 会立刻再次撞限流，退避成果全丢。
func (s *Store) SaveGovernor(ctx context.Context, jobID string, g *collect.RateGovernor, status string, nextAttemptAt *string, lastErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE collect_job SET
			gear_index      = $2,
			ok_streak       = $3,
			status          = $4,
			last_error      = $5,
			next_attempt_at = $6::timestamptz,
			updated_at      = now()
		WHERE id = $1`,
		jobID, g.Index(), g.OkStreak(), status, nullIfEmpty(lastErr), nextAttemptAt)
	if err != nil {
		return fmt.Errorf("collectstore: 保存档位: %w", err)
	}
	return nil
}

// ───────────────────────── 断点续传：读回作业 ─────────────────────────

// LoadJob 读回作业状态（断点续传的入口）。
//
// 重启后调用它即可还原：游标、已取页数、已落行数、当前档位、连击数。
// 这正是「进程怎么死都能接着跑」的实现。
func (s *Store) LoadJob(ctx context.Context, jobID string) (*collect.JobState, error) {
	var st collect.JobState
	var cursor *string
	var slotID *string

	err := s.pool.QueryRow(ctx, `
		SELECT id, slot_id, scope, region, cursor, pages_done, rows_staged,
		       gear_index, ok_streak, status, COALESCE(last_error, '')
		FROM collect_job WHERE id = $1`, jobID).
		Scan(&st.ID, &slotID, &st.Scope, &st.Region, &cursor,
			&st.PagesDone, &st.RowsStaged, &st.GearIndex, &st.OkStreak,
			&st.Status, &st.LastError)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("collectstore: 作业 %s 不存在", jobID)
		}
		return nil, fmt.Errorf("collectstore: 读作业 %s: %w", jobID, err)
	}
	if cursor != nil {
		st.Cursor = *cursor
	}
	if slotID != nil {
		st.SlotID = *slotID
	}
	return &st, nil
}

// ───────────────────────── 守卫统计与过闸 ─────────────────────────

// StagingStats staging 实测统计（守卫判定的输入来源）。
type StagingStats struct {
	ActualPages int
	ActualRows  int64
	HasCursorEnded bool
	CursorEnded    bool
}

// CollectStats 汇总某作业的 staging 实测值。
//
// ★ 实现要点：ActualPages 用 COUNT(DISTINCT page_no) 而非 COUNT(*)，
//   否则「同一页被重试写入」会被算成多页，让守卫误判页数已闭合。
//   CursorEnded 直接取 collect_job.status='COMPLETE' —— 那是上游明确
//   「没有下一页」的唯一权威信号，不接受任何推测。
func (s *Store) CollectStats(ctx context.Context, jobID string) (StagingStats, error) {
	var out StagingStats
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(DISTINCT page_no) FROM collect_page  WHERE job_id = $1),
			(SELECT COUNT(*)               FROM staging_record WHERE job_id = $1),
			(SELECT status = 'COMPLETE'    FROM collect_job    WHERE id     = $1)`,
		jobID).Scan(&out.ActualPages, &out.ActualRows, &out.CursorEnded)
	if err != nil {
		return out, fmt.Errorf("collectstore: 汇总 staging 统计: %w", err)
	}
	out.HasCursorEnded = true
	return out, nil
}

// EvaluateGuard 执行守卫判定并**落库判定结果**，返回判定。
//
// 纪律：判定结果必须落库（staging_ingest_guard），因为「过闸」是一个
// **可审计的事件** —— 事后要能回答「这批数据当时凭什么被放行」。
// 只返回不落库，等于把准入决策变成不可追溯的黑盒。
func (s *Store) EvaluateGuard(ctx context.Context, jobID string, in collect.GuardInput) (collect.GuardVerdict, int64, error) {
	v := collect.Evaluate(in)

	// ★ 显式把 nil 切片转成空切片再入库。
	//
	//   真实事故（真库抓出，单测抓不到）：
	//     ERROR: null value in column "reasons" ... violates not-null constraint (SQLSTATE 23502)
	//   表定义是 `reasons text[] NOT NULL DEFAULT '{}'`，看起来「有默认值」——
	//   但 **DEFAULT 只在「不提供该列」时生效**；显式传 NULL 会直接撞 NOT NULL。
	//   而 Go 的 nil []string 经 pgx 正是被编码成 SQL NULL。
	//
	//   注意这个 bug 只在「守卫通过」时触发：通过时 Reasons 为空（nil），
	//   不通过时至少有一条原因（非 nil）。所以它专门在**最开心的路径**上炸，
	//   典型的「只在成功分支才暴露」的缺陷。
	if v.Reasons == nil {
		v.Reasons = []string{}
	}

	var guardID int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO staging_ingest_guard
			(job_id, expected_pages, actual_pages, expected_rows, actual_rows,
			 cursor_ended, required_ok, coverage, passed, reasons)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id`,
		jobID, in.ExpectedPages, in.ActualPages, in.ExpectedRows, in.ActualRows,
		in.CursorEnded, in.RequiredOK, in.Coverage, v.Passed, v.Reasons).Scan(&guardID)
	if err != nil {
		return v, 0, fmt.Errorf("collectstore: 落守卫判定: %w", err)
	}

	// 同步 staging_record 的守卫状态，便于按状态检索/清理。
	// 注意：**不通过时标 PENDING（而非 REJECTED）** —— 用户明确要求
	// 「未过闸的数据停留 staging 标 PENDING」，因为补齐后还要再过闸。
	state := "PENDING"
	if v.Passed {
		state = "PASSED"
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE staging_record SET guard_state = $2,
			reject_reason = CASE WHEN $2 = 'PENDING' THEN $3 ELSE NULL END
		WHERE job_id = $1`, jobID, state, joinReasons(v.Reasons)); err != nil {
		return v, guardID, fmt.Errorf("collectstore: 更新 staging 守卫状态: %w", err)
	}
	return v, guardID, nil
}

// ReleaseToProjection 过闸后放行到投影层。
//
// ★ 强制前置：只有 passed=true 的守卫记录才允许放行。
//   这里重新查一次 DB 的 passed 值，而不是信调用方传进来的布尔 ——
//   避免「上层记错状态导致残缺数据进投影」。
//
// 用户已定保留策略：**过闸即清理** —— 放行成功后删除该作业的 staging 行。
// 删除与放行标记在同一事务，避免「标了放行却删失败」或反之。
func (s *Store) ReleaseToProjection(ctx context.Context, jobID string, guardID int64, targetTable string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("collectstore: 开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 权威校验：该守卫判定确实是通过
	var passed bool
	if err := tx.QueryRow(ctx,
		`SELECT passed FROM staging_ingest_guard WHERE id = $1 AND job_id = $2`,
		guardID, jobID).Scan(&passed); err != nil {
		return 0, fmt.Errorf("collectstore: 读守卫判定 #%d: %w", guardID, err)
	}
	if !passed {
		return 0, fmt.Errorf("collectstore: 守卫判定 #%d 未通过，拒绝放行到 %s（fail-closed）",
			guardID, targetTable)
	}

	var staged int64
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM staging_record WHERE job_id = $1`, jobID).Scan(&staged); err != nil {
		return 0, fmt.Errorf("collectstore: 统计待放行行数: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO staging_project_release (job_id, guard_id, released_rows, target_table)
		VALUES ($1, $2, $3, $4)`, jobID, guardID, staged, targetTable); err != nil {
		return 0, fmt.Errorf("collectstore: 记放行台账: %w", err)
	}

	// 用户已定保留策略：过闸即清理
	if _, err := tx.Exec(ctx, `DELETE FROM staging_record WHERE job_id = $1`, jobID); err != nil {
		return 0, fmt.Errorf("collectstore: 清理 staging: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("collectstore: 提交放行: %w", err)
	}
	return staged, nil
}

// ───────────────────────── 小工具 ─────────────────────────

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func joinReasons(rs []string) string {
	if len(rs) == 0 {
		return ""
	}
	out := rs[0]
	for _, r := range rs[1:] {
		out += "；" + r
	}
	return out
}
