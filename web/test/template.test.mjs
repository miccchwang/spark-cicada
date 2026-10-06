/**
 * test/template.test.mjs —— M-TEMPLATE 模板中心：规范化 / 套用 / 分组 / 默认 / 深链
 *
 * 运行：node --test test/*.test.mjs（或 npm test）
 *
 * ★ 本测试要钉死的核心语义（纯逻辑层，无需网络）：
 *   1. 套用是**替换**而非**合并** —— 且不污染模板缓存（深拷贝）。
 *   2. 规范化是幂等的 —— 同一逻辑状态不会在列表里长出多条。
 *   3. 默认选取优先级 个人 > 团队 > 系统，且无默认时**返回 null**（不编造）。
 *   4. 深链：模板 id 在 URL 上往返无损。
 *   5. 排序稳定 —— 同样的输入永远同样的顺序（列表不会自己跳动）。
 */

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, existsSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL, fileURLToPath } from "node:url";

// WEB 已经是 web/ 目录本身（本文件位于 web/test/），不要再 join "web"。
const WEB = fileURLToPath(new URL("..", import.meta.url));
const OUT = join(WEB, ".test-build");

let M;

before(async () => {
  const esbuild = await import("esbuild");
  if (!existsSync(OUT)) mkdirSync(OUT, { recursive: true });
  // bundle: true —— center.ts 依赖 contracts/view-template.ts，
  // 必须打包进来才能 import（否则 data：/ 相对路径都解析不了）。
  await esbuild.build({
    entryPoints: [join(WEB, "src", "template", "center.ts")],
    outfile: join(OUT, "template-center.mjs"),
    format: "esm",
    bundle: true,
    platform: "neutral",
  });
  M = await import(pathToFileURL(join(OUT, "template-center.mjs")).href);
});

// ───────────────────────────── 帮手 ─────────────────────────────

function qs(preset = "mtd") {
  return {
    v: "1.0",
    time: { mode: "preset", preset, grain: "month", timezone: "Asia/Bangkok" },
    filters: [],
    dims: { level: "store" },
  };
}

function mk(opts = {}) {
  return {
    id: opts.id ?? "tpl_a",
    name: opts.name ?? "模板 A",
    scope: opts.scope ?? "personal",
    owner: opts.owner ?? "u.alice",
    page: opts.page ?? "/report",
    queryState: opts.queryState ?? qs(),
    columns: opts.columns ?? [
      { key: "month", visible: true, order: 0 },
      { key: "gmv", visible: true, order: 1 },
    ],
    layout: opts.layout ?? { expanded: { L0: true } },
    createdAt: opts.createdAt ?? "2026-10-01T00:00:00Z",
    updatedAt: opts.updatedAt ?? "2026-10-01T00:00:00Z",
    useCount: opts.useCount ?? 0,
    ...(opts.isDefault !== undefined ? { isDefault: opts.isDefault } : {}),
  };
}

// ───────────────────────────── 1. 规范化 ─────────────────────────────

test("normalizeColumns: 去重（同 key 保留最后）+ order 连续重排", () => {
  const out = M.normalizeColumns([
    { key: "a", visible: true, order: 0 },
    { key: "b", visible: true, order: 5 },
    { key: "a", visible: false, order: 9 }, // 覆盖前一条 a
  ]);
  assert.deepEqual(out.map((c) => c.key), ["a", "b"]);
  assert.deepEqual(out.map((c) => c.order), [0, 1], "order 应重排为连续 0..n-1");
  assert.equal(out[0].visible, false, "同 key 应保留最后一条的可见性");
});

test("normalizeColumns: 幂等（跑两次结果一致）", () => {
  const once = M.normalizeColumns([
    { key: "x", visible: true, order: 7 },
    { key: "y", visible: true, order: 3 },
  ]);
  const twice = M.normalizeColumns(once);
  assert.deepEqual(twice, once, "★ 规范化必须幂等，否则每次保存都会漂移");
});

