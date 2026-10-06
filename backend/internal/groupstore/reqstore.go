// reqstore.go —— M-REQ 申请单的 Postgres 实现。
//
// ★ 表结构来自迁移 0001 的 fact_permission_request：
//     approvals jsonb   ← contracts/permission-request.ts ApprovalStep[]
//     ccs       jsonb   ← CcRecord[]
//   本包**不**另建 fact_request_approval / fact_request_cc 子表 ——
//   那会让审批步骤同时存在于 jsonb 与子表两处，即「两套真相」。
//   详见 store.go 顶部的复盘注释。
//
// ★ 核心纪律：**状态与留痕必须同一条 UPDATE**。
//
//   申请单的 status 与 approvals/ccs 若分两次写：
//     - status 先改、审批步骤没写 ⇒ 界面显示已批但看不到「谁批的」，等于无留痕
//     - 步骤先写、status 没改   ⇒ 界面仍显示待批，审批人重复点，重复授权
//   因此这里把三者放进**一条** UPDATE 语句 —— 单语句天然原子，无需显式事务。
//
//   这不只是「更优雅」—— M-REQ 的整个价值就在于**可追溯**，
//   一次不一致的写入就让这张表失去可信度。
package groupstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

// SaveRequest 落库一张申请单（含审批步骤与抄送）—— 单条 UPDATE，天然原子。
//
// 幂等：以 id 为准 upsert；approvals/ccs 整体覆盖，
// 使表内容与内存态严格一致（避免「撤回后步骤仍在」这类残留）。
func (s *Store) SaveRequest(ctx context.Context, r *req.Request) error {
	if r == nil || r.ID == "" {
		return errors.New("groupstore: 申请单为空或缺 id")
	}
	draftJSON, err := json.Marshal(r.Draft)
	if err != nil {
		return fmt.Errorf("groupstore: 序列化草案 %s: %w", r.ID, err)
	}
	approvalsJSON, err := json.Marshal(nonNilApprovals(r.Approvals))
	if err != nil {
		return fmt.Errorf("groupstore: 序列化审批步骤 %s: %w", r.ID, err)
	}
	ccsJSON, err := json.Marshal(nonNilCCs(r.CCs))
	if err != nil {
		return fmt.Errorf("groupstore: 序列化抄送 %s: %w", r.ID, err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO fact_permission_request
			(id, applicant, draft, purpose, requested_expiry, status,
			 approvals, ccs, cross_dept, created_at, resolved_at)
		VALUES ($1,$2,$3::jsonb,$4,$5,$6,$7::jsonb,$8::jsonb,$9,
		        COALESCE($10, now()), $11)
		ON CONFLICT (id) DO UPDATE SET
			draft            = EXCLUDED.draft,
			purpose          = EXCLUDED.purpose,
			requested_expiry = EXCLUDED.requested_expiry,
			status           = EXCLUDED.status,
			approvals        = EXCLUDED.approvals,
			ccs              = EXCLUDED.ccs,
			cross_dept       = EXCLUDED.cross_dept,
			resolved_at      = EXCLUDED.resolved_at`,
		r.ID, r.Applicant, draftJSON, r.Purpose, r.RequestedExpiry, string(r.Status),
		approvalsJSON, ccsJSON, r.CrossDept, nullTime(r.CreatedAt), r.ResolvedAt)
	if err != nil {
		return fmt.Errorf("groupstore: 落申请单 %s: %w", r.ID, err)
	}
	return nil
}

// LoadRequest 读回一张申请单（含审批步骤与抄送）。
func (s *Store) LoadRequest(ctx context.Context, id string) (*req.Request, error) {
	r := &req.Request{ID: id}
	var (
		draftJSON     []byte
		approvalsJSON []byte
		ccsJSON       []byte
		status        string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT applicant, draft, purpose, requested_expiry, status,
		       COALESCE(approvals, '[]'::jsonb), COALESCE(ccs, '[]'::jsonb),
		       cross_dept, created_at, resolved_at
		FROM fact_permission_request WHERE id = $1`, id).
		Scan(&r.Applicant, &draftJSON, &r.Purpose, &r.RequestedExpiry, &status,
			&approvalsJSON, &ccsJSON, &r.CrossDept, &r.CreatedAt, &r.ResolvedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("groupstore: 申请单 %s 不存在", id)
		}
		return nil, fmt.Errorf("groupstore: 读申请单 %s: %w", id, err)
	}
	r.Status = req.Status(status)

	if len(draftJSON) > 0 {
		if err := json.Unmarshal(draftJSON, &r.Draft); err != nil {
			return nil, fmt.Errorf("groupstore: 解析草案 %s: %w", id, err)
		}
	}
	if len(approvalsJSON) > 0 {
		var aps []req.ApprovalStep
		if err := json.Unmarshal(approvalsJSON, &aps); err != nil {
			return nil, fmt.Errorf("groupstore: 解析审批步骤 %s: %w", id, err)
		}
		r.Approvals = aps
	}
	if len(ccsJSON) > 0 {
		var ccs []chain.CcRecord
		if err := json.Unmarshal(ccsJSON, &ccs); err != nil {
			return nil, fmt.Errorf("groupstore: 解析抄送 %s: %w", id, err)
		}
		r.CCs = ccs
	}
	return r, nil
}

