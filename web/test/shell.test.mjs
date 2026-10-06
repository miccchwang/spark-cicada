/**
 * test/shell.test.mjs —— 装配层纪律：层级 → 查询维度 / 导航 / 模板套用
 *
 * ★ 本测试要钉死的核心语义（都是「错了会展示错误数字」的类别）：
 *
 *   1. **逐级收敛**：`dimsFromPath` 必须把 domain/channel/store 一并下发。
 *      只传 `dims.level="store"` 会让后端返回全量店铺 ——
 *      用户以为在看某店，其实在看全公司。这是本文件最重要的断言。
 *
 *   2. **导航只前进一层、返回保留上层选择**（返回上级不该丢失方向）。
 *
 *   3. **模板套用是「替换」**：层级也要从模板反推，
 *      否则会出现「模板的筛选 + 当前的层级」这种模板作者没表达过的组合。
 *
 *   4. **URL 降级提示不静默**：参数不完整必须给出提示文案。
 */

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL, fileURLToPath } from "node:url";

const WEB = fileURLToPath(new URL("..", import.meta.url));
const OUT = join(WEB, ".test-build");

let S;

before(async () => {
  const esbuild = await import("esbuild");
  if (!existsSync(OUT)) mkdirSync(OUT, { recursive: true });
  await esbuild.build({
    entryPoints: [join(WEB, "src", "shell", "app.ts")],
    outfile: join(OUT, "shell-app.mjs"),
    format: "esm",
    bundle: true,
    platform: "neutral",
  });
  S = await import(pathToFileURL(join(OUT, "shell-app.mjs")).href);
});

// ───────────────────────────── 层级 ↔ 维度（最关键） ─────────────────────────────

test("dimsFromPath: L3 必须同时下发 domain + channel + store（逐级收敛）", () => {
  const dims = S.dimsFromPath({ level: "store", domain: "housebrand", channel: "TK-TH", store: "S1" });
  assert.equal(dims.level, "store");
  assert.deepEqual(dims.domain, ["housebrand"]);
  assert.deepEqual(dims.channelCode, ["TK-TH"]);
  assert.deepEqual(dims.storeKey, ["S1"]);
});

test("dimsFromPath: L2 只带祖先层参数，不带 store", () => {
  const dims = S.dimsFromPath({ level: "channel", domain: "housebrand", channel: "TK-TH" });
  assert.deepEqual(dims.domain, ["housebrand"]);
  assert.deepEqual(dims.channelCode, ["TK-TH"]);
  assert.equal(dims.storeKey, undefined);
});

test("dimsFromPath: L0 只有 level，无任何维度约束（全量）", () => {
  const dims = S.dimsFromPath({ level: "overview" });
  assert.equal(dims.level, "overview");
  assert.equal(dims.domain, undefined);
  assert.equal(dims.channelCode, undefined);
  assert.equal(dims.storeKey, undefined);
});

test("queryStateFromPath: 出口版本号恒为契约版本，且含 dims.level", () => {
  const qs = S.queryStateFromPath({ level: "channel", domain: "hb", channel: "TK-TH" });
  assert.equal(qs.v, "1.0");
  assert.equal(qs.dims.level, "channel");
  assert.deepEqual(qs.dims.channelCode, ["TK-TH"]);
});

test("queryStateFromPath: custom 区间需要 from+to，给了就用 custom", () => {
  const qs = S.queryStateFromPath({ level: "overview" }, { from: "2026-09-01", to: "2026-09-30" });
  assert.equal(qs.time.mode, "custom");
  assert.equal(qs.time.from, "2026-09-01");
  assert.equal(qs.time.to, "2026-09-30");
});

// ───────────────────────────── 层级互转 ─────────────────────────────