test("normalizeColumns: 不修改入参", () => {
  const input = [{ key: "a", visible: true, order: 0 }];
  const snapshot = JSON.stringify(input);
  M.normalizeColumns(input);
  assert.equal(JSON.stringify(input), snapshot, "★ 不得就地修改入参");
});

test("normalizeLayout: 缺失 expanded 补空对象", () => {
  assert.deepEqual(M.normalizeLayout(undefined).expanded, {});
  assert.deepEqual(M.normalizeLayout({}).expanded, {});
  assert.deepEqual(M.normalizeLayout({ expanded: { L1: true } }).expanded, { L1: true });
});

test("normalizeTemplate: name 去空格 + useCount 归正", () => {
  const t = M.normalizeTemplate(mk({ name: "  带空格  ", useCount: -3 }));
  assert.equal(t.name, "带空格");
  assert.equal(t.useCount, 0);
});

test("validateTemplate: 空名 / 非法档位 / 缺 page 应报问题", () => {
  assert.ok(M.validateTemplate({ name: "  ", scope: "personal", page: "/r", queryState: qs() }).length > 0);
  assert.ok(M.validateTemplate({ name: "x", scope: "public", page: "/r", queryState: qs() }).length > 0);
  assert.ok(M.validateTemplate({ name: "x", scope: "personal", page: "", queryState: qs() }).length > 0);
  assert.ok(M.validateTemplate({ name: "x", scope: "personal", page: "/r" }).length > 0);
  assert.equal(M.validateTemplate({ name: "x", scope: "personal", page: "/r", queryState: qs() }).length, 0);
});

// ───────────────────────────── 2. 套用：替换语义 + 不污染 ─────────────────────────────

test("applyTemplate: 返回完整 QueryState + 列 + 布局", () => {
  const t = mk({ queryState: qs("ytd") });
  const a = M.applyTemplate(t);
  assert.equal(a.templateId, "tpl_a");
  assert.equal(a.templateName, "模板 A");
  assert.equal(a.queryState.time.preset, "ytd");
  assert.equal(a.columns.length, 2);
  assert.deepEqual(a.layout.expanded, { L0: true });
});

test("applyTemplate: 深拷贝 —— 改动套用结果不污染模板", () => {
  const t = mk();
  const a = M.applyTemplate(t);
  // 模拟用户在筛选栏改动
  a.queryState.time.preset = "last_full_month";
  a.queryState.filters.push({ field: "channel_code", op: "in", value: ["TK-TH"] });

  assert.equal(t.queryState.time.preset, "mtd", "★ 不得污染模板持有的 QueryState");
  assert.equal(t.queryState.filters.length, 0, "★ 不得污染模板的 filters");
});

test("applyTemplate: 两次套用互相独立", () => {
  const t = mk();
  const a1 = M.applyTemplate(t);
  const a2 = M.applyTemplate(t);
  a1.columns[0].visible = false;
  assert.equal(a2.columns[0].visible, true, "★ 两次套用必须是独立副本");
});

// ───────────────────────────── 3. 派生（另存为） ─────────────────────────────

test("makeDraft: 生成新 id、时间戳、useCount=0", () => {
  const d = M.makeDraft({
    page: "/report",
    name: "我的视图",
    scope: "personal",
    owner: "u.alice",
    queryState: qs(),
    columns: [{ key: "month", visible: true, order: 0 }],
    layout: { expanded: { L0: true } },
    now: new Date("2026-10-05T12:00:00Z"),
  });
  assert.ok(d.id.startsWith("tpl_"), "id 应有 tpl_ 前缀");
  assert.equal(d.name, "我的视图");
  assert.equal(d.useCount, 0);
  assert.equal(d.createdAt, "2026-10-05T12:00:00.000Z");
  assert.equal(d.isDefault, undefined, "新建不应带默认标记");
});

test("forkTemplate: 新 id + 清零计数 + 不继承默认", () => {
  const src = mk({ id: "tpl_src", name: "源模板", useCount: 42, isDefault: true });
  const copy = M.forkTemplate(src);
  assert.notEqual(copy.id, "tpl_src", "★ 另存为必须新 id");
  assert.equal(copy.useCount, 0, "★ 另存为不得继承使用次数（否则推荐失真）");
  assert.equal(copy.isDefault, undefined, "★ 另存为不得继承默认位（否则静默抢默认）");
  assert.equal(copy.name, "源模板 副本");
});

