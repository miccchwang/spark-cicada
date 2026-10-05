/**
 * src/filter/query-state.ts —— M-FILTER
 *
 * 职责：把用户交互累积为 QueryState。
 *
 * ★★ 边界纪律（G1，由 scripts/layering-gate.mjs 静态强制）：
 *   1. 本模块**不得**出现任何网络调用（fetch / XMLHttpRequest / http(s):// / WebSocket）。
 *   2. 本模块**不得**引入渲染层（render/）。
 *   3. 本模块只**产出** QueryState，不做任何业务换算。
 *
 * 这一层允许做的事：规范化、校验、按 preset 计算时间区间、生成默认值。
 */

import {
  QUERY_STATE_VERSION,
  type QueryState,
  type TimeRange,
  type TimePreset,
  type TimeGrain,
  type DimSelection,
  type ReadingLevel,
  type FilterClause,
  type FilterOp,
  type OrderClause,
  type PageClause,
} from "../contracts/query-state.js";

/** 默认时区（门店所在时区；可被页面覆盖）。 */
export const DEFAULT_TIMEZONE = "Asia/Bangkok";

/** 默认粒度。 */
export const DEFAULT_GRAIN: TimeGrain = "month";

/** 默认每页行数。 */
export const DEFAULT_PAGE_SIZE = 100;

/** 最大每页行数（防御：避免前端请求超大页）。 */
export const MAX_PAGE_SIZE = 1000;

// ───────────────────────────── 时间：preset → 区间 ─────────────────────────────

/**
 * 把「现在」按给定时区的本地日期切分为 YYYY-MM-DD。
 *
 * 注意：这里只用 Date 做**日期算术**，不做业务换算（不涉及任何金额/数量）。
 */
function localDate(now: Date, timezone: string): string {
  // 用 Intl 取该时区下的年月日，避免宿主时区影响
  const fmt = new Intl.DateTimeFormat("en-CA", {
    timeZone: timezone,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  });
  return fmt.format(now); // en-CA → YYYY-MM-DD
}

function monthStart(isoDate: string): string {
  return `${isoDate.slice(0, 7)}-01`;
}

function addDays(isoDate: string, days: number): string {
  const [y, m, d] = isoDate.split("-").map(Number) as [number, number, number];
  const dt = new Date(Date.UTC(y, m - 1, d));
  dt.setUTCDate(dt.getUTCDate() + days);
  return dt.toISOString().slice(0, 10);
}

function addMonths(isoDate: string, months: number): string {
  const [y, m] = isoDate.split("-").map(Number) as [number, number, number];
  const dt = new Date(Date.UTC(y, m - 1 + months, 1));
  return dt.toISOString().slice(0, 10);
}

/**
 * 解析 preset 为 [from, to]（含端）。
 *
 * 纪律：解析不出（例如 all）时返回 `{from: undefined, to: undefined}`，
 * 由 M-QUERY 决定是否留空查询全部 —— 本层**不编造**区间。
 */
export function resolvePreset(
  preset: TimePreset,
  timezone: string,
  now: Date = new Date(),
): { from?: string; to?: string } {
  const today = localDate(now, timezone);
  switch (preset) {
    case "today":
      return { from: today, to: today };
    case "last7d":
      return { from: addDays(today, -6), to: today };
    case "last30d":
      return { from: addDays(today, -29), to: today };
    case "mtd":
      return { from: monthStart(today), to: today };
    case "last_full_month": {
      const firstOfThisMonth = monthStart(today);
      const firstOfPrev = addMonths(firstOfThisMonth, -1);
      return { from: firstOfPrev, to: addDays(firstOfThisMonth, -1) };
    }
    case "ytd":
      return { from: `${today.slice(0, 4)}-01-01`, to: today };
    case "all":
      return {}; // 不编造区间
  }
}

/** 构造一个时间范围（唯一入口，保证 preset 与 from/to 不矛盾）。 */
export function makeTimeRange(
  mode: TimeRange["mode"],
  opts: {
    preset?: TimePreset;
    from?: string;
    to?: string;
    grain?: TimeGrain;
    timezone?: string;
  } = {},
  now: Date = new Date(),
): TimeRange {
  const timezone = opts.timezone ?? DEFAULT_TIMEZONE;
  const grain = opts.grain ?? DEFAULT_GRAIN;

  if (mode === "preset") {
    const preset = opts.preset ?? "mtd";
    const { from, to } = resolvePreset(preset, timezone, now);
    const tr: TimeRange = { mode, preset, grain, timezone };
    if (from !== undefined) tr.from = from;
    if (to !== undefined) tr.to = to;
    return tr;
  }

  // custom / slider：必须显式给 from/to
  if (!opts.from || !opts.to) {
    throw new Error(`M-FILTER: mode=${mode} 必须提供 from 与 to（不得留空）`);
  }
  return { mode, from: opts.from, to: opts.to, grain, timezone };
}

