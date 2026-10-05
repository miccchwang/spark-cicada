/**
 * contracts/permission-request.ts —— v1.0
 *
 * 权限申请单契约（D14，2026-10-05 第二轮拍板新增）。
 *
 * 闭环：用户勾选数据权限 → 提交申请 → 上级审批 → 系统自动开通。
 * 与管理员直接勾选（D11）并存，最终都落到同一张 Entitlement 表。
 *
 * 审批路由约束（D13）：
 *   - 审批人必须**自身拥有**被申请的全部权限（S ⊆ 权限(P)）
 *   - 上级权限不足 → 自动升级到最近的权限超集上级
 *   - 越级/跨枝 → 最近公共上级；无则 T1 兜底
 *   - IT 不可申请业务数值（D7）
 *
 * 详见 docs/08-分组与权限申请设计.md §3–§4
 */

export const PERMISSION_REQUEST_VERSION = "1.0" as const;

export type RequestStatus =
  | "DRAFT"          // 草稿
  | "SUBMITTED"      // 已提交，待路由
  | "APPROVING"      // 审批中
  | "APPROVED"       // 已通过（已开通）
  | "REJECTED"       // 已拒绝
  | "WITHDRAWN";     // 申请人撤回

export interface PermissionRequest {
  v: typeof PERMISSION_REQUEST_VERSION;
  id: string;                       // req_xxx
  applicant: string;                // 申请人账号
  /** 申请内容 = 一份 Entitlement 草案 */
  draft: RequestDraft;
  /** 批量申请：给某个分组申请（与 draft 二选一或并存） */
  targetGroupId?: string;
  /** 用途说明（必填） */
  purpose: string;
  /** 期望有效期；空 = 长期 */
  requestedExpiry?: string;
  status: RequestStatus;
  /** 审批链 */
  approvals: ApprovalStep[];
  createdAt: string;
  resolvedAt?: string;
}

export interface RequestDraft {
  modules: string[];
  dimensions: RequestDimensionGrant[];
  maxLevel: "L1" | "L2" | "L3" | "L4";
  // 注意：canViewBusinessValues 不出现在草案中——
  // IT 恒不可申请（D7），非 IT 由密级决定，无需单独申请。
}

export interface RequestDimensionGrant {
  dim: string;
  values: string[];
  includeDescendants: boolean;
}

export interface ApprovalStep {
  approver: string;
  tier: string;                     // T1..T6
  action: "APPROVE" | "REJECT" | "RETURN" | "PENDING";
  reason?: string;
  at?: string;
}

/** 申请提交前的预校验结果 */
export interface RequestValidation {
  /** 是否通过校验 */
  ok: boolean;
  /** 超出申请人可申请范围的项（如 IT 申请业务数值） */
  outOfScope: string[];
  /** 与已有权限重复、将被自动去除的项 */
  duplicates: string[];
  /** 校验后的审批路由（§3.3 算法结果） */
  route: ApprovalStep[];
  message?: string;
}

/**
 * 审批路由算法签名。
 * 输入：申请人、申请集合。
 * 输出：应走的审批链；若无法路由（如超范围）则返回空链 + 拒绝原因。
 */
export type RouteApproval = (
  applicant: string,
  draft: RequestDraft
) => RequestValidation;
