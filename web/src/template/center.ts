/**
 * src/template/center.ts —— M-TEMPLATE 模板中心
 *
 * 职责：视图模板的保存 / 套用 / 分享 / 推荐（docs/01 §10、docs/02 M-TEMPLATE）。
 *
 * ══════════════════════════════════════════════════════════════════════════
 * ★★ 本模块与 M-FILTER / M-RENDER 的边界（由 scripts/layering-gate.mjs 强制）：
 *
 *   1. **不发起网络请求**。模板的持久化由 M-QUERY 层（src/query）负责，
 *      本模块只产出「要保存/套用/删除的模板对象」与「套用后的视图偏好」。
 *      这样模板中心可以被纯单测穷举，不依赖 fetch 替身。
 *
 *   2. **不做业务换算**。模板里唯一与数据有关的字段是 QueryState ——
 *      它由 M-FILTER 构造、被完整透传，本模块**不解读**它的内部语义，
 *      只保证「原样存、原样还」。
 *
 *   3. **不含权限判断**。谁能看到什么档位的模板，由后端的档位规则决定；
 *      本模块只把 scope 如实带上。前端的「可见性」仅用于**展示分组**，
 *      绝不作为访问控制的依据（那是典型的「前端权限」反模式）。
 * ══════════════════════════════════════════════════════════════════════════
 *
 * ★ 三条纪律：
 *
 *   1. **套用必须是「替换」而非「合并」。** 模板的语义是「重建整个视图」。
 *      若与当前状态合并，用户会得到「模板 × 当前残留」的四不像状态，
 *      而且无法用任何模板精确复现（因为复现结果取决于套用前的状态）。
 *
 *   2. **保存前必须规范化。** 同一逻辑状态的不同写法（字段顺序、undefined、
 *      列 order 重复）会让「同一个视图」在模板列表里长出多条 ——
 *      用户会以为是 bug。规范化集中在一处（normalizeTemplate）。
 *
 *   3. **默认模板不编造。** 没有默认就返回 null，绝不在前端「挑一个」。
 *      否则用户会莫名被套上一个不属于自己口径的视图，还以为是自己设的。
 */

import {
  VIEW_TEMPLATE_VERSION,
  type ColumnPref,
  type LayoutPref,
  type TemplateScope,
  type ViewTemplate,
} from "../contracts/view-template.js";
import type { QueryState } from "../contracts/query-state.js";

/** 模板契约版本（与后端 template.Version 对齐）。 */
export const TEMPLATE_VERSION = VIEW_TEMPLATE_VERSION;

/** 档位的展示元数据（仅用于 UI 分组与提示，**不是**访问控制）。 */
export const SCOPE_META: Record<TemplateScope, { label: string; hint: string }> = {
  personal: { label: "个人", hint: "只有你自己能看到和使用" },
  team: { label: "团队", hint: "你所在部门/组可以看到的统一口径" },
  system: { label: "系统", hint: "全体可见的默认视图（需管理员）" },
};

/** 档位展示顺序（个人 → 团队 → 系统，由近及远）。 */
export const SCOPE_ORDER: readonly TemplateScope[] = ["personal", "team", "system"];

// ───────────────────────────── 规范化 ─────────────────────────────

/**
 * 规范化列偏好：去重（同 key 保留最后一条）、重排 order（0..n-1 连续）、
 * 稳定按原 order 排序。返回**新数组**，不改入参。
 */
