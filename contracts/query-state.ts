/**
 * contracts/query-state.ts —— v1.0
 *
 * 筛选模块（M-FILTER）→ 查询模块（M-QUERY）的唯一契约。
 *
 * 纪律：
 *  1. 本结构必须是**纯可序列化数据**（JSON 安全）：无函数、无 DOM 引用、无闭包。
 *  2. M-FILTER 只产出本结构，不查库、不渲染。
 *  3. M-QUERY 消费本结构，只翻译为查询，不含任何业务公式。
 */

export const QUERY_STATE_VERSION = "1.0" as const;

export interface QueryState {
  /** 协议版本，破坏性变更才升 */
  v: typeof QUERY_STATE_VERSION;
  /** 时间模块状态（对应时间区间滑块） */
  time: TimeRange;
  /** 表格筛选状态 */
  filters: FilterClause[];
  /** 维度选择 */
  dims: DimSelection;
  order?: OrderClause;
  page?: PageClause;
  /** 提示可命中的预计算桶，未命中则 M-QUERY 自行择桶 */
  precomputeHint?: PrecomputeHint;
  /** 来源视图模板 ID（见 view-template.ts） */
  profileId?: string;
}

export interface TimeRange {
  mode: "preset" | "custom" | "slider";
  preset?:
    | "today"
    | "last7d"
    | "last30d"
    | "mtd"
    | "last_full_month"
    | "ytd"
    | "all";
  /** ISO date，mode=custom|slider 时必填 */
  from?: string;
  /** ISO date，mode=custom|slider 时必填 */
  to?: string;
  grain: "day" | "week" | "month" | "quarter" | "half" | "year";
  /** 默认门店所在时区 */
  timezone: string;
}

export interface FilterClause {
  /** 字段 key（不是列名） */
  field: string;
  op: "eq" | "in" | "gt" | "gte" | "lt" | "lte" | "between" | "contains";
  value: unknown;
}

export interface DimSelection {
  /** 阅览层级：与报表五层对应 */
  level: "overview" | "domain" | "channel" | "store" | "sku";
  brand?: string[];
  domain?: string[];
  channelCode?: string[];
  storeKey?: string[];
  category?: string[];
  productStatus?: string[];
}

export interface OrderClause {
  field: string;
  dir: "asc" | "desc";
}

export interface PageClause {
  offset: number;
  limit: number;
}

export interface PrecomputeHint {
  bucket: string;
  /** 期望命中的版本，用于缓存键 */
  algoVersion?: number;
  ruleVersion?: number;
}

/** 规范化：供查询哈希与缓存键使用。实现必须保证字典序稳定的键序列化。 */
export type CanonicalizeQueryState = (s: QueryState) => string;
