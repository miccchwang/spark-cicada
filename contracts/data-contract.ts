/**
 * contracts/data-contract.ts —— v1.0
 *
 * 查询模块（M-QUERY）→ 渲染模块（M-RENDER）的唯一契约。
 *
 * 纪律：
 *  1. M-RENDER 的**唯一入参**是本结构；禁止在渲染层读 QueryState 做换算。
 *  2. 每个非空值必须能沿 algoTrace 回溯到算法与数据槽。
 *  3. 缺失值以 null 表达（前端渲染为「待接入」/「—」），**不得用 0 占位**。
 */

export const DATA_CONTRACT_VERSION = "1.0" as const;

export interface DataContract<T = Row> {
  v: typeof DATA_CONTRACT_VERSION;
  /** 由 QueryState 规范化后哈希，同时作为缓存键 */
  queryHash: string;
  rows: T[];
  columns: ColumnDef[];
  aggregates: Record<string, number | null>;
  /** 五层阅览层级汇总 */
  levels: LevelSummary[];
  /** 每个字段的算法追溯 */
  algoTrace: AlgoTrace[];
  /** 缺失数据清单（渲染为「待接入」） */
  gaps: DataGap[];
  /** 是否命中预计算桶（性能审计用） */
  precomputed: boolean;
  generatedAt: string;
}

export type Row = Record<string, string | number | null>;

export interface ColumnDef {
  key: string;
  label: string;
  /** 密级，渲染层据此做门控 */
  perm: "L1" | "L2" | "L3" | "L4";
  kind: "text" | "number" | "percent" | "currency" | "date";
  pin?: "left" | "right";
  /** 是否为可折叠子表的列（默认收起） */
  collapsible?: boolean;
  /** 来源算法 ID，便于前端展示口径说明 */
  algoId?: string;
}

export interface LevelSummary {
  level: "L0" | "L1" | "L2" | "L3" | "L4";
  key: string;
  label: string;
  metrics: Record<string, number | null>;
  /** 子节点数量（用于「点击展开」提示） */
  childCount: number;
  /** 默认展开状态（L0=true，其余 false） */
  defaultExpanded: boolean;
}

export interface AlgoTrace {
  field: string;
  algoId: string;
  /** 实际用到的数据槽 */
  dataSlots: string[];
  /** 数据不足而被跳过 */
  skipped: boolean;
  reason?: string;
}

export interface DataGap {
  field: string;
  slot: string;
  /** 当前覆盖率 */
  coverage: number;
  /** 门限 */
  gate: number;
  reason: string;
}

/**
 * 默认收起契约（硬性要求）：
 * 所有可收起扩展的表格默认收起，仅 L0 总览常驻。
 */
export const DEFAULT_EXPANDED = {
  L0: true,
  L1: false,
  L2: false,
  L3: false,
  L4: false,
  anyCollapsibleTable: false,
} as const;
