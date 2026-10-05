/**
 * contracts/data-chain.ts —— v1.1
 *
 * 数据链与审批链契约（第三轮拍板新增；v1.1 落地 F9/F10）。
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
 * v1.1（F9=A 单主属 / F10=C 混合）：org 节点改为唯一 `primaryDept` + 唯一 `supervisor`；
 *   虚线汇报仅抄送；部门来源区分派生/人工覆盖。
 *
 * 详见 docs/08-分组与权限申请设计.md §3.3–§3.4、docs/11 F9/F10
 */

export const DATA_CHAIN_VERSION = "1.1" as const;

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
  /** 审批人（+1）：申请人**主属**直接上级；唯一有决定权的人 */
  approver: string;
  approverTier: string;
  /** 抄送人（+2 或 +1 的直属上级；**含虚线汇报上级**） */
  ccList: string[];
  /** 虚线汇报上级（F9：**仅抄送，不入审批**） */
  dottedLineCc?: string[];
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

/**
 * 组织节点。
 *
 * F9 = **A 单主属**（已拍板）：
 *   - 每人有**唯一** `primaryDept` 与**唯一** `supervisor`；审批链/数据链**只走主属**。
 *   - **虚线汇报**（矩阵组织的分管上级）仅进 `dottedLineSupervisors`，**只抄送、不参与审批**。
 *   - 临时跨部门不看虚线，走**限期授权**（`docs/11` F9），到期自动失效。
 *
 * F10 = **C 混合**（已拍板）：`primaryDept` 默认由组织架构**自动派生**；
 *   允许人工覆盖（`deptSource="MANUAL"`），**人工优先**；人员离职自动回退为派生值。
 */
export interface OrgNode {
  account: string;
  /** 直接上级（**主属**唯一）；顶层为 null */
  supervisor: string | null;
  /** 职权层级：T1 最高，T6 最低 */
  tier: "T1" | "T2" | "T3" | "T4" | "T5" | "T6";
  /** 主属部门（唯一）——取消多部门歧义 */
  primaryDept: string;
  /** 部门来源：派生（默认）/ 人工覆盖（优先） */
  deptSource?: "DERIVED" | "MANUAL";
  /**
   * 虚线汇报上级（矩阵组织分管上级）：**仅抄送，不入审批链**。
   * 默认空；有值时按 cc-only 处理。
   */
  dottedLineSupervisors?: string[];
  /** 是否具备审批职责 */
  canApprove: boolean;
  /** 主属部门（兼容旧字段名 `dept`，等同 `primaryDept`） */
  dept?: string;
}
