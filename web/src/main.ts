/**
 * src/main.ts —— 页面外壳（**副作用**层）
 *
 * 装配规则：  [用户交互] → M-FILTER → QueryState → M-QUERY → DataContract → M-RENDER → DOM
 *
 * ★ 本文件只做「副作用」：读/写 URL、发请求、画 DOM。
 *   所有可测的**纯逻辑**（层级路径、面包屑、导航、模板套用）都在
 *   src/shell/app.ts —— 那里不碰 DOM/fetch，能被穷举单测。
 *   这样「层级会不会跳错」这类正确性问题不依赖浏览器就能验证。
 *
 * 装配层纪律：
 *   * 不实现任何业务公式；
 *   * 不修改 QueryState / DataContract 的内容（只传递）；
 *   * 把 M-QUERY 的 fail-closed 错误如实呈现给用户（不吞、不降级为 0）。
 */

import type { QueryState, ReadingLevel } from "./contracts/query-state.js";
import type { DataContract } from "./contracts/data-contract.js";
import type { ViewTemplate } from "./contracts/view-template.js";
import { QueryClient, QueryError, fetchMe, type MeView } from "./query/client.js";
import { renderDataContract, gateColumns, type ViewVM } from "./render/render.js";
import { MISSING_TEXT } from "./contracts/data-contract.js";
import { LEVEL_META, metaOf, type LevelPath } from "./report/levels.js";
import {
  applyTemplateToState,
  buildShellVM,
  downgradeNotice,
  navigate,
  queryStateFromPath,
  type Nav,
  type ShellVM,
} from "./shell/app.js";

/** 渲染层判定的业务数值字段（D7 二次防线用）。 */
const BUSINESS_VALUE_KEYS = ["gp", "cogs", "net_contrib", "gmp", "rev"];

/** 应用状态（仅装配层持有）。 */
interface AppState {
  /** 当前层级路径（真源在 URL；这里是它最近一次的投影） */
  path: LevelPath;
  me: MeView | null;
  vm: ViewVM | null;
  /** 最近一次查询结果（面包屑标签来源） */
  dc: DataContract | null;
  /** 已套用的模板（用于回显） */
  applied: ViewTemplate | null;
  /** 由模板带来的 QueryState 覆盖（为 null 时用层级路径组装） */
  templateQS: QueryState | null;
  /** fail-closed 错误信息（非 null 时展示为告警，而非假数据） */
  error: { reason: string; detail: string } | null;
  busy: boolean;
}

const state: AppState = {
  path: { level: "overview" },
  me: null,
  vm: null,
  dc: null,
  applied: null,
  templateQS: null,
  error: null,
  busy: false,
};

/** 从宿主页面取身份（生产由网关注入；开发期允许 ?account= 便于本地联调）。 */
function resolveAccount(): string {
  const el = document.querySelector('meta[name="spark-account"]') as HTMLMetaElement | null;
  if (el?.content) return el.content;
  const q = new URLSearchParams(globalThis.location?.search ?? "");
  return q.get("account") ?? "";
}

const account = resolveAccount();
const client = new QueryClient({ account });

/** 组装一次查询（M-FILTER 出口）。模板生效时，模板的 QueryState 优先。 */
function currentQueryState(): QueryState {
  if (state.templateQS) return state.templateQS;
  return queryStateFromPath(state.path);
}

/** 执行一轮：M-FILTER → M-QUERY → M-RENDER。 */
async function refresh(): Promise<void> {
  state.busy = true;
  state.error = null;
  paint();

  const qs = currentQueryState();
  try {
    const dc = await client.run(qs, JSON.stringify(qs));

    // M-RENDER 的二次门控（服务端已先门控）
    const view = state.me ?? { maxLevel: "L1" as const, canViewBusinessValues: false, modules: [], dimensions: {}, account, dataUseGroups: [] };
    const gated: DataContract = {
      ...dc,
      columns: gateColumns(dc.columns, {
        maxLevel: view.maxLevel,
        canViewBusinessValues: view.canViewBusinessValues,
        businessValueKeys: BUSINESS_VALUE_KEYS,
      }),
    };

    state.dc = gated;
    const shell = currentShell();
    state.vm = renderDataContract(gated, shell.expanded);
  } catch (e) {
    // ★ fail-closed：把错误如实呈现，绝不填入假数据
    if (e instanceof QueryError) {
      state.error = { reason: humanReason(e.reason), detail: e.detail };
    } else {
      state.error = { reason: "未知错误", detail: e instanceof Error ? e.message : String(e) };
    }
    state.vm = null;
    state.dc = null;
  } finally {
    state.busy = false;
    paint();
  }
}

