/**
 * src/render/render.ts —— M-RENDER
 *
 * 职责：把 DataContract 渲染为视图模型（VM）。
 *
 * ★★ 边界纪律（G1，由 scripts/layering-gate.mjs 静态强制）：
 *   1. 本模块**不得**引入 M-FILTER（filter/）或 QueryState —— 唯一入参是 DataContract。
 *   2. 本模块**不得**发起网络请求（也不应需要）。
 *   3. 本模块**不得**做业务换算：值只能「原样展示或标缺失」，禁止算 gp/cogs 等。
 *
 * 硬性契约：
 *   * 缺失（null/undefined）⇒ 渲染「待接入」或「—」，**绝不允许回退为 0**。
 *   * 五层阅览 L0–L4：L0 默认展开，L1–L4 默认收起。
 *   * 可折叠子表默认收起。
 */

import {
  MISSING_TEXT,
  MISSING_TEXT_SHORT,
  type ColumnDef,
  type DataContract,
  type DataGap,
  type LevelSummary,
  type Row,
} from "../contracts/data-contract.js";

/** 单元格视图模型。 */
export interface CellVM {
  key: string;
  /** 展示文本（缺失时是「待接入」/「—」，永不为 "0" 替身） */
  text: string;
  /** 是否为缺失占位 */
  missing: boolean;
  /** 是否右对齐（数值类） */
  align: "left" | "right";
  /** 缺失原因（供 tooltip / 口径说明） */
  gapReason?: string;
  /** 数据来源算法 + 槽（口径追溯） */
  algoId?: string;
  dataSlots?: string[];
}

/** 行视图模型。 */
export interface RowVM {
  /** 行键（用于展开/折叠与深链） */
  rowKey: string;
  cells: CellVM[];
}

/** 层级视图模型。 */
export interface LevelVM {
  level: LevelSummary["level"];
  key: string;
  label: string;
  expanded: boolean;
  childCount: number;
  metrics: CellVM[]; // 该层汇总指标（同样遵循缺失语义）
}

/** 表格视图模型。 */
export interface TableVM {
  columns: ColumnDef[];
  rows: RowVM[];
  /** 可折叠子表是否展开（默认 false） */
  collapsibleExpanded: boolean;
  /** 命中的预计算桶（性能提示） */
  precomputed: boolean;
  queryHash: string;
  generatedAt: string;
  /** 缺失清单（供「待接入」集中展示） */
  gaps: DataGap[];
}

/** 顶层视图模型。 */
export interface ViewVM {
  levels: LevelVM[];
  table: TableVM;
  /** 无数据（rows=0）时为 true —— 与「值为 0」严格区分 */
  empty: boolean;
}

// ───────────────────────────── 数值格式化（纯展示，不做业务换算） ─────────────────────────────

/**
 * 格式化数值。
 *
 * ★ 关键约束：`null` / `undefined` **永不**返回 "0"，一律返回缺失文案。
 * 这是 G3 在前端的落点。
 */
export function formatNumber(
  value: number | null | undefined,
  kind: ColumnDef["kind"],
  opts: { short?: boolean } = {},
): { text: string; missing: boolean } {
  if (value === null || value === undefined || Number.isNaN(value)) {
    return { text: opts.short ? MISSING_TEXT_SHORT : MISSING_TEXT, missing: true };
  }
  switch (kind) {
    case "currency":
      return { text: `¥${value.toLocaleString("zh-CN", { maximumFractionDigits: 2 })}`, missing: false };
    case "percent":
      // gmp 已在后端算好（0.6 表示 60%）；前端只做展示换算
      return { text: `${(value * 100).toFixed(2)}%`, missing: false };
    case "number":
      return { text: value.toLocaleString("zh-CN", { maximumFractionDigits: 4 }), missing: false };
    case "date":
    case "text":
    default:
      return { text: String(value), missing: false };
  }
}

// ───────────────────────────── 门控（二次防线） ─────────────────────────────

/** 渲染层可接受的最低密级排序（服务端已门控；此处兜底）。 */
const LEVEL_RANK: Record<ColumnDef["perm"], number> = { L1: 1, L2: 2, L3: 3, L4: 4 };

/**
 * 依据用户 maxLevel 过滤列（二次防线）。
 *
 * 服务端 `api.Gate` 已在序列化前剔除未授权字段；前端再挡一次，
 * 保证即使服务端被绕过，浏览器也不会渲染越权列。
 */
export function gateColumns(
  columns: readonly ColumnDef[],
  view: { maxLevel: ColumnDef["perm"]; canViewBusinessValues: boolean; businessValueKeys?: string[] },
): ColumnDef[] {
  const business = new Set(view.businessValueKeys ?? []);
  return columns.filter((c) => {
    if (LEVEL_RANK[c.perm] > LEVEL_RANK[view.maxLevel]) return false;
    // D7：不可见业务数值时，业务数值列一律不渲染
    if (!view.canViewBusinessValues && business.has(c.key)) return false;
    return true;
  });
}

// ───────────────────────────── DataContract → ViewVM ─────────────────────────────

/** 行键：优先用维度键拼接，保证稳定可深链。 */
const KEY_COLUMNS = ["month", "channel_code", "shop_id", "brand", "spu", "sku"] as const;

