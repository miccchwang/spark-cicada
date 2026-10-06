/**
 * src/report/levels.ts —— M-REPORT 五层阅览层级（L0→L4）的编排与跳转
 *
 * 职责（docs/01 §9、docs/02 M-REPORT）：
 *   * 五层阅览：L0 总览 / L1 板块 / L2 渠道 / L3 店铺 / L4 SKU；
 *   * 层级路由契约（URL 深链）；
 *   * 面包屑逐级返回；
 *   * 默认收起契约（仅 L0 展开 —— 硬性要求，§9.4）。
 *
 * ══════════════════════════════════════════════════════════════════════════
 * ★★ 边界纪律（由 scripts/layering-gate.mjs 静态强制）：
 *
 *   1. **不发起网络请求**。本模块只负责「层级状态 ↔ URL」与「面包屑」，
 *      取数由装配层（main.ts）调 M-QUERY。
 *
 *   2. **不做业务换算**。层级汇总的金额由 DataContract 提供，
 *      本模块只决定「显示哪几层、每层叫什么、怎么跳」。
 *
 *   3. **不引用 M-RENDER 的 VM**。本模块产出**纯数据**（LevelPath / Crumb），
 *      渲染由装配层交给 M-RENDER 的结果去画。
 * ══════════════════════════════════════════════════════════════════════════
 *
 * ★ 三条纪律（都对应真实会踩的坑）：
 *
 *   1. **URL 是唯一真源。** 层级状态存 URL，不存内存单例。
 *      否则「刷新后回到 L0」「分享链接打开是别人的层级」这类问题无解。
 *
 *   2. **非法/不完整的层级参数一律降级到最近的可达层，而不是报错或臆测。**
 *      用户手改 URL（或链接被截断）是常态：`?level=store` 却漏了 channel，
 *      此时应退到 L1 并提示，而不是「猜一个渠道」—— 猜错会展示**错误的数字**。
 *
 *   3. **默认收起是硬性契约**：L0 恒展开，L1–L4 默认收起。
 *      这不是审美偏好，而是「避免用户误读汇总口径」的设计约束（§9.4）。
 */

import type { ReadingLevel } from "../contracts/query-state.js";

/**
 * 五层阅览的层级 id（overview 起，sku 终）。
 *
 * 与 contracts/query-state.ts 的 ReadingLevel 是**同一套值** ——
 * 这里重新声明常量数组是为了拿到「有序」语义（ReadingLevel 是联合类型，无序）。
 */
export const READING_LEVELS: readonly ReadingLevel[] = ["overview", "domain", "channel", "store", "sku"];

/** 层级展示元数据。 */
export interface LevelMeta {
  /** 0..4 —— L0 总览 … L4 SKU */
  depth: number;
  id: ReadingLevel;
  /** 短名（面包屑用） */
  short: string;
  /** 全名（标题用） */
  label: string;
  /** 默认是否展开（§9.4：仅 L0 为 true） */
  defaultExpanded: boolean;
}

/** 层级元数据表（顺序即深度）。 */
export const LEVEL_META: readonly LevelMeta[] = [
  { depth: 0, id: "overview", short: "总览", label: "总览 (Overview)", defaultExpanded: true },
  { depth: 1, id: "domain", short: "板块", label: "板块 (Domain)", defaultExpanded: false },
  { depth: 2, id: "channel", short: "渠道", label: "渠道 (Channel)", defaultExpanded: false },
  { depth: 3, id: "store", short: "店铺", label: "店铺 (Store)", defaultExpanded: false },
  { depth: 4, id: "sku", short: "SKU", label: "SKU 明细 (SKU)", defaultExpanded: false },
];

/** 默认展开状态表（§9.4 硬性契约）。 */
export const DEFAULT_EXPANDED: Readonly<Record<ReadingLevel, boolean>> = Object.freeze(
  LEVEL_META.reduce(
    (acc, m) => {
      acc[m.id] = m.defaultExpanded;
      return acc;
    },
    {} as Record<ReadingLevel, boolean>,
  ),
);

/** 取某层的元数据（未知层级返回 L0）。 */
export function metaOf(level: ReadingLevel): LevelMeta {
  return LEVEL_META.find((m) => m.id === level) ?? LEVEL_META[0]!;
}

/** 深度（0..4）。 */
export function depthOf(level: ReadingLevel): number {
  return metaOf(level).depth;
}

// ───────────────────────────── 层级路径（深链状态） ─────────────────────────────

/**
 * 一条层级路径 —— 从 L0 到当前层的完整选择。
 *
 * ★ 用**完整路径**而非单个 level：
 *   面包屑需要每一级的标签与目标 URL；只存 level 会丢失「各层选了什么」，
 *   导致「返回上一级」时不知道该回哪个板块。
 */
export interface LevelPath {
  level: ReadingLevel;
  /** L1 板块 id（如 housebrand） */
  domain?: string;
  /** L2 渠道 code（如 TK-TH） */
  channel?: string;
  /** L3 店铺 key（shop_id） */
  store?: string;
}