/** 从当前状态派生外壳描述（不做副作用）。 */
function currentShell(): ShellVM {
  return buildShellVM(locationSearch(), {
    levels: state.dc?.levels,
    appliedTemplate: state.applied,
    identity: state.me ? `${state.me.account} (${state.me.maxLevel}${state.me.canViewBusinessValues ? "" : " · 不含业务数值"})` : "未登录",
    canViewBusinessValues: state.me?.canViewBusinessValues ?? false,
  });
}

/** 读原始 search（无 DOM 时为 ""，便于在测试/SSR 下不炸）。 */
function locationSearch(): string {
  return globalThis.location?.search ?? "";
}

/** 把机器原因翻译成用户能懂的说明（不隐藏严重性）。 */
function humanReason(r: QueryError["reason"]): string {
  switch (r) {
    case "STORE_NOT_CONFIGURED":
      return "数据库未接入（后端降级模式）—— 当前没有数据，而不是数据为 0";
    case "BUCKET_NOT_FRESH":
      return "预计算桶未就绪（STALE/未注册）—— 为避免展示错误数字，已拒绝查询";
    case "FORBIDDEN":
      return "当前账号无权限访问该数据";
    case "UNAUTHENTICATED":
      return "缺少身份信息（请通过网关/SSO 登录）";
    case "MALFORMED_REQUEST":
      return "筛选条件不合法，请调整后重试";
    case "PROTOCOL_MISMATCH":
      return "前后端契约版本不一致，请刷新或联系管理员";
    case "NETWORK":
      return "网络不可达";
    default:
      return "查询失败";
  }
}

// ───────────────────────────── 导航（唯一写 URL 的地方） ─────────────────────────────

/**
 * 施加一次导航：算 URL → 写 history → 从 URL 重新投影状态 → 重新取数。
 *
 * ★ 刻意「先写 URL 再从 URL 读回」：让 URL 保持唯一真源。
 *   若直接改 state.path，就会出现「内存与 URL 不一致」的分裂，
 *   表现为刷新后回到别的层级 —— 这是这类页面最常见的 bug。
 */
function go(nav: Nav): void {
  const res = navigate(state.path, nav);

  if (nav.kind === "applyTemplate") {
    const applied = applyTemplateToState(nav.template);
    state.applied = nav.template;
    state.templateQS = applied.queryState;
    state.path = applied.path;
    writeUrl(applied.path);
    void refresh();
    return;
  }

  // 层级跳转：清掉模板带来的 QueryState 覆盖（用户已开始手动导航，
  // 继续套着模板的筛选会让「点了却没变化」)
  state.templateQS = null;
  state.path = res.path;
  writeUrl(res.path);
  void refresh();
}

/** 写 URL（保持其它 query 参数，如 account / tpl）。 */
function writeUrl(path: LevelPath): void {
  if (typeof history === "undefined" || !globalThis.location) return;
  const u = new URL(globalThis.location.href, "http://localhost");
  const cur = new URLSearchParams(u.search);
  const next = new URLSearchParams(res_ser(path));
  // 保留非层级参数
  for (const [k, v] of cur.entries()) {
    if (k === "level" || k === "domain" || k === "channel" || k === "store") continue;
    next.set(k, v);
  }
  u.search = next.toString();
  const rel = `${u.pathname}${u.search}${u.hash}`;
  try {
    history.pushState({ path }, "", rel);
  } catch {
    // 某些沙箱/内嵌环境禁止 pushState —— 退化为 replace，不影响取数
    try {
      history.replaceState({ path }, "", rel);
    } catch {
      /* 完全不可写：忽略（深链能力降级，但页面仍可用） */
    }
  }
}