test("readerToL / lToReader: 五层互转往返一致", () => {
  for (const lv of ["overview", "domain", "channel", "store", "sku"]) {
    assert.equal(S.lToReader(S.readerToL(lv)), lv);
  }
  assert.equal(S.readerToL("store"), "L3");
  assert.equal(S.lToReader("L0"), "overview");
  assert.equal(S.lToReader("bogus"), "overview"); // 未知回退，不抛
});

// ───────────────────────────── 面包屑标签 ─────────────────────────────

test("makeLabelResolver: 只认 (level,key) 匹配项，不认则 undefined（不猜）", () => {
  const r = S.makeLabelResolver([
    { level: "L1", key: "housebrand", label: "Housebrand", metrics: {}, childCount: 1, defaultExpanded: false },
  ]);
  assert.equal(r("domain", "housebrand"), "Housebrand");
  // 同 id 但层级不同 → 不得命中（否则把渠道名挂到店铺位上）
  assert.equal(r("channel", "housebrand"), undefined);
  assert.equal(r("domain", "nope"), undefined);
});

test("makeLabelResolver: 无 levels 时恒 undefined（降级为 id）", () => {
  const r = S.makeLabelResolver(undefined);
  assert.equal(r("domain", "x"), undefined);
});

// ───────────────────────────── 外壳描述 ─────────────────────────────

test("buildShellVM: 深链 L2 → 面包屑三级、当前层不可点", () => {
  const vm = S.buildShellVM("?level=channel&domain=hb&channel=TK-TH");
  assert.equal(vm.path.level, "channel");
  assert.equal(vm.crumbs.length, 3); // L0 / L1 / L2
  assert.equal(vm.crumbs[0].current, false);
  assert.equal(vm.crumbs[0].clickable, true);
  assert.equal(vm.crumbs[2].current, true);
  assert.equal(vm.crumbs[2].clickable, false);
  assert.equal(vm.downgraded, false);
});

test("buildShellVM: 不完整 URL 降级 + 提示文案非空", () => {
  const vm = S.buildShellVM("?level=store"); // 缺 domain/channel
  assert.equal(vm.path.level, "overview");
  assert.equal(vm.downgraded, true);
  assert.ok(S.downgradeNotice(vm), "降级必须给出提示，不得静默");
  assert.equal(S.downgradeNotice(S.buildShellVM("?level=overview")), null);
});

test("buildShellVM: tabs 只有当前层及更浅层可达", () => {
  const vm = S.buildShellVM("?level=channel&domain=hb&channel=TK-TH");
  const reach = vm.tabs.map((t) => [t.level, t.reachable]);
  assert.deepEqual(reach, [
    ["overview", true],
    ["domain", true],
    ["channel", true],
    ["store", false],
    ["sku", false],
  ]);
  assert.equal(vm.canDrillDown, true);
  assert.equal(vm.canDrillUp, true);
});

test("buildShellVM: 展开状态仅 L0 与当前层为 true（默认收起契约）", () => {
  const vm = S.buildShellVM("?level=domain&domain=hb");
  assert.equal(vm.expanded.overview, true);
  assert.equal(vm.expanded.domain, true); // 当前层
  assert.equal(vm.expanded.channel, false);
  assert.equal(vm.expanded.store, false);
  assert.equal(vm.expanded.sku, false);
});

test("buildShellVM: L0 处展开只有 overview（不可被 overrides 关掉）", () => {
  const vm = S.buildShellVM("?level=overview", { expandedOverrides: { overview: false, sku: true } });
  assert.equal(vm.expanded.overview, true, "L0 是硬性契约，不可收起");
  assert.equal(vm.expanded.sku, true, "其余层允许覆盖");
});

test("buildShellVM: title 与 crumbText 反映当前层", () => {
  const vm = S.buildShellVM("?level=store&domain=hb&channel=TK-TH&store=S1");
  assert.equal(vm.title, "店铺 (Store)");
  assert.equal(vm.crumbText, "总览 / hb / TK-TH / S1"); // 无 labels → 回退 id
});

