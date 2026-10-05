/**
 * src/contracts/query-state.ts —— QueryState 契约镜像
 *
 * 唯一真源在仓库根 contracts/query-state.ts。此处为 web 构建期的类型镜像，
 * 二者字段必须逐字一致（由 test/contracts.test.mjs 断言，防止漂移）。
 *
 * 纪律：
 *  1. 纯可序列化（JSON 安全）：无函数/无 DOM 引用。
 *  2. M-FILTER 只产出本结构 —— 不查库、不渲染。
 *  3. M-QUERY 消费本结构 —— 只翻译为请求，不含业务公式。
 */

export const QUERY_STATE_VERSION = "1.0" as const;

export type TimeGrain = "day" | "week" | "month" | "quarter" | "half" | "year";

export type TimePreset =
  | "today"
  | "last7d"
  | "last30d"
  | "mtd"
  | "last_full_month"
  | "ytd"
  | "all";

export type ReadingLevel = "overview" | "domain" | "channel" | "store" | "sku";

export type FilterOp =
  | "eq"
  | "in"
  | "gt"
  | "gte"
  | "lt"
  | "lte"
  | "between"
  | "contains";

export interface TimeRange {
  mode: "preset" | "custom" | "slider";
  preset?: TimePreset;
  from?: string;
  to?: string;
  grain: TimeGrain;
  timezone: string;
}

export interface FilterClause {
  field: string;
  op: FilterOp;
  value: unknown;
}

export interface DimSelection {
  level: ReadingLevel;
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
  algoVersion?: number;
  ruleVersion?: number;
}

export interface QueryState {
  v: typeof QUERY_STATE_VERSION;
  time: TimeRange;
  filters: FilterClause[];
  dims: DimSelection;
  order?: OrderClause;
  page?: PageClause;
  precomputeHint?: PrecomputeHint;
  profileId?: string;
}