/**
 * 从 URL search string **原样**解析层级路径（不做降级、不做裁剪）。
 *
 * ★ 为什么需要一个「不降级」的解析器：
 *   `parseLevelPath` 出于安全会立即降级（避免调用方误用不完整路径去取数），
 *   于是「链接是否被降级过」这个信息在返回值里就丢失了 ——
 *   而降级本身要给用户一个提示（不静默）。
 *   所以拆成两个：这个负责「如实读」，parseLevelPath 负责「读 + 规范化」。
 *
 *   只有**确实需要报告降级**的装配层才用这个函数；其余一律用 parseLevelPath。
 */
export function parseRawPath(search: string): LevelPath {
  const q = new URLSearchParams(search.startsWith("?") ? search : `?${search}`);
  const raw = (q.get("level") ?? "overview") as ReadingLevel;
  const level = READING_LEVELS.includes(raw) ? raw : "overview";

  const p: LevelPath = { level };
  const domain = q.get("domain");
  const channel = q.get("channel");
  const store = q.get("store");
  if (domain) p.domain = domain;
  if (channel) p.channel = channel;
  if (store) p.store = store;
  return p;
}

/**
 * 从 URL search string 解析层级路径（**已规范化**：不完整即降级）。
 *
 * 这是默认入口 —— 调用方拿到的一定是「可安全取数」的路径。
 * 若需要知道原始形态（例如给用户提示「链接不完整」），
 * 用 parseRawPath 再自行比较（见 wasDowngraded）。
 */
export function parseLevelPath(search: string): LevelPath {
  return normalizePath(parseRawPath(search));
}

/** 把层级路径序列化回 query string（不含前导 `?`）。 */
export function levelPathToQuery(p: LevelPath): string {
  const q = new URLSearchParams();
  q.set("level", p.level);
  // 只带上「当前层及其祖先层」的参数 —— 带多余参数会让深链看起来
  // 属于别的层级（例如在 L1 却带着 channel），混淆排障。
  const d = depthOf(p.level);
  if (d >= 1 && p.domain) q.set("domain", p.domain);
  if (d >= 2 && p.channel) q.set("channel", p.channel);
  if (d >= 3 && p.store) q.set("store", p.store);
  return q.toString();
}

/**
 * 规范化层级路径：把**不完整的层级选择降级到最近可达层**。
 *
 * ★ 这是本模块最重要的正确性保证。举例：
 *   `?level=store` 但没给 channel ⇒ 无法定位店铺 ⇒ 退到 L1（板块）。
 *   若不降级而硬闯 L3，取数时 channel 为空会命中**全量**数据，
 *   用户看到的是「看起来是某店铺、其实是全公司」的数字 —— 比报错危险得多。
 */
export function normalizePath(p: LevelPath): LevelPath {
  const d = depthOf(p.level);
  // L1 需要 domain；L2 需要 domain+channel；L3/L4 需要 domain+channel+store
  if (d >= 1 && !p.domain) return { level: "overview" };
  if (d >= 2 && !p.channel) return { level: "domain", domain: p.domain };
  if (d >= 3 && !p.store) return { level: "channel", domain: p.domain, channel: p.channel };
  // 逐层裁剪：只保留当前层用得到的参数（避免脏参数残留）
  const out: LevelPath = { level: p.level };
  if (d >= 1 && p.domain) out.domain = p.domain;
  if (d >= 2 && p.channel) out.channel = p.channel;
  if (d >= 3 && p.store) out.store = p.store;
  return out;
}

/** 路径是否被降级过（用于给用户一个「链接不完整」的提示）。 */
export function wasDowngraded(original: LevelPath, normalized: LevelPath): boolean {
  return original.level !== normalized.level;
}

// ───────────────────────────── 跳转 ─────────────────────────────

/**
 * 下钻一层：从当前层 + 选中项，算出下一层的路径。
 *
 * ★ 每次只前进一步（而不是「直接跳到 L4」）：
 *   层级是**逐级收敛**的过程（L0 全量 → L1 某板块 → …），
 *   一步跳多级会跳过中间层的口径确认。
 */
export function drillDown(current: LevelPath, selection: { id: string; label?: string }): LevelPath {
  const d = depthOf(current.level);
  switch (d) {
    case 0:
      return { level: "domain", domain: selection.id };
    case 1:
      return { level: "channel", domain: current.domain, channel: selection.id };
    case 2:
      return { level: "store", domain: current.domain, channel: current.channel, store: selection.id };
    default:
      // 已在最细层，无可下钻（返回原路径，调用方据此禁用交互）
      return current;
  }
}

/**
 * 上钻一级（面包屑的上一级 / 返回）。
 *
 * ★ 返回时保留**上层已选**的参数：
 *   从 L3 返回 L2 应停在「当前所在渠道」，而不是跳回全部渠道 ——
 *   后者会让用户瞬间丢失方向感（「我刚才在哪个渠道来着」）。
 */
