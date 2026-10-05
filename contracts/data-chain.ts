/**
 * contracts/data-chain.ts —— v1.0
 *
 * 数据链与审批链契约（第三轮拍板新增）。
 *
 * 本项目区分两条**不同**的链路：
 *   - 数据链（Data Chain）：一份数据归属谁、由谁负责 → 决定「可见边界」与「是否跨部门」
 *   - 审批链（Approval Chain）：一次申请/变更由谁批、抄送给谁 → 「+1 审批 / +2 抄送」
 *
 * 两套链路共同决定：
 *   1. 谁能申请/被授予哪些数据；
 *   2. 申请走给谁批、抄送给谁；
 *   3. 是否触发「跨部门 → 需 T1 批准 + 不可自助申请」。
 *
 * 详见 docs/08-分组与权限申请设计.md §3.3–§3.4
 */

export const DATA_CHAIN_VERSION = "1.0" as const;

/* ───────────────────────── 数据链 ───────────────────────── */

export interface DataChainNode {
  /** 数据范围标识，如 channel:TK-TH / store:S123 / brand:KONVY */
  scope: string;
  /** 数据责任人（该数据的业务 owner） */
  owner: string;
  /** 责任人的上级链（用于跨部门时找共同上级）：[owner, owner.supervisor, …, T1] */
  ownerChain: string[];
  /** 部门 */
  dept: string;
}

export interface DataChain {
  v: typeof DATA_CHAIN_VERSION;
  /** 该数据范围的责任链 */
  nodes: DataChainNode[];
  /** 部门归属（去重） */
  depts: string[];
  /** 是否跨部门（由多个部门共同负责） */
  crossDept: boolean;
}

/** 数据链解析函数签名：给定数据范围，解析出责任链与部门归属 */
export type ResolveDataChain = (scopes: string[]) => DataChain;

/* ───────────────────────── 审批链 ───────────────────────── */

/**
 * 审批链：+1 审批，+2 或 +1 的直属上级抄送。
 *
 *  申请人 U ─┬─ +1 = U.supervisor              → 审批（唯一决定权）
 *            └─ +2 = U.supervisor.supervisor   → 抄送
 *  若 +2 不存在 → 抄送 +1 的直属上级
 */
export interface ApprovalChain {
  applicant: string;
  /** 审批人（+1）：申请人直接上级；唯一有决定权的人 */
  approver: string;
  approverTier: string;
  /** 抄送人（+2 或 +1 的直属上级） */
  ccList: string[];
  /** 抄送规则说明，便于审计与排障 */
  ccRule: "PLUS_TWO" | "PLUS_ONE_SUPERVISOR" | "NONE";
  /** 申请人已是顶层（无 +1）时，+1 取 T1 兜底 */
  fallback: boolean;
  /** 是否跨部门（跨部门时 approver 必须为 T1） */
  crossDept: boolean;
}

/** 审批链解析函数签名 */
export type ResolveApprovalChain = (
  applicant: string,
  dataChain: DataChain
) => ApprovalChain;

/* ───────────────────────── 跨部门规则 ───────────────────────── */

/**
 * 跨部门判定与规则（A1）：
 *   |depts(S)| ≥ 2  →  跨部门  →  需 T1 批准 + 不可自助申请
 */
export interface CrossDeptPolicy {
  /** 判定阈值：涉及部门数 ≥ 该值即视为跨部门 */
  deptThreshold: number;
  /** 跨部门是否允许（本项目：允许覆盖） */
  allowed: boolean;
  /** 是否必须最高权限批准（本项目：true → 必须 T1） */
  requireTopApproval: boolean;
  /** 是否允许自助申请（本项目：false → 不出现在自助申请页） */
  selfServiceable: boolean;
}

export const DEFAULT_CROSS_DEPT_POLICY: CrossDeptPolicy = {
  deptThreshold: 2,
  allowed: true,
  requireTopApproval: true,
  selfServiceable: false,
};

/* ───────────────────────── 组织 ───────────────────────── */

export interface OrgNode {
  account: string;
  /** 直接上级；顶层为 null */
  supervisor: string | null;
  /** 职权层级：T1 最高，T6 最低 */
  tier: "T1" | "T2" | "T3" | "T4" | "T5" | "T6";
  /** 部门 */
  dept: string;
  /** 是否具备审批职责 */
  canApprove: boolean;
}