/** 与 levelPathToQuery 同构，单独取一次以免重复 import 名字冲突。 */
function res_ser(p: LevelPath): string {
  const q = new URLSearchParams();
  q.set("level", p.level);
  const d = LEVEL_META.findIndex((m) => m.id === p.level);
  if (d >= 1 && p.domain) q.set("domain", p.domain);
  if (d >= 2 && p.channel) q.set("channel", p.channel);
  if (d >= 3 && p.store) q.set("store", p.store);
  return q.toString();
}

// ───────────────────────────── DOM 渲染（最小实现） ─────────────────────────────

function h(tag: string, attrs: Record<string, string> = {}, children: (Node | string)[] = []): HTMLElement {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") el.className = v;
    else el.setAttribute(k, v);
  }
  for (const c of children) el.append(typeof c === "string" ? document.createTextNode(c) : c);
  return el;
}

function paint(): void {
  const app = document.getElementById("app");
  if (!app) return;
  app.textContent = "";

  const shell = currentShell();

  // 顶栏
  const bar = h("header", { class: "bar" }, [
    h("strong", {}, ["spark-cicada"]),
    h("span", { class: "sep" }, ["·"]),
    h("span", {}, [shell.header.identity]),
  ]);

  // 面包屑（逐级可返回；当前层不可点）
  const crumbNav = h("nav", { class: "breadcrumb", "aria-label": "层级路径" });
  shell.crumbs.forEach((c, i) => {
    if (i > 0) crumbNav.append(h("span", { class: "bc-sep" }, ["/"]));
    if (c.clickable) {
      const b = h("button", { class: "bc-item", "data-depth": String(c.depth) }, [c.text]);
      crumbNav.append(b);
    } else {
      crumbNav.append(h("span", { class: "bc-item current", "aria-current": "page" }, [c.text]));
    }
  });
  crumbNav.addEventListener("click", (ev) => {
    const t = (ev.target as HTMLElement).closest("button[data-depth]") as HTMLButtonElement | null;
    if (!t) return;
    go({ kind: "jump", depth: Number(t.dataset.depth) });
  });

  // 层级切换（仅「当前层及更浅层」可点）
  const tabs = h("nav", { class: "tabs" }, shell.tabs.map((t) =>
    h("button", {
      class: t.active ? "tab active" : t.reachable ? "tab" : "tab locked",
      "data-level": t.level,
      ...(t.reachable ? {} : { disabled: "disabled", title: "请逐层下钻（深层需要先选定上层）" }),
    }, [t.short]),
  ));
  tabs.addEventListener("click", (ev) => {
    const t = (ev.target as HTMLElement).closest("button[data-level]") as HTMLButtonElement | null;
    if (!t || t.disabled) return;
    go({ kind: "setLevel", level: t.dataset.level as ReadingLevel });
  });

  app.append(bar, crumbNav, tabs);

  // 降级提示（URL 参数不完整时，绝不静默）
  const notice = downgradeNotice(shell);
  if (notice) app.append(h("div", { class: "notice", role: "status" }, [notice]));

  // 模板回显
  if (shell.appliedTemplateName) {
    app.append(h("div", { class: "hint small" }, [`当前视图来自模板「${shell.appliedTemplateName}」`]));
  }

  // 状态区
  if (state.busy) app.append(h("div", { class: "hint" }, ["加载中…"]));
  if (state.error) {
    // ★ 错误用醒目样式，绝不静默
    app.append(h("div", { class: "error", role: "alert" }, [
      h("strong", {}, [`⚠ ${state.error.reason}`]),
      h("div", { class: "detail" }, [state.error.detail]),
    ]));
  }

  // 标题
  app.append(h("h2", { class: "ltitle" }, [`${shell.title} · ${shell.crumbText}`]));

  // 层级（默认收起；L0 常驻）
  const vm = state.vm;
  if (vm) {
    const lvBox = h("section", { class: "levels" });
    for (const l of vm.levels) {
      const row = h("div", { class: l.level === "L0" ? "level l0" : "level" }, [
        h("button", {
          class: "lv-toggle",
          ...(l.level === "L0" ? { disabled: "disabled", title: "总览常驻展开（默认收起契约）" } : {}),
        }, [`${l.expanded ? "▾" : "▸"} ${l.level} ${l.label}`]),
        h("span", { class: "meta" }, [`子项 ${l.childCount}`, l.expanded ? "（展开）" : "（已收起）"]),
      ]);
      lvBox.append(row);
    }
    app.append(lvBox);

    // 表格
    if (vm.empty) {
      app.append(h("div", { class: "hint" }, ["该条件下无数据行"]));
    } else {
      const table = h("table", { class: "grid" });
      const thead = h("thead");
      const htr = h("tr");
      for (const c of vm.table.columns) htr.append(h("th", { class: c.kind }, [c.label]));
      thead.append(htr);
      table.append(thead);

      const tbody = h("tbody");
      for (const r of vm.table.rows) {
        const tr = h("tr");
        for (const c of r.cells) {
          const td = h("td", { class: c.missing ? `${c.align} missing` : c.align }, [c.text]);
          if (c.gapReason) td.title = c.gapReason;
          if (c.algoId) td.dataset.algo = c.algoId;
          tr.append(td);
        }

        // ★ 行内下钻：点击行即「以该行作为选择下钻一层」（§9.3）。
        //   到 L4 后不再下钻（normalizePath 会原样返回）。
        if (shell.canDrillDown) {
          tr.classList.add("drillable");
          tr.title = "点击下钻到下一层";
        }
        tbody.append(tr);
      }
      table.append(tbody);

      // 行点击 → 下钻（取该行第一个非数值单元格作为选择 id）
      table.addEventListener("click", (ev) => {
        if (!shell.canDrillDown) return;
        const tr = (ev.target as HTMLElement).closest("tr");
        if (!tr || tr.parentElement?.tagName !== "TBODY") return;
        const idx = [...(tr.parentElement?.children ?? [])].indexOf(tr);
        const vmRow = state.vm?.table.rows[idx];
        if (!vmRow) return;
        const first = vmRow.cells.find((c) => c.align === "left" && !c.missing);
        if (!first) return;
        go({ kind: "drillDown", id: first.text });
      });

      app.append(table);

      if (vm.table.gaps.length > 0) {
        app.append(h("footer", { class: "gaps" }, [
          h("strong", {}, ["缺失项（待接入）: "]),
          h("span", {}, [vm.table.gaps.map((g) => `${g.field}@${g.slot}(${g.coverage}/${g.gate})`).join("  ")]),
        ]));
      }
      app.append(h("div", { class: "hint small" }, [
        `queryHash=${vm.table.queryHash} · precomputed=${vm.table.precomputed} · ${vm.table.generatedAt}`,
      ]));
    }
  }

  // 缺失文案自检（把契约暴露给运维）
  app.append(h("div", { class: "hint small" }, [`缺失文案真源：${MISSING_TEXT}`]));
}

// ───────────────────────────── 启动 / 前进后退 ─────────────────────────────

/** 从 URL 重新投影状态（刷新 / 前进后退都走这里）。 */
function syncFromUrl(): void {
  const shell = buildShellVM(locationSearch());
  state.path = shell.path;
  // 手改 URL 时模板上下文失效：URL 里没有 tpl 就认为不在模板视图
  const tplId = new URLSearchParams(locationSearch()).get("tpl");
  if (!tplId) {
    state.applied = null;
    state.templateQS = null;
  }
  void refresh();
}

/** 启动。 */
async function boot(): Promise<void> {
  try {
    state.me = await fetchMe({ account });
  } catch {
    state.me = null; // 身份不可得时仍允许查看（服务端会 403）
  }

  if (typeof window !== "undefined") {
    window.addEventListener("popstate", () => syncFromUrl());
  }

  // 首屏：URL 是唯一真源（深链直达）
  syncFromUrl();
}

if (typeof document !== "undefined" && document.getElementById("app")) {
  void boot();
}

// 供未来模块（模板中心 / 管理台）复用的外部入口
export { go, applyTemplateToState };
export type { Nav };