test("buildShellVM: 有 labels 时面包屑用 label", () => {
  const vm = S.buildShellVM("?level=domain&domain=hb", {
    levels: [
      { level: "L1", key: "hb", label: "Housebrand", metrics: {}, childCount: 3, defaultExpanded: false },
    ],
  });
  assert.equal(vm.crumbText, "总览 / Housebrand");
});

test("buildShellVM: L4 不能下钻，L0 不能上钻", () => {
  const l4 = S.buildShellVM("?level=sku&domain=hb&channel=TK-TH&store=S1");
  assert.equal(l4.canDrillDown, false);
  assert.equal(l4.canDrillUp, true);

  const l0 = S.buildShellVM("?level=overview");
  assert.equal(l0.canDrillUp, false);
  assert.equal(l0.canDrillDown, true);
});

// ───────────────────────────── 导航 ─────────────────────────────

test("navigate drillDown: L0→L1→L2→L3 且 search 逐级带上祖先参数", () => {
  let path = { level: "overview" };
  let r = S.navigate(path, { kind: "drillDown", id: "hb" });
  assert.equal(r.search, "level=domain&domain=hb");
  path = r.path;

  r = S.navigate(path, { kind: "drillDown", id: "TK-TH" });
  assert.equal(r.search, "level=channel&domain=hb&channel=TK-TH");
  path = r.path;

  r = S.navigate(path, { kind: "drillDown", id: "S1" });
  assert.equal(r.search, "level=store&domain=hb&channel=TK-TH&store=S1");
});

test("navigate drillDown: 在 L4 再下钻应原地不动（无更深层）", () => {
  const l4 = { level: "sku", domain: "hb", channel: "TK-TH", store: "S1" };
  const r = S.navigate(l4, { kind: "drillDown", id: "X" });
  assert.equal(r.path.level, "sku");
  assert.equal(r.path.store, "S1");
});

test("navigate drillUp: L3→L2 保留 domain+channel（不丢方向）", () => {
  const r = S.navigate({ level: "store", domain: "hb", channel: "TK-TH", store: "S1" }, { kind: "drillUp" });
  assert.equal(r.path.level, "channel");
  assert.equal(r.path.domain, "hb");
  assert.equal(r.path.channel, "TK-TH");
  assert.equal(r.path.store, undefined);
});

test("navigate drillUp: L0 原地不动", () => {
  const r = S.navigate({ level: "overview" }, { kind: "drillUp" });
  assert.equal(r.search, "level=overview");
});

test("navigate jump: 跳到 L1 保留 domain，丢弃更深的参数", () => {
  const r = S.navigate({ level: "store", domain: "hb", channel: "TK-TH", store: "S1" }, { kind: "jump", depth: 1 });
  assert.equal(r.path.level, "domain");
  assert.equal(r.path.domain, "hb");
  assert.equal(r.path.channel, undefined);
});

test("navigate setLevel: 直接跳到更深的层会被降级（无参数支撑）", () => {
  const r = S.navigate({ level: "overview" }, { kind: "setLevel", level: "store" });
  assert.equal(r.path.level, "overview", "缺 domain/channel 时不得直接进 L3");
});

test("navigate setLevel: 已有参数时切层保留", () => {
  const r = S.navigate({ level: "domain", domain: "hb" }, { kind: "setLevel", level: "domain" });
  assert.equal(r.path.level, "domain");
  assert.equal(r.path.domain, "hb");
});

test("navigate applyTemplate: 不改层级路径", () => {
  const cur = { level: "channel", domain: "hb", channel: "TK-TH" };
  const r = S.navigate(cur, { kind: "applyTemplate", template: { id: "t1" } });
  assert.deepEqual(r.path, cur);
  assert.equal(r.search, "level=channel&domain=hb&channel=TK-TH");
});

// ───────────────────────────── 模板套用（替换语义） ─────────────────────────────