export function drillUp(current: LevelPath): LevelPath {
  const d = depthOf(current.level);
  if (d === 0) return current;
  if (d === 1) return { level: "overview" };
  if (d === 2) return { level: "domain", domain: current.domain };
  return { level: "channel", domain: current.domain, channel: current.channel };
}

/** 跳到指定深度的祖先层（面包屑点击任意一级）。 */
export function jumpToDepth(current: LevelPath, depth: number): LevelPath {
  const target = LEVEL_META[Math.max(0, Math.min(LEVEL_META.length - 1, Math.floor(depth)))]!;
  const out: LevelPath = { level: target.id };
  if (target.depth >= 1 && current.domain) out.domain = current.domain;
  if (target.depth >= 2 && current.channel) out.channel = current.channel;
  if (target.depth >= 3 && current.store) out.store = current.store;
  return out;
}

// ───────────────────────────── 面包屑 ─────────────────────────────

/** 面包屑的一项。 */
export interface Crumb {
  depth: number;
  level: ReadingLevel;
  /** 显示文本（当前层用传入的 label，祖先层用已选项 id 或层级短名） */
  text: string;
  /** 该级对应的路径（用于点击返回） */
  target: LevelPath;
  /** 是否为当前层（UI 上不可点 / 高亮） */
  current: boolean;
  /** 是否可点击（当前层不可点） */
  clickable: boolean;
}

/** 标签解析函数：把 层级+id 转成展示名（由装配层注入数据源，避免本模块查库）。 */
export type LabelResolver = (level: ReadingLevel, id: string) => string | undefined;

/**
 * 构造面包屑。
 *
 * ★ 始终包含 L0（总览）作为根 —— 用户任何时候都能一键回到全貌。
 *   L0 的文案固定为「总览」；若 data 里没有对应 label，祖先层回退到 id。
 *   回退到 id 而不是省略该项：**宁可显示一个丑的 id，也不要让用户少一级可返回**。
 */
export function breadcrumb(path: LevelPath, resolveLabel?: LabelResolver): Crumb[] {
  const d = depthOf(path.level);
  const out: Crumb[] = [];

  for (const m of LEVEL_META) {
    if (m.depth > d) break;
    const target = jumpToDepth(path, m.depth);

    let text: string;
    if (m.depth === 0) {
      text = m.short;
    } else {
      const id = idAt(path, m.depth);
      const resolved = id && resolveLabel ? resolveLabel(m.id, id) : undefined;
      // 祖先层优先显示「已选项」，解析不到则回退到 id（绝不省略）
      text = resolved ?? id ?? m.short;
    }

    out.push({
      depth: m.depth,
      level: m.id,
      text,
      target,
      current: m.depth === d,
      clickable: m.depth !== d,
    });
  }
  return out;
}

/** 取某深度对应的已选 id（无则 undefined）。 */
function idAt(p: LevelPath, depth: number): string | undefined {
  switch (depth) {
    case 1:
      return p.domain;
    case 2:
      return p.channel;
    case 3:
      return p.store;
    default:
      return undefined;
  }
}

/** 面包屑的纯文本形式（用于 title / 日志 / 导出）。 */
export function breadcrumbText(path: LevelPath, resolveLabel?: LabelResolver): string {
  return breadcrumb(path, resolveLabel)
    .map((c) => c.text)
    .join(" / ");
}

// ───────────────────────────── 展开状态 ─────────────────────────────

/**
 * 计算某层级的初始展开状态。
 *
 * ★ 硬性契约（§9.4）：
 *   * L0 恒 true（常驻）；
 *   * 当前**所在一层**展开（否则「我在 L2 却只看见 L0」很怪）；
 *   * 其余默认 false。
 *
 * 传入 overrides 时，overrides 优先 —— 但**不得**把 L0 关掉
 * （那是契约违反，直接忽略该覆盖）。
 */
export function initialExpanded(path: LevelPath, overrides?: Partial<Record<ReadingLevel, boolean>>): Record<string, boolean> {
  const out: Record<string, boolean> = { ...DEFAULT_EXPANDED };
  // 所在一层展开
  out[path.level] = true;
  if (overrides) {
    for (const [k, v] of Object.entries(overrides)) {
      if (k === "overview") continue; // ★ L0 不可被覆盖为收起
      if (typeof v === "boolean") out[k] = v;
    }
  }
  // 最后再强制 L0（防 overrides 与 DEFAULT_EXPANDED 之外的写法漏改）
  out.overview = true;
  return out;
}

/**
 * 断言默认收起契约（供测试与运行时自检）。
 * 返回违规描述（空 = 合规）。
 */
export function checkDefaultCollapsedContract(): string[] {
  const bad: string[] = [];
  for (const m of LEVEL_META) {
    const want = m.depth === 0;
    if (DEFAULT_EXPANDED[m.id] !== want) {
      bad.push(`层级 ${m.id} 默认展开=${DEFAULT_EXPANDED[m.id]}，期望 ${want}（仅 L0 展开）`);
    }
  }
  return bad;
}