test("forkTemplate: 可覆盖字段", () => {
  const src = mk({ scope: "personal", name: "源" });
  const copy = M.forkTemplate(src, { scope: "team", name: "团队版" });
  assert.equal(copy.scope, "team");
  assert.equal(copy.name, "团队版");
});

// ───────────────────────────── 4. 分组 / 排序 / 推荐 ─────────────────────────────

test("groupByScope: 按三档分组", () => {
  const g = M.groupByScope([
    mk({ id: "p1", scope: "personal" }),
    mk({ id: "t1", scope: "team" }),
    mk({ id: "s1", scope: "system" }),
    mk({ id: "p2", scope: "personal" }),
  ]);
  assert.equal(g.personal.length, 2);
  assert.equal(g.team.length, 1);
  assert.equal(g.system.length, 1);
  assert.deepEqual(g.personal.map((t) => t.id), ["p1", "p2"], "应保持原顺序");
});

test("sortByUse: 次数倒序，同行按更新时间新→旧，再按 id", () => {
  const list = [
    mk({ id: "a", useCount: 1 }),
    mk({ id: "b", useCount: 10 }),
    mk({ id: "c", useCount: 5 }),
  ];
  assert.deepEqual(M.sortByUse(list).map((t) => t.id), ["b", "c", "a"]);
});

test("sortByUse: 排序稳定（多次调用结果一致）", () => {
  const list = [
    mk({ id: "z", useCount: 5, updatedAt: "2026-10-01T00:00:00Z" }),
    mk({ id: "a", useCount: 5, updatedAt: "2026-10-01T00:00:00Z" }),
  ];
  for (let i = 0; i < 5; i++) {
    assert.deepEqual(M.sortByUse(list).map((t) => t.id), ["a", "z"], `第 ${i} 次不稳定`);
  }
});

test("sortByUse: 不修改入参", () => {
  const list = [mk({ id: "a", useCount: 1 }), mk({ id: "b", useCount: 9 })];
  M.sortByUse(list);
  assert.deepEqual(list.map((t) => t.id), ["a", "b"], "★ 不得就地排序入参");
});

test("recommend: 仅本页 + 限量 + 按常用", () => {
  const list = [
    mk({ id: "r1", page: "/report", useCount: 1 }),
    mk({ id: "r2", page: "/report", useCount: 9 }),
    mk({ id: "r3", page: "/report", useCount: 5 }),
    mk({ id: "p1", page: "/pnl", useCount: 99 }), // 异页，应被排除
  ];
  assert.deepEqual(M.recommend(list, "/report", 2).map((t) => t.id), ["r2", "r3"]);
});

test("recommend: 新存的模板（0 次）仍会出现", () => {
  const list = [mk({ id: "new", page: "/report", useCount: 0 })];
  assert.deepEqual(M.recommend(list, "/report", 5).map((t) => t.id), ["new"]);
});

// ───────────────────────────── 5. 默认选取（与后端同优先级） ─────────────────────────────

test("pickDefault: 优先级 个人 > 团队 > 系统", () => {
  const all = [
    mk({ id: "sys", scope: "system", isDefault: true }),
    mk({ id: "team", scope: "team", isDefault: true }),
    mk({ id: "per", scope: "personal", isDefault: true }),
  ];
  assert.equal(M.pickDefault(all, "/report").id, "per");
  assert.equal(M.pickDefault(all.filter((t) => t.id !== "per"), "/report").id, "team");
  assert.equal(M.pickDefault(all.filter((t) => t.scope === "system"), "/report").id, "sys");
});

test("pickDefault: 无默认 ⇒ null（不编造）", () => {
  const list = [mk({ id: "a", isDefault: false }), mk({ id: "b" })];
  assert.equal(M.pickDefault(list, "/report"), null, "★ 没有默认必须返回 null");
});

