/**
 * src/shell/app.ts —— 应用外壳（装配层的**纯逻辑**部分）
 *
 * ★ 为什么把「纯逻辑」从 main.ts 里拆出来：
 *   main.ts 直接摸 DOM 与 fetch，几乎无法单测（要造 jsdom + fetch 替身）。
 *   而这个文件只做**状态 → 视图描述**的转换，是可以被穷举单测的。
 *   真正的副作用（history.pushState / paint / 网络）留在 main.ts。
 *
 * ══════════════════════════════════════════════════════════════════════
 * ★★ 本文件是**装配层**，也是唯一允许同时引用 M-FILTER / M-REPORT /
 *    M-TEMPLATE 的地方。但它仍然守三条纪律：
 *
 *   1. **不实现业务公式**（任何金额/比率都不在这里算）；
 *   2. **不修改 QueryState 的语义**（只做 level → dims 的映射与模板套用）；
 *   3. **不做权限判断**（谁能看什么由后端；这里只做展示分组）。
 * ══════════════════════════════════════════════════════════════════════
 *
 * ★ 三条纪律（都对应真实会踩的坑）：
 *
 *   1. **层级 → 查询维度必须是「逐级收敛」，不能只传 level 而丢参数。**
 *      `?level=store&channel=TK-TH&store=S1` 若只把 level 转成 `dims.level="store"`，
 *      后端会返回**全部店铺**——用户以为在看 S1，其实在看全量。
 *      所以 levelPath → DimSelection 必须把 domain/channel/store 一并带上。
 *
 *   2. **URL 是层级状态的唯一真源**，内存里的 path 只是它的投影。
 *      每次跳转都「先算新 URL，再写 history，再从 URL 重新解析」——
 *      保证「刷新 / 前进后退 / 分享链接」三条路径得到同一结果。
 *
 *   3. **面包屑的祖先层标签必须来自已取到的数据，取不到则回退 id。**
 *      绝不用「层级短名」去顶替（那会让 L2 显示成「渠道」而不是「TK-TH」，
 *      用户就不知道自己在哪个渠道里了）。
 */

import { DEFAULT_TIMEZONE, buildQueryState, makeDims, makePage, makeTimeRange } from "../filter/query-state.js";
import type { DimSelection, QueryState, ReadingLevel } from "../contracts/query-state.js";
import type { LevelSummary } from "../contracts/data-contract.js";
import {
  breadcrumb,
  depthOf,
  drillDown,
  drillUp,
  initialExpanded,
  levelPathToQuery,
  metaOf,
  normalizePath,
  parseRawPath,
  wasDowngraded,
  type Crumb,
  type LabelResolver,
  type LevelPath,
} from "../report/levels.js";
import { applyTemplate, type AppliedView } from "../template/center.js";
import type { ViewTemplate } from "../contracts/view-template.js";

// ───────────────────────────── 层级 ↔ 查询维度 ─────────────────────────────

/** 后端 LevelSummary.level 是 "L0".."L4"；前端 ReadingLevel 是名字。二者需互转。 */
const READER_TO_L: Record<ReadingLevel, LevelSummary["level"]> = {
  overview: "L0",
  domain: "L1",
  channel: "L2",
  store: "L3",
  sku: "L4",
};

const L_TO_READER: Record<LevelSummary["level"], ReadingLevel> = {
  L0: "overview",
  L1: "domain",
  L2: "channel",
  L3: "store",
  L4: "sku",
};

/** ReadingLevel → LevelSummary.level（用于把层级路径映射到返回的 levels）。 */
export function readerToL(level: ReadingLevel): LevelSummary["level"] {
  return READER_TO_L[level] ?? "L0";
}

/** LevelSummary.level → ReadingLevel（反向；未知回退 overview）。 */
export function lToReader(l: LevelSummary["level"]): ReadingLevel {
  return L_TO_READER[l] ?? "overview";
}

/**
 * 把层级路径映射为 DimSelection。
 *
 * ★ 逐级收敛（见文件头纪律 1）：带上「当前层及祖先层」的所有选择。
 *   这不是可选优化 —— 少带一个 channel，L3 的查询就会命中全量店铺。
 *
 * sku 层（L4）目前没有独立的 dims 字段，语义仍由 storeKey 表达
 * （SKU 明细属于某个店铺）；后端后续若增加 sku 维度再补。
 */
export function dimsFromPath(path: LevelPath): DimSelection {
  const d = depthOf(path.level);
  const partial: Partial<Omit<DimSelection, "level">> = {};
  if (d >= 1 && path.domain) partial.domain = [path.domain];
  if (d >= 2 && path.channel) partial.channelCode = [path.channel];
  if (d >= 3 && path.store) partial.storeKey = [path.store];
  return makeDims(path.level, partial);
}