export function normalizeColumns(cols: readonly ColumnPref[]): ColumnPref[] {
  const byKey = new Map<string, ColumnPref>();
  for (const c of cols) {
    if (!c.key) continue;
    const prev = byKey.get(c.key);
    // 同 key 保留最后一条（后设覆盖），但继承更早的 order（保持用户直觉位置）
    byKey.set(c.key, prev ? { ...c, order: prev.order } : { ...c });
  }
  const arr = [...byKey.values()];
  // 稳定排序：order 相同时按 key 字典序，保证结果确定
  arr.sort((a, b) => (a.order !== b.order ? a.order - b.order : a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
  // 重排为连续 order，避免「0, 5, 9」这类稀疏值在多次保存后越飘越远
  return arr.map((c, i) => ({ ...c, order: i }));
}

/** 规范化布局：保证 expanded 存在（空对象而非 undefined）。 */
export function normalizeLayout(layout: LayoutPref | undefined): LayoutPref {
  const out: LayoutPref = { expanded: { ...(layout?.expanded ?? {}) } };
  if (layout?.sidebarCollapsed !== undefined) out.sidebarCollapsed = layout.sidebarCollapsed;
  if (layout?.panelPositions) out.panelPositions = { ...layout.panelPositions };
  return out;
}

/**
 * 规范化整个模板（保存前调用）。
 *
 * ★ 只做**形状**规范化，不动 queryState 的内容 ——
 *   queryState 是 M-FILTER 的产物，本模块无权改写其语义。
 */
export function normalizeTemplate(t: ViewTemplate): ViewTemplate {
  return {
    ...t,
    name: t.name.trim(),
    columns: normalizeColumns(t.columns),
    layout: normalizeLayout(t.layout),
    useCount: Math.max(0, Math.floor(t.useCount)),
  };
}

/** 校验模板是否可保存。返回问题列表（空 = 可保存）。 */
export function validateTemplate(t: {
  name?: string;
  scope?: string;
  page?: string;
  queryState?: unknown;
}): string[] {
  const problems: string[] = [];
  if (!t.name || t.name.trim() === "") {
    // 「自命名」是需求能力；空名会让模板列表变成一堆无标识条目
    problems.push("请给模板起个名字");
  }
  if (t.scope !== "personal" && t.scope !== "team" && t.scope !== "system") {
    problems.push("请选择可见范围（个人/团队/系统）");
  }
  if (!t.page) {
    problems.push("模板必须归属于某个页面");
  }
  if (t.queryState === undefined || t.queryState === null) {
    problems.push("模板缺少筛选状态（QueryState）");
  }
  return problems;
}

// ───────────────────────────── 套用（替换语义） ─────────────────────────────

/** 套用模板后得到的视图偏好（供装配层重建）。 */
export interface AppliedView {
  /** 由模板带来的完整 QueryState（**整体替换**当前状态） */
  queryState: QueryState;
  /** 列偏好（已规范化） */
  columns: ColumnPref[];
  /** 布局偏好（已规范化） */
  layout: LayoutPref;
  /** 模板 id + 名称（用于回显「当前来自哪个模板」） */
  templateId: string;
  templateName: string;
}

/**
 * 套用模板。
 *
 * ★ 「替换」而非「合并」：见文件头纪律 1。
 *   返回的是**新对象**，调用方必须整体替换自己的视图状态；
 *   若调用方选择合并，那是调用方的 bug，不是本函数的语义。
 *
 * ★ 深拷贝 queryState：模板对象常被缓存与复用，若直接引用其内部对象，
 *   用户之后在筛选栏改动会**污染模板缓存**，进而污染其它标签页。
 *   structuredClone 在无该 API 的环境（老浏览器/测试）下回退到 JSON 往返。
 */
export function applyTemplate(t: ViewTemplate): AppliedView {
  return {
    queryState: deepClone(t.queryState) as QueryState,
    columns: normalizeColumns(t.columns),
    layout: normalizeLayout(t.layout),
    templateId: t.id,
    templateName: t.name,
  };
}

function deepClone<T>(v: T): T {
  if (typeof structuredClone === "function") return structuredClone(v);
  return JSON.parse(JSON.stringify(v)) as T;
}

// ───────────────────────────── 保存 / 派生 ─────────────────────────────

/** 生成模板 id（前端本地生成；服务端按 id upsert）。 */
export function newTemplateId(page: string, now: Date = new Date()): string {
  const ts = now.getTime().toString(36);
  const rand = Math.random().toString(36).slice(2, 8);
  const p = page.replace(/[^a-zA-Z0-9]+/g, "_").replace(/^_+|_+$/g, "") || "page";
  return `tpl_${p}_${ts}_${rand}`;
}

/** 从「当前视图状态」派生出待保存的模板草稿。 */
export interface DraftInput {
  page: string;
  name: string;
  scope: TemplateScope;
  owner: string;
  queryState: QueryState;
  columns: readonly ColumnPref[];
  layout: LayoutPref | undefined;
  /** 若是在既有模板上「另存为」，传其 id 以保留（否则新建） */
  id?: string;
  isDefault?: boolean;
  now?: Date;
}

export function makeDraft(input: DraftInput): ViewTemplate {
  const now = input.now ?? new Date();
  const iso = now.toISOString();
  const t: ViewTemplate = {
    id: input.id ?? newTemplateId(input.page, now),
    name: input.name,
    scope: input.scope,
    owner: input.owner,
    page: input.page,
    queryState: deepClone(input.queryState),
    columns: normalizeColumns(input.columns),
    layout: normalizeLayout(input.layout),
    createdAt: iso,
    updatedAt: iso,
    useCount: 0,
  };
  if (input.isDefault) t.isDefault = true;
  return t;
}

/**
 * 「另存为」：基于既有模板派生一份新模板（新 id、清零使用次数、不带默认标记）。
 *
 * ★ 刻意**不继承** isDefault 与 useCount：
 *   - isDefault 继承会让「另存为」意外抢走默认位（用户只想复制一份）；
 *   - useCount 继承会让新模板凭空排在常用前列，推荐失真。
 */
export function forkTemplate(t: ViewTemplate, overrides: Partial<DraftInput> = {}): ViewTemplate {
  return makeDraft({
    page: overrides.page ?? t.page,
    name: overrides.name ?? `${t.name} 副本`,
    scope: overrides.scope ?? t.scope,
    owner: overrides.owner ?? t.owner,
    queryState: overrides.queryState ?? (t.queryState as QueryState),
    columns: overrides.columns ?? t.columns,
    layout: overrides.layout ?? t.layout,
    id: undefined, // 一律新 id
  });
}

// ───────────────────────────── 列表：分组 / 排序 / 推荐 ─────────────────────────────

/** 按档位分组（UI 展示用，不代表可见性判断）。 */
export function groupByScope(list: readonly ViewTemplate[]): Record<TemplateScope, ViewTemplate[]> {
  const out: Record<TemplateScope, ViewTemplate[]> = { personal: [], team: [], system: [] };
  for (const t of list) {
    if (out[t.scope]) out[t.scope].push(t);
  }
  return out;
}

/**
 * 按使用次数倒序 + 更新时间兜底 + id 最终兜底（保证稳定，列表不跳动）。
 * 返回新数组。
 */
export function sortByUse(list: readonly ViewTemplate[]): ViewTemplate[] {
  return [...list].sort((a, b) => {
    if (a.useCount !== b.useCount) return b.useCount - a.useCount;
    const at = a.updatedAt ?? "";
    const bt = b.updatedAt ?? "";
    if (at !== bt) return at < bt ? 1 : -1; // 新的在前
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

/**
 * 智能推荐：在模板中心顶部展示的「常用」列表。
 *
 * 规则：仅对**同一页面**的模板排序，过滤掉本页不相关的模板；
 * 使用次数为 0 的也保留（新用户需要看到自己刚存的模板），
 * 但排在所有用过的模板之后（由 sortByUse 自然满足）。
 */
export function recommend(list: readonly ViewTemplate[], page: string, limit = 5): ViewTemplate[] {
  const scoped = list.filter((t) => t.page === page);
  return sortByUse(scoped).slice(0, Math.max(0, limit));
}

/**
 * 从列表里选出「该页默认模板」。
 *
 * ★ 优先级：个人 > 团队 > 系统（与后端 DefaultsForPage 一致）。
 *   前端也实现一遍是为了「离线/首屏」时不必等接口 —— 但**必须**与后端
 *   保持同一优先级，否则会出现「刷新前后套用的模板不一样」的灵异现象。
 *   两者的契约由 web/test/template.test.mjs 的用例钉住。
 *
 * 没有默认 ⇒ 返回 null（不编造，见纪律 3）。
 */
export function pickDefault(list: readonly ViewTemplate[], page: string): ViewTemplate | null {
  const scoped = list.filter((t) => t.page === page && t.isDefault === true);
  for (const scope of SCOPE_ORDER) {
    const hit = scoped.find((t) => t.scope === scope);
    if (hit) return hit;
  }
  return null;
}

// ───────────────────────────── 深链 ─────────────────────────────

/**
 * 把模板 id 追加到页面 URL（模板天然可深链，docs/01 §10.4）。
 *
 * ★ 用 URL API 而非字符串拼接：拼接会在已有 query/hash 的 URL 上出错
 *   （例如 `?a=1` + `?tpl=x`）。
 */
export function templateUrl(baseUrl: string, templateId: string): string {
  const u = new URL(baseUrl, "http://localhost");
  u.searchParams.set("tpl", templateId);
  // 返回相对形式（去掉 hosts，否则会带上占位 host）
  return `${u.pathname}${u.search}${u.hash}`;
}

/** 从 URL 里取出模板 id（无则 null）。 */
export function templateIdFromUrl(url: string): string | null {
  const u = new URL(url, "http://localhost");
  const v = u.searchParams.get("tpl");
  return v && v.trim() !== "" ? v : null;
}

// ───────────────────────────── 分享 ─────────────────────────────

/** 判断某档位是否支持分享链接。 */
export function isShareable(t: ViewTemplate): boolean {
  // 个人档不产生分享链接 —— 分享出去也打不开（对方无权限），
  // 生成一个「能点但只能用的人自己看」的链接只会制造困惑。
  return t.scope === "team" || t.scope === "system";
}

/** 生成分享信息（供 UI 展示；不做网络请求）。 */
export interface ShareInfo {
  url: string;
  shareable: boolean;
  reason?: string;
}

export function shareInfo(t: ViewTemplate, pageUrl: string): ShareInfo {
  if (!isShareable(t)) {
    return {
      url: "",
      shareable: false,
      reason: "个人模板不生成分享链接 —— 其他人无权打开，链接没有意义",
    };
  }
  return { url: templateUrl(pageUrl, t.id), shareable: true };
}
