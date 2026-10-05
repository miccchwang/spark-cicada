/**
 * src/main.ts —— 页面外壳（装配层）
 *
 * ★ 这是**唯一**允许同时引用 M-FILTER / M-QUERY / M-RENDER 的模块。
 * 它的职责只有「把三段接起来」：
 *
 *     [用户交互] → M-FILTER → QueryState → M-QUERY → DataContract → M-RENDER → DOM
 *
 * 装配层纪律：
 *   * 不实现任何业务公式；
 *   * 不修改 QueryState / DataContract 的内容（只传递）；
 *   * 把 M-QUERY 的 fail-closed 错误如实呈现给用户（不吞、不降级为 0）。
 */

import { DEFAULT_TIMEZONE, buildQueryState, makeDims, makePage, makeTimeRange, describeQueryState } from "./filter/query-state.js";
import type { ReadingLevel, QueryState } from "./contracts/query-state.js";
import { QueryClient, QueryError, fetchMe, type MeView } from "./query/client.js";
import { renderDataContract, gateColumns, type ViewVM } from "./render/render.js";
import { MISSING_TEXT } from "./contracts/data-contract.js";

/** 渲染层判定的业务数值字段（D7 二次防线用）。 */
const BUSINESS_VALUE_KEYS = ["gp", "cogs", "net_contrib", "gmp", "rev"];

/** 应用状态（仅装配层持有）。 */
interface AppState {
  level: ReadingLevel;
  me: MeView | null;
  vm: ViewVM | null;
  /** fail-closed 错误信息（非 null 时展示为告警，而非假数据） */
  error: { reason: string; detail: string } | null;
  busy: boolean;
}

const state: AppState = { level: "store", me: null, vm: null, error: null, busy: false };

/** 从宿主页面取身份（生产由网关注入；开发期允许 ?account= 便于本地联调）。 */
function resolveAccount(): string {
  const el = document.querySelector('meta[name="spark-account"]') as HTMLMetaElement | null;
  if (el?.content) return el.content;
  const q = new URLSearchParams(globalThis.location?.search ?? "");
  return q.get("account") ?? "";
}

const account = resolveAccount();
const client = new QueryClient({ account });

/** 组装一次查询（M-FILTER 出口）。 */
function currentQueryState(level: ReadingLevel): QueryState {
  return buildQueryState({
    time: makeTimeRange("preset", { preset: "mtd", grain: "month", timezone: DEFAULT_TIMEZONE }),
    dims: makeDims(level),
    page: makePage(0, 200),
  });
}

/** 执行一轮：M-FILTER → M-QUERY → M-RENDER。 */
async function refresh(): Promise<void> {
  state.busy = true;
  state.error = null;
  paint();

  const qs = currentQueryState(state.level);
  try {
    const dc = await client.run(qs, describeQueryState(qs));

    // M-RENDER 的二次门控（服务端已先门控）
    const view = state.me ?? { maxLevel: "L1" as const, canViewBusinessValues: false, modules: [], dimensions: {}, account, dataUseGroups: [] };
    const gated = { ...dc, columns: gateColumns(dc.columns, {
      maxLevel: view.maxLevel,
      canViewBusinessValues: view.canViewBusinessValues,
      businessValueKeys: BUSINESS_VALUE_KEYS,
    }) };

    state.vm = renderDataContract(gated);
  } catch (e) {
    // ★ fail-closed：把错误如实呈现，绝不填入假数据
    if (e instanceof QueryError) {
      state.error = { reason: humanReason(e.reason), detail: e.detail };
    } else {
      state.error = { reason: "未知错误", detail: e instanceof Error ? e.message : String(e) };
    }
    state.vm = null;
  } finally {
    state.busy = false;
    paint();
  }
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

  // 顶栏
  const bar = h("header", { class: "bar" }, [
    h("strong", {}, ["spark-cicada"]),
    h("span", { class: "sep" }, ["·"]),
    h("span", {}, [state.me ? `${state.me.account} (${state.me.maxLevel}${state.me.canViewBusinessValues ? "" : " · 不含业务数值"})` : "未登录"]),
  ]);

  // 层级切换（五层阅览）
  const levels: ReadingLevel[] = ["overview", "domain", "channel", "store", "sku"];
  const tabs = h("nav", { class: "tabs" }, levels.map((lv) =>
    h("button", {
      class: lv === state.level ? "tab active" : "tab",
      "data-level": lv,
    }, [lv]),
  ));
  tabs.addEventListener("click", (ev) => {
    const t = (ev.target as HTMLElement).closest("button[data-level]") as HTMLButtonElement | null;
    if (!t) return;
    state.level = t.dataset.level as ReadingLevel;
    void refresh();
  });

  app.append(bar, tabs);

  // 状态区
  if (state.busy) {
    app.append(h("div", { class: "hint" }, ["加载中…"]));
  }
  if (state.error) {
    // ★ 错误用醒目样式，绝不静默
    app.append(h("div", { class: "error", role: "alert" }, [
      h("strong", {}, [`⚠ ${state.error.reason}`]),
      h("div", { class: "detail" }, [state.error.detail]),
    ]));
  }

  // 层级（默认收起；L0 常驻）
  const vm = state.vm;
  if (vm) {
    const lvBox = h("section", { class: "levels" });
    for (const l of vm.levels) {
      const row = h("div", { class: "level" }, [
        h("button", { class: "lv-toggle" }, [`${l.expanded ? "▾" : "▸"} ${l.level} ${l.label}`]),
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
        tbody.append(tr);
      }
      table.append(tbody);
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

/** 启动。 */
async function boot(): Promise<void> {
  try {
    state.me = await fetchMe({ account });
  } catch {
    state.me = null; // 身份不可得时仍允许查看（服务端会 403）
  }
  await refresh();
}

if (typeof document !== "undefined" && document.getElementById("app")) {
  void boot();
}