/** 由层级路径组装 QueryState（M-FILTER 出口）。 */
export function queryStateFromPath(
  path: LevelPath,
  opts: { from?: string; to?: string; grain?: QueryState["time"]["grain"]; limit?: number } = {},
): QueryState {
  const time = opts.from && opts.to
    ? makeTimeRange("custom", { from: opts.from, to: opts.to, grain: opts.grain ?? "month", timezone: DEFAULT_TIMEZONE })
    : makeTimeRange("preset", { preset: "mtd", grain: opts.grain ?? "month", timezone: DEFAULT_TIMEZONE });

  return buildQueryState({
    time,
    dims: dimsFromPath(path),
    page: makePage(0, opts.limit ?? 200),
  });
}

// ───────────────────────────── 面包屑标签解析 ─────────────────────────────

/**
 * 从 DataContract.levels 造一个 LabelResolver。
 *
 * ★ 只认「与请求层级一致」的 level 汇总：后端可能返回多层的 levels，
 *   用错层的 label 会让面包屑显示成别的实体的名字（例如把渠道名挂到店铺位）。
 *   匹配不上就返回 undefined，由 breadcrumb 回退到 id —— 绝不猜。
 */
export function makeLabelResolver(levels: readonly LevelSummary[] | undefined): LabelResolver {
  const byKey = new Map<string, string>();
  for (const l of levels ?? []) {
    byKey.set(`${l.level}\u0000${l.key}`, l.label);
  }
  return (level: ReadingLevel, id: string) => byKey.get(`${readerToL(level)}\u0000${id}`);
}

// ───────────────────────────── 视图描述（可测的纯数据） ─────────────────────────────

/** 页头描述。 */
export interface HeaderVM {
  /** 身份摘要文本 */
  identity: string;
  /** 是否可以看到业务数值（D7 提示） */
  canViewBusinessValues: boolean;
}

/** 层级标签页描述。 */
export interface LevelTabVM {
  level: ReadingLevel;
  label: string;
  short: string;
  active: boolean;
  /** 当前层之前（含）都是「可达」的；之后为 false（未解锁，不可直接点） */
  reachable: boolean;
  depth: number;
}

/** 一次渲染需要的全部描述（不含 DOM）。 */
export interface ShellVM {
  header: HeaderVM;
  /** 面包屑（当前层不可点） */
  crumbs: Crumb[];
  /** 面包屑纯文本（title / 日志用） */
  crumbText: string;
  /** 五层切换按钮 */
  tabs: LevelTabVM[];
  /** 当前层级路径（已规范化） */
  path: LevelPath;
  /** 原始 URL 是否因参数不完整被降级（用于给用户提示） */
  downgraded: boolean;
  /** 展开状态（仅 L0 与当前层为 true；L0 不可关） */
  expanded: Record<string, boolean>;
  /** 当前层标题（如「店铺 (Store)」） */
  title: string;
  /** 已套用模板的名称（无则 null） */
  appliedTemplateName: string | null;
  /** 可下钻（未到 L4） */
  canDrillDown: boolean;
  /** 可上钻（未在 L0） */
  canDrillUp: boolean;
}

/** 构造标签页清单。 */
export function levelTabs(path: LevelPath): LevelTabVM[] {
  const d = depthOf(path.level);
  return (["overview", "domain", "channel", "store", "sku"] as ReadingLevel[]).map((lv) => {
    const m = metaOf(lv);
    return {
      level: lv,
      label: m.label,
      short: m.short,
      active: lv === path.level,
      // ★ 只允许点到「当前层及更浅层」：直接跳到更深的层缺参数，
      //   只会被 normalizePath 降级回来（点了没反应 = 更差的体验）。
      reachable: m.depth <= d,
      depth: m.depth,
    };
  });
}

/**
 * 组装 ShellVM。
 *
 * @param rawSearch       原始 location.search（可能不完整/被手改）
 * @param levels          DataContract.levels（用于面包屑标签）
 * @param appliedTemplate 当前已套用的模板（用于回显「当前来自哪个模板」）
 */
export function buildShellVM(
  rawSearch: string,
  opts: {
    levels?: readonly LevelSummary[];
    appliedTemplate?: ViewTemplate | null;
    identity?: string;
    canViewBusinessValues?: boolean;
    expandedOverrides?: Partial<Record<ReadingLevel, boolean>>;
  } = {},
): ShellVM {
  // ★ 用 parseRawPath 而不是 parseLevelPath：后者已经降级，
  //   拿它去和 normalizePath 的结果比，永远相等 —— 降级就永远报不出来（静默）。
  const raw = parseRawPath(rawSearch);
  const path = normalizePath(raw);
  const resolver = makeLabelResolver(opts.levels);
  const crumbs = breadcrumb(path, resolver);
  const d = depthOf(path.level);

  return {
    header: {
      identity: opts.identity ?? "未登录",
      canViewBusinessValues: opts.canViewBusinessValues ?? false,
    },
    crumbs,
    crumbText: crumbs.map((c) => c.text).join(" / "),
    tabs: levelTabs(path),
    path,
    downgraded: wasDowngraded(raw, path),
    expanded: initialExpanded(path, opts.expandedOverrides),
    title: metaOf(path.level).label,
    appliedTemplateName: opts.appliedTemplate?.name ?? null,
    canDrillDown: d < 4,
    canDrillUp: d > 0,
  };
}