function rowKeyOf(row: Row, index: number): string {
  const parts: string[] = [];
  for (const k of KEY_COLUMNS) {
    const v = row[k];
    if (v !== null && v !== undefined && v !== "") parts.push(`${k}=${String(v)}`);
  }
  return parts.length > 0 ? parts.join("|") : `row-${index}`;
}

/**
 * 把 DataContract 渲染为 ViewVM。
 *
 * @param dc       唯一数据来源（M-QUERY 产出）
 * @param expanded 层级展开状态（缺省用 defaultExpanded；L0 恒真，其余假）
 */
export function renderDataContract(dc: DataContract, expanded?: Record<string, boolean>): ViewVM {
  const gapByField = new Map<string, DataGap>();
  for (const g of dc.gaps) gapByField.set(g.field, g);

  const traceByField = new Map<string, { algoId: string; dataSlots: string[] }>();
  for (const t of dc.algoTrace) traceByField.set(t.field, { algoId: t.algoId, dataSlots: t.dataSlots });

  // 列（保留顺序）
  const columns = dc.columns;

  // 行
  const rows: RowVM[] = dc.rows.map((r, i) => {
    const cells: CellVM[] = columns.map((c) => {
      const raw = r[c.key];
      const isNumeric = c.kind === "currency" || c.kind === "number" || c.kind === "percent";

      let text: string;
      let missing: boolean;
      if (isNumeric) {
        const f = formatNumber(typeof raw === "number" ? raw : null, c.kind);
        text = f.text;
        missing = f.missing;
      } else {
        missing = raw === null || raw === undefined || raw === "";
        text = missing ? MISSING_TEXT_SHORT : String(raw);
      }

      const cell: CellVM = {
        key: c.key,
        text,
        missing,
        align: isNumeric ? "right" : "left",
      };
      const gap = gapByField.get(c.key);
      if (missing && gap) cell.gapReason = gap.reason;
      const tr = traceByField.get(c.key);
      if (tr) {
        cell.algoId = tr.algoId;
        cell.dataSlots = tr.dataSlots;
      } else if (c.algoId) {
        cell.algoId = c.algoId;
      }
      return cell;
    });
    return { rowKey: rowKeyOf(r, i), cells };
  });

  // 层级
  const levels: LevelVM[] = (dc.levels ?? []).map((l) => {
    const isExpanded = expanded?.[l.level] ?? l.defaultExpanded;
    const metrics: CellVM[] = Object.entries(l.metrics).map(([k, v]) => {
      const f = formatNumber(v, "currency");
      const gap = gapByField.get(k);
      const cell: CellVM = { key: k, text: f.text, missing: f.missing, align: "right" };
      if (f.missing && gap) cell.gapReason = gap.reason;
      return cell;
    });
    return {
      level: l.level,
      key: l.key,
      label: l.label,
      expanded: isExpanded,
      childCount: l.childCount,
      metrics,
    };
  });

  // 若后端未给 levels（例如小结果集），按默认收起契约合成 L0 单层
  if (levels.length === 0) {
    levels.push({
      level: "L0",
      key: "all",
      label: "总览",
      expanded: true, // L0 恒展开
      childCount: rows.length,
      metrics: [],
    });
  }

  return {
    levels,
    table: {
      columns,
      rows,
      collapsibleExpanded: false, // 硬性：可折叠子表默认收起
      precomputed: dc.precomputed,
      queryHash: dc.queryHash,
      generatedAt: dc.generatedAt,
      gaps: dc.gaps,
    },
    empty: rows.length === 0,
  };
}

/**
 * 断言默认收起契约（供测试与运行时自检）。
 *
 * 返回违规描述（空 = 合规）。
 */
export function checkDefaultCollapsed(dc: DataContract, expanded?: Record<string, boolean>): string[] {
  const vm = renderDataContract(dc, expanded);
  const bad: string[] = [];
  for (const l of vm.levels) {
    const want = l.level === "L0";
    if (l.expanded !== want) {
      bad.push(`层级 ${l.level} expanded=${l.expanded}，期望 ${want}`);
    }
  }
  if (vm.table.collapsibleExpanded) bad.push("可折叠子表默认应收起");
  return bad;
}

/**
 * 断言「缺失不被展示为 0」（G3 前端落点）。
 *
 * 扫描所有单元格：若源值为 null 而展示文本为 "0"（或含 "0" 的货币串），即违规。
 */
export function checkNoZeroImputation(dc: DataContract, expanded?: Record<string, boolean>): string[] {
  const vm = renderDataContract(dc, expanded);
  const bad: string[] = [];
  vm.table.rows.forEach((r, i) => {
    r.cells.forEach((c) => {
      const raw = dc.rows[i]?.[c.key];
      const isNull = raw === null || raw === undefined;
      if (!isNull) return;
      // 源为 null 时，展示文本不得像是一个数值 0
      const looksZero = /^[¥]?\s*0(\.0+)?\s*%?$/.test(c.text.trim());
      if (looksZero) bad.push(`行 ${r.rowKey} 字段 ${c.key} 源为 null 却展示为「${c.text}」`);
    });
  });
  return bad;
}
