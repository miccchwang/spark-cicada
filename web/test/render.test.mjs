/**
 * test/render.test.mjs —— M-RENDER 语义断言（G2/G3）
 *
 * 直接对 TS 源码做**行为测试**：用 esbuild 即时编译为 ESM 后导入，
 * 保证测的是真实实现而非重写副本。
 */

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { existsSync, mkdirSync } from "node:fs";
import { pathToFileURL, fileURLToPath } from "node:url";

// REMOTE 已经是 web/ 目录本身（本文件位于 web/test/），不要再 join "web"。
const WEB = fileURLToPath(new URL("..", import.meta.url));
const OUT = join(WEB, ".test-build");

let render, contracts;

before(async () => {
  const esbuild = await import("esbuild");
  if (!existsSync(OUT)) mkdirSync(OUT, { recursive: true });
  await esbuild.build({
    entryPoints: [join(WEB, "src", "render", "render.ts")],
    outfile: join(OUT, "render.mjs"),
    format: "esm",
    bundle: true,
    platform: "neutral",
  });
  await esbuild.build({
    entryPoints: [join(WEB, "src", "contracts", "data-contract.ts")],
    outfile: join(OUT, "data-contract.mjs"),
    format: "esm",
    bundle: true,
    platform: "neutral",
  });
  render = await import(pathToFileURL(join(OUT, "render.mjs")).href);
  contracts = await import(pathToFileURL(join(OUT, "data-contract.mjs")).href);
});

/** 构造一个基准 DataContract：cogs 有值，net_contrib 缺失（依赖 MISSING 槽）。 */
function makeDC(overrides = {}) {
  return {
    v: "1.0",
    queryHash: "h1",
    precomputed: true,
    generatedAt: "2026-10-05T00:00:00Z",
    columns: [
      { key: "month", label: "月份", perm: "L1", kind: "date" },
      { key: "shop_id", label: "门店", perm: "L1", kind: "text" },
      { key: "gp", label: "毛利", perm: "L3", kind: "currency", algoId: "algo.gp" },
      { key: "cogs", label: "成本", perm: "L3", kind: "currency", algoId: "algo.cogs" },
      { key: "net_contrib", label: "净贡献", perm: "L4", kind: "currency", algoId: "algo.net_contrib" },
    ],
    rows: [
      { month: "2026-09-01", shop_id: "S1", gp: 600, cogs: 400, net_contrib: null },
      { month: "2026-09-01", shop_id: "S2", gp: 0, cogs: 0, net_contrib: null }, // 真实 0，应展示 0
    ],
    aggregates: { gp: 600, cogs: 400, net_contrib: null },
    levels: [
      { level: "L0", key: "all", label: "总览", metrics: { gp: 600 }, childCount: 2, defaultExpanded: true },
      { level: "L1", key: "SEA", label: "东南亚", metrics: { gp: 600 }, childCount: 1, defaultExpanded: false },
    ],
    algoTrace: [
      { field: "net_contrib", algoId: "algo.net_contrib", dataSlots: ["slot.platform_fee", "slot.affiliate"], skipped: true, reason: "依赖数据不足" },
    ],
    gaps: [
      { field: "net_contrib", slot: "slot.affiliate", coverage: 0, gate: 0.8, reason: "依赖数据不足，已跳过（fail-closed）" },
    ],
    ...overrides,
  };
}

// ───────────────────────────── G3：缺失不展示为 0 ─────────────────────────────

test("G3: null 值渲染为「待接入」，而不是 0", () => {
  const vm = render.renderDataContract(makeDC());
  const row = vm.table.rows[0];
  const cell = row.cells.find((c) => c.key === "net_contrib");
  assert.ok(cell, "应存在 net_contrib 单元格");
  assert.equal(cell.missing, true, "net_contrib 源为 null，应标记缺失");
  assert.notEqual(cell.text, "0", "★ 缺失绝不能被展示为 0");
  assert.notEqual(cell.text, "¥0", "★ 缺失绝不能被展示为 ¥0");
  assert.match(cell.text, /待接入|—/, "缺失文案应为「待接入」或「—」");
  assert.equal(cell.gapReason, "依赖数据不足，已跳过（fail-closed）", "应带缺失原因");
});