// ───────────────────────────── 导航（返回新 URL，不碰 DOM） ─────────────────────────────

/** 导航意图。 */
export type Nav =
  | { kind: "drillDown"; id: string; label?: string }
  | { kind: "drillUp" }
  | { kind: "jump"; depth: number }
  | { kind: "setLevel"; level: ReadingLevel }
  | { kind: "applyTemplate"; template: ViewTemplate };

/** 导航结果：新 URL 的 search（不含前导 `?`）+ 新路径。 */
export interface NavResult {
  search: string;
  path: LevelPath;
}

/**
 * 计算一次导航后的 URL。
 *
 * ★ 纯函数（入参 → 出参），不写 history、不触发渲染。
 *   这样「导航逻辑」可以在没有 DOM 的环境里被穷举验证 ——
 *   而 history/渲染这类副作用由 main.ts 统一施加。
 */
export function navigate(current: LevelPath, nav: Nav): NavResult {
  const cur = normalizePath(current);
  let next: LevelPath;

  switch (nav.kind) {
    case "drillDown":
      next = normalizePath(drillDown(cur, { id: nav.id, label: nav.label }));
      break;
    case "drillUp":
      next = normalizePath(drillUp(cur));
      break;
    case "jump":
      next = normalizePath(jumpTo(cur, nav.depth));
      break;
    case "setLevel": {
      // 直接点某一层标签：保留该层能用的祖先参数（多了会被裁剪）
      next = normalizePath({ ...cur, level: nav.level });
      break;
    }
    case "applyTemplate": {
      // 模板改的是「怎么看」，不改「在看哪一层」：层级路径保持当前值，
      // 由模板带来的 QueryState 决定筛选条件（见 template/center.ts 的替换语义）。
      next = cur;
      break;
    }
    default:
      next = cur;
  }

  return { search: levelPathToQuery(next), path: next };
}

/** 局部复制 jumpToDepth（避免再引一次模块顶层函数，便于单点维护）。 */
function jumpTo(current: LevelPath, depth: number): LevelPath {
  const order: ReadingLevel[] = ["overview", "domain", "channel", "store", "sku"];
  const d = Math.max(0, Math.min(order.length - 1, Math.floor(depth)));
  const level = order[d]!;
  const out: LevelPath = { level };
  if (d >= 1 && current.domain) out.domain = current.domain;
  if (d >= 2 && current.channel) out.channel = current.channel;
  if (d >= 3 && current.store) out.store = current.store;
  return out;
}

// ───────────────────────────── 模板套用 ─────────────────────────────

/** 套用模板后的完整查询状态（层级来自模板的 queryState.dims.level）。 */
export interface TemplateApplied {
  /** 换上一整套 QueryState（替换语义） */
  queryState: QueryState;
  /** 由模板带来的层级路径（从 queryState.dims 反推） */
  path: LevelPath;
  view: AppliedView;
}

/**
 * 套用模板 → 新的「查询状态 + 层级路径」。
 *
 * ★ 替换语义：模板的语义是「重建整个视图」。合并会得到
 *   「模板 × 当前残留」的四不像，且无法用任何模板精确复现。
 *   （见 template/center.ts 文件头纪律 1）
 *
 * ★ 层级从模板的 dims 反推：模板不仅要定筛选，还要定「在几层看」。
 *   若只套筛选不套层级，用户会看到「模板的渠道筛选 + 当前的店铺层级」——
 *   那正是模板作者没表达过的组合，数字会误导人。
 */
export function applyTemplateToState(t: ViewTemplate): TemplateApplied {
  const view = applyTemplate(t);
  const qs = view.queryState;
  const level = (qs?.dims?.level ?? "overview") as ReadingLevel;

  const path: LevelPath = { level };
  const domain = qs?.dims?.domain?.[0];
  const channel = qs?.dims?.channelCode?.[0];
  const store = qs?.dims?.storeKey?.[0];
  if (domain) path.domain = domain;
  if (channel) path.channel = channel;
  if (store) path.store = store;

  return { queryState: qs, path: normalizePath(path), view };
}

/** 从当前层级路径 + 视图偏好派生「保存模板」所需的最小输入（供模板中心调用）。 */
export function describeCurrentView(
  path: LevelPath,
  extra: { filters?: QueryState["filters"]; from?: string; to?: string } = {},
): { page: string; queryState: QueryState } {
  const qs = queryStateFromPath(path, { from: extra.from, to: extra.to });
  if (extra.filters && extra.filters.length > 0) {
    qs.filters = [...qs.filters, ...extra.filters];
  }
  return { page: path.level, queryState: qs };
}

/** 文案：URL 被降级时给用户的提示（不静默）。 */
export function downgradeNotice(vm: ShellVM): string | null {
  if (!vm.downgraded) return null;
  return `链接中的层级参数不完整，已回到「${metaOf(vm.path.level).short}」——请逐层下钻（深层需要先选定上层）`;
}
