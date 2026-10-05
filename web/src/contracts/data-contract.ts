/**
 * src/contracts/data-contract.ts —— DataContract 契约镜像
 *
 * 唯一真源在仓库根 contracts/data-contract.ts（由 contracts.test.mjs 校验一致）。
 *
 * 纪律：
 *  1. M-RENDER 的**唯一入参**是本结构；禁止在渲染层读 QueryState。
 *  2. 每个非空值可沿 algoTrace 回溯到算法与数据槽。
 *  3. 缺失值以 null 表达（渲染「待接入」/「—」），**不得用 0 占位**。
 */

export const DATA_CONTRACT_VERSION = "1.0" as const;

export type Row = Record<string, string | number | null>;

export type PermLevel = "L1" | "L2" | "L3" | "L4";
export type ColumnKind = "text" | "number" | "percent" | "currency" | "date";
export type LevelKey = "L0" | "L1" | "L2" | "L3" | "L4";

export interface ColumnDef {
  key: string;
  label: string;
  /** 密级；渲染层据此门控（服务端已先门控，前端为二次防线） */
  perm: PermLevel;
  kind: ColumnKind;
  pin?: "left" | "right";
  /** 可折叠子表的列（默认收起） */
  collapsible?: boolean;
  /** 来源算法 ID，用于口径说明 */
  algoId?: string;
}

export interface LevelSummary {
  level: LevelKey;
  key: string;
  label: string;
  metrics: Record<string, number | null>;
  childCount: number;
  /** L0=true，其余 false（硬性契约） */
  defaultExpanded: boolean;
}

export interface AlgoTrace {
  field: string;
  algoId: string;
  dataSlots: string[];
  skipped: boolean;
  reason?: string;
}

export interface DataGap {
  field: string;
  slot: string;
  coverage: number;
  gate: number;
  reason: string;
}

export interface DataContract<T = Row> {
  v: typeof DATA_CONTRACT_VERSION;
  queryHash: string;
  rows: T[];
  columns: ColumnDef[];
  aggregates: Record<string, number | null>;
  levels: LevelSummary[];
  algoTrace: AlgoTrace[];
  gaps: DataGap[];
  precomputed: boolean;
  generatedAt: string;
}

/** 默认收起契约（硬性要求）。 */
export const DEFAULT_EXPANDED = {
  L0: true,
  L1: false,
  L2: false,
  L3: false,
  L4: false,
  anyCollapsibleTable: false,
} as const;

/** 缺失文案（唯一真源，禁止在别处硬编码 0）。 */
export const MISSING_TEXT = "待接入" as const;
export const MISSING_TEXT_SHORT = "—" as const;