// ───────────────────────────── 维度 / 筛选 / 排序 / 分页 ─────────────────────────────

/** 构造维度选择。 */
export function makeDims(level: ReadingLevel, partial: Partial<Omit<DimSelection, "level">> = {}): DimSelection {
  const out: DimSelection = { level };
  if (partial.brand) out.brand = [...partial.brand];
  if (partial.domain) out.domain = [...partial.domain];
  if (partial.channelCode) out.channelCode = [...partial.channelCode];
  if (partial.storeKey) out.storeKey = [...partial.storeKey];
  if (partial.category) out.category = [...partial.category];
  if (partial.productStatus) out.productStatus = [...partial.productStatus];
  return out;
}

/** 支持的算子白名单（与后端 query.BuildWhere 一致）。 */
const OPS: readonly FilterOp[] = ["eq", "in", "gt", "gte", "lt", "lte", "between", "contains"];

/** 构造一条过滤条件（校验算子合法性）。 */
export function makeFilter(field: string, op: FilterOp, value: unknown): FilterClause {
  if (!field) throw new Error("M-FILTER: filter.field 不能为空");
  if (!OPS.includes(op)) throw new Error(`M-FILTER: 非法算子 ${op}`);
  return { field, op, value };
}

/** 构造排序（字段非空）。 */
export function makeOrder(field: string, dir: "asc" | "desc" = "desc"): OrderClause {
  if (!field) throw new Error("M-FILTER: order.field 不能为空");
  return { field, dir };
}

/** 构造分页（带边界钳制，避免请求超大页）。 */
export function makePage(offset = 0, limit = DEFAULT_PAGE_SIZE): PageClause {
  const safeOffset = Math.max(0, Math.floor(offset));
  const safeLimit = Math.min(MAX_PAGE_SIZE, Math.max(1, Math.floor(limit)));
  return { offset: safeOffset, limit: safeLimit };
}

// ───────────────────────────── 组装 QueryState ─────────────────────────────

export interface BuildQueryStateInput {
  time: TimeRange;
  dims: DimSelection;
  filters?: FilterClause[];
  order?: OrderClause;
  page?: PageClause;
  precomputeHintBucket?: string;
  profileId?: string;
}

/**
 * 组装 QueryState（M-FILTER 的**唯一出口**）。
 *
 * 保证：
 *   * `v` 恒为契约版本；
 *   * filters 去重（同 field+op 只留最后一条）；
 *   * 不产生任何 undefined 字段（JSON 安全）。
 */
export function buildQueryState(input: BuildQueryStateInput): QueryState {
  const filters = dedupeFilters(input.filters ?? []);

  const qs: QueryState = {
    v: QUERY_STATE_VERSION,
    time: input.time,
    dims: input.dims,
    filters,
  };
  if (input.order) qs.order = input.order;
  if (input.page) qs.page = input.page;
  if (input.precomputeHintBucket) {
    qs.precomputeHint = { bucket: input.precomputeHintBucket };
  }
  if (input.profileId) qs.profileId = input.profileId;
  return qs;
}

/** 同 (field, op) 去重，保留最后一条（后设覆盖先设）。 */
export function dedupeFilters(filters: readonly FilterClause[]): FilterClause[] {
  const map = new Map<string, FilterClause>();
  for (const f of filters) {
    map.set(`${f.field}\u0000${f.op}`, f);
  }
  return [...map.values()];
}

/**
 * 从 QueryState 派生「人类可读摘要」——用于筛选栏回显。
 *
 * 注意：这是**展示**用的，不参与查询翻译；放这里是为了让 M-RENDER
 * 不必依赖 QueryState（渲染层只拿 DataContract）。
 */
export function describeQueryState(qs: QueryState): string {
  const parts: string[] = [];
  parts.push(qs.time.mode === "preset" ? `预设:${qs.time.preset ?? "-"}` : `${qs.time.from ?? "?"}~${qs.time.to ?? "?"}`);
  parts.push(`粒度:${qs.time.grain}`);
  parts.push(`层级:${qs.dims.level}`);
  if (qs.filters.length) parts.push(`筛选:${qs.filters.length}条`);
  return parts.join(" · ");
}