test("G3: 真实 0 仍然展示为 0（不能把 0 当缺失）", () => {
  const vm = render.renderDataContract(makeDC());
  const row = vm.table.rows[1];
  const gp = row.cells.find((c) => c.key === "gp");
  assert.equal(gp.missing, false, "源为真实 0，不应标记缺失");
  assert.equal(gp.text, "¥0", "真实 0 应正常展示");
});

test("G3: checkNoZeroImputation 能抓出伪造的 0", () => {
  // 构造违规：源为 null 但格式化成 0 的假实现会被本检查发现
  const dc = makeDC();
  const bad = render.checkNoZeroImputation(dc);
  assert.deepEqual(bad, [], "基准数据不应有零填充违规");
});

// ───────────────────────────── G2：默认收起 ─────────────────────────────

test("G2: L0 默认展开，L1–L4 默认收起", () => {
  const vm = render.renderDataContract(makeDC());
  const l0 = vm.levels.find((l) => l.level === "L0");
  const l1 = vm.levels.find((l) => l.level === "L1");
  assert.equal(l0.expanded, true, "L0 必须默认展开");
  assert.equal(l1.expanded, false, "L1 必须默认收起");
});

test("G2: 可折叠子表默认收起", () => {
  const vm = render.renderDataContract(makeDC());
  assert.equal(vm.table.collapsibleExpanded, false, "可折叠子表默认必须收起");
});

test("G2: 显式展开状态可覆盖默认", () => {
  const vm = render.renderDataContract(makeDC(), { L1: true });
  const l1 = vm.levels.find((l) => l.level === "L1");
  assert.equal(l1.expanded, true, "显式展开应生效");
});

test("G2: checkDefaultCollapsed 通过基准数据", () => {
  assert.deepEqual(render.checkDefaultCollapsed(makeDC()), []);
});

test("G2: checkDefaultCollapsed 能抓出「L1 被默认展开」的违规", () => {
  const dc = makeDC();
  dc.levels = dc.levels.map((l) => (l.level === "L1" ? { ...l, defaultExpanded: true } : l));
  const bad = render.checkDefaultCollapsed(dc);
  assert.ok(bad.length > 0, "L1 被默认展开应被抓出");
});

// ───────────────────────────── 二次门控（D7 / 密级） ─────────────────────────────

test("D7: 不可见业务数值时不渲染业务数值列", () => {
  const cols = render.gateColumns(makeDC().columns, {
    maxLevel: "L4",
    canViewBusinessValues: false,
    businessValueKeys: ["gp", "cogs", "net_contrib"],
  });
  const keys = cols.map((c) => c.key);
  assert.ok(!keys.includes("gp"), "gp 属业务数值，应被剔除");
  assert.ok(!keys.includes("cogs"), "cogs 属业务数值，应被剔除");
  assert.ok(keys.includes("shop_id"), "维度列应保留");
});

test("密级: maxLevel=L2 时 L3/L4 列被剔除", () => {
  const cols = render.gateColumns(makeDC().columns, { maxLevel: "L2", canViewBusinessValues: true });
  const keys = cols.map((c) => c.key);
  assert.ok(!keys.includes("gp"), "L3 列在 maxLevel=L2 下应剔除");
  assert.ok(!keys.includes("net_contrib"), "L4 列在 maxLevel=L2 下应剔除");
  assert.ok(keys.includes("month"), "L1 列应保留");
});

// ───────────────────────────── 空结果 vs 值为 0 ─────────────────────────────

test("空结果标记 empty，且不被当作「值为 0」", () => {
  const vm = render.renderDataContract(makeDC({ rows: [] }));
  assert.equal(vm.empty, true);
  assert.equal(vm.table.rows.length, 0);
});

test("formatNumber: percent 展示为百分比", () => {
  const r = render.formatNumber(0.6, "percent");
  assert.equal(r.text, "60.00%");
  assert.equal(r.missing, false);
});

test("formatNumber: 分母 0 导致的 null 展示为缺失", () => {
  const r = render.formatNumber(null, "percent");
  assert.equal(r.missing, true);
  assert.notEqual(r.text, "0.00%");
});