function mkTemplate(over = {}) {
  return {
    id: "tpl_1",
    name: "泰国月报",
    scope: "team",
    owner: "u.a",
    page: "store",
    queryState: {
      v: "1.0",
      time: { mode: "preset", preset: "mtd", grain: "month", timezone: "Asia/Bangkok" },
      filters: [],
      dims: { level: "store", domain: ["hb"], channelCode: ["TK-TH"], storeKey: ["S1"] },
    },
    columns: [{ key: "gp", label: "GP", visible: true, order: 0 }],
    layout: { expanded: {} },
    createdAt: "2026-10-01T00:00:00Z",
    updatedAt: "2026-10-01T00:00:00Z",
    useCount: 3,
    ...over,
  };
}

test("applyTemplateToState: 层级从模板 dims 反推（不是沿用当前层级）", () => {
  const a = S.applyTemplateToState(mkTemplate());
  assert.equal(a.path.level, "store");
  assert.equal(a.path.domain, "hb");
  assert.equal(a.path.channel, "TK-TH");
  assert.equal(a.path.store, "S1");
  assert.equal(a.queryState.dims.level, "store");
});

test("applyTemplateToState: 模板 dims 不完整 → 层级被降级（不硬闯）", () => {
  const t = mkTemplate({
    queryState: {
      v: "1.0",
      time: { mode: "preset", preset: "mtd", grain: "month", timezone: "Asia/Bangkok" },
      filters: [],
      dims: { level: "store" }, // 缺 domain/channel/store
    },
  });
  const a = S.applyTemplateToState(t);
  assert.equal(a.path.level, "overview", "不完整模板不得把用户带进缺参数的层");
});

test("applyTemplateToState: 深拷贝 —— 改动套用结果不污染模板缓存", () => {
  const t = mkTemplate();
  const a = S.applyTemplateToState(t);
  a.queryState.filters.push({ field: "channelCode", op: "eq", value: "SP-TH" });
  assert.equal(t.queryState.filters.length, 0, "模板的 filters 不得被套用方污染");
});

test("applyTemplateToState: 回显模板名与 id", () => {
  const a = S.applyTemplateToState(mkTemplate());
  assert.equal(a.view.templateId, "tpl_1");
  assert.equal(a.view.templateName, "泰国月报");
});

// ───────────────────────────── describeCurrentView ─────────────────────────────

test("describeCurrentView: page 为当前层级，queryState 带上层级维度", () => {
  const d = S.describeCurrentView({ level: "channel", domain: "hb", channel: "TK-TH" });
  assert.equal(d.page, "channel");
  assert.equal(d.queryState.dims.level, "channel");
  assert.deepEqual(d.queryState.dims.channelCode, ["TK-TH"]);
});

test("describeCurrentView: 额外 filters 被追加（不覆盖原有）", () => {
  const d = S.describeCurrentView(
    { level: "overview" },
    { filters: [{ field: "brand", op: "eq", value: "X" }] },
  );
  assert.equal(d.queryState.filters.length, 1);
  assert.equal(d.queryState.filters[0].field, "brand");
});

// ───────────────────────────── 端到端：URL 往返 ─────────────────────────────

test("端到端: 深链 → 解析 → dims → 导航返回 → URL 往返稳定", () => {
  const search = "?level=store&domain=hb&channel=TK-TH&store=S1";
  const vm = S.buildShellVM(search);
  const dims = S.dimsFromPath(vm.path);
  assert.deepEqual(dims.storeKey, ["S1"]);

  // 点面包屑第 1 级（L1）
  const back = S.navigate(vm.path, { kind: "jump", depth: 1 });
  assert.equal(back.search, "level=domain&domain=hb");

  // 再解析回来，层级一致
  const vm2 = S.buildShellVM(`?${back.search}`);
  assert.equal(vm2.path.level, "domain");
  assert.equal(vm2.path.domain, "hb");
});

test("端到端: 未知 level 值回退 overview（不抛错）", () => {
  const vm = S.buildShellVM("?level=bogus");
  assert.equal(vm.path.level, "overview");
});