// ListPendingFor 列出某审批人当前待批的申请（走 0006 的 jsonb_path_ops GIN）。
//
// ★ 查询用 `approvals @> '[{"approver":...,"action":"PENDING"}]'`：
//   containment 查询正是 jsonb_path_ops 索引支持的形态。
//   若改成 `approvals ->> 'approver' = $1` 这类取值比较，索引就用不上了
//   （表达式索引不存在）—— 那会退化成全表扫，审批人越多越慢。
func (s *Store) ListPendingFor(ctx context.Context, approver string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	probe, err := json.Marshal([]map[string]string{
		{"approver": approver, "action": "PENDING"},
	})
	if err != nil {
		return nil, fmt.Errorf("groupstore: 构造待批探针: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM fact_permission_request
		WHERE status IN ('APPROVING','COSIGN_PENDING')
		  AND approvals @> $1::jsonb
		ORDER BY created_at DESC LIMIT $2`, probe, limit)
	if err != nil {
		return nil, fmt.Errorf("groupstore: 列待批（%s）: %w", approver, err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("groupstore: 扫描申请单 id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListRequestsByStatus 按状态列出申请单（管理台/回收任务用）。
func (s *Store) ListRequestsByStatus(ctx context.Context, status req.Status, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM fact_permission_request
		WHERE status = $1 ORDER BY created_at DESC LIMIT $2`, string(status), limit)
	if err != nil {
		return nil, fmt.Errorf("groupstore: 列申请单（%s）: %w", status, err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("groupstore: 扫描申请单 id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListReclaimable 列出「已 approve 且已到期」的申请（走 0006 的复合部分索引）。
//
// ★ 回收任务必须能**只拉出到期待回收的单**，而不是全表扫描后逐条判断 ——
//   后者在单量大时会拖垮查询。WHERE 与 0006 的
//   idx_request_reclaimable(status, requested_expiry) WHERE requested_expiry IS NOT NULL
//   完全对齐，整个条件走一次索引扫描。
func (s *Store) ListReclaimable(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM fact_permission_request
		WHERE status = 'APPROVED'
		  AND requested_expiry IS NOT NULL
		  AND requested_expiry <= $1
		ORDER BY requested_expiry LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("groupstore: 列待回收: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("groupstore: 扫描待回收 id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListCosignPending 列出卡在会签上的申请（读 0006 的 v_cosign_pending 视图）。
//
// 返回 request_id → 所有未表态的会签人。
func (s *Store) ListCosignPending(ctx context.Context) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT request_id, cosigner FROM v_cosign_pending ORDER BY request_id`)
	if err != nil {
		return nil, fmt.Errorf("groupstore: 列会签待办: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var id, cc string
		if err := rows.Scan(&id, &cc); err != nil {
			return nil, fmt.Errorf("groupstore: 扫描会签待办: %w", err)
		}
		out[id] = append(out[id], cc)
	}
	return out, rows.Err()
}

// ───────────────────────────── 小工具 ─────────────────────────────

func nonNilApprovals(aps []req.ApprovalStep) []req.ApprovalStep {
	if aps == nil {
		return []req.ApprovalStep{}
	}
	return aps
}

func nonNilCCs(ccs []chain.CcRecord) []chain.CcRecord {
	if ccs == nil {
		return []chain.CcRecord{}
	}
	return ccs
}

// nullTime 把 time.Time 的零值转成 SQL NULL，非零值原样返回。
//
// ★ 为什么必须：`resolved_at` 为 NULL 表示「尚未终结」，这是状态机的
//   一个**语义位**。若把零值时间原样写进去，读回时会得到一个
//   公元 1 年的时间戳 —— 既不是 NULL（未终结），也不像真实时间，
//   下游「resolved_at IS NULL」的判定全部失效。
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