test("pickDefault: 按页隔离", () => {
  const list = [mk({ id: "r", page: "/report", isDefault: true })];
  assert.equal(M.pickDefault(list, "/pnl"), null, "★ /pnl 不得取到 /report 的默认");
});

test("pickDefault: 无默认表时不误判 undefined 为 true", () => {
  // isDefault 省略（undefined）不得被当成 true
  const list = [mk({ id: "a" })];
  assert.equal(M.pickDefault(list, "/report"), null);
});

// ───────────────────────────── 6. 深链 ─────────────────────────────

test("templateUrl / templateIdFromUrl: 往返无损", () => {
  const url = M.templateUrl("/report?level=channel&domain=housebrand", "tpl_xyz");
  assert.ok(url.includes("tpl=tpl_xyz"), `应带 tpl 参数：${url}`);
  assert.ok(url.includes("level=channel"), "应保留原有参数");
  assert.equal(M.templateIdFromUrl(url), "tpl_xyz");
});

test("templateUrl: 已有 tpl 参数时覆盖而非重复", () => {
  const url = M.templateUrl("/report?tpl=old", "tpl_new");
  assert.equal(M.templateIdFromUrl(url), "tpl_new");
  assert.equal(url.split("tpl=").length - 1, 1, "★ 不应出现两个 tpl 参数");
});

test("templateIdFromUrl: 无参数 ⇒ null", () => {
  assert.equal(M.templateIdFromUrl("/report"), null);
  assert.equal(M.templateIdFromUrl("/report?tpl="), null);
});

test("templateUrl: 保留 hash", () => {
  const url = M.templateUrl("/report?a=1#section", "tpl_h");
  assert.ok(url.endsWith("#section"), `应保留 hash：${url}`);
});

// ───────────────────────────── 7. 分享 ─────────────────────────────

test("shareInfo: 个人档不生成分享链接", () => {
  const s = M.shareInfo(mk({ scope: "personal" }), "/report");
  assert.equal(s.shareable, false);
  assert.equal(s.url, "");
  assert.ok(s.reason && s.reason.length > 0, "应说明原因");
});

test("shareInfo: 团队/系统档生成分享链接", () => {
  for (const scope of ["team", "system"]) {
    const s = M.shareInfo(mk({ scope, id: "tpl_s" }), "/report");
    assert.equal(s.shareable, true, `${scope} 档应可分享`);
    assert.equal(M.templateIdFromUrl(s.url), "tpl_s");
  }
});

// ───────────────────────────── 8. 与契约一致 ─────────────────────────────

test("TEMPLATE_VERSION 与 contracts/view-template.ts 一致", () => {
  const truth = readFileSync(join(WEB, "..", "contracts", "view-template.ts"), "utf8");
  const v = truth.match(/VIEW_TEMPLATE_VERSION\s*=\s*"([^"]+)"/)?.[1];
  assert.equal(M.TEMPLATE_VERSION, v, "模板版本号漂移");
});

test("SCOPE_ORDER 覆盖全部三档且顺序为 个人→团队→系统", () => {
  assert.deepEqual([...M.SCOPE_ORDER], ["personal", "team", "system"]);
  for (const s of M.SCOPE_ORDER) {
    assert.ok(M.SCOPE_META[s] && M.SCOPE_META[s].label, `档位 ${s} 缺展示元数据`);
  }
});

test("模板**不含**任何权限字段（防止变成越权跳板）", () => {
  const d = M.makeDraft({
    page: "/report",
    name: "x",
    scope: "personal",
    owner: "u.a",
    queryState: qs(),
    columns: [],
    layout: { expanded: {} },
  });
  const keys = Object.keys(d);
  // ★ 模板只描述「怎么看」。若日后有人往模板里塞 grants/maxLevel，
  //   就会出现「套用模板 = 提权」的漏洞，这里提前钉死。
  for (const forbidden of ["grants", "maxLevel", "modules", "dimensions", "canViewBusinessValues", "dataUseGroups"]) {
    assert.ok(!keys.includes(forbidden), `★ 模板不得含权限字段 ${forbidden}`);
  }
});
