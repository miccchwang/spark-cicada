/**
 * test/filter.test.mjs —— M-FILTER 行为断言
 *
 * 重点：
 *   * preset → 时间区间的确定性（注入固定 now，避免时区/时钟导致 flaky）
 *   * QueryState 组装正确、JSON 安全（无 undefined）
 *   * 分页边界钳制（拒绝超大页）
 */

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { existsSync, mkdirSync } from "node:fs";
import { pathToFileURL, fileURLToPath } from "node:url";

// 本文件位于 web/test/，故 ".." 即 web/。
const WEB = fileURLToPath(new URL("..", import.meta.url));
const OUT = join(WEB, ".test-build");

let F;

before(async () => {
  const esbuild = await import("esbuild");
  if (!existsSync(OUT)) mkdirSync(OUT, { recursive: true });
  await esbuild.build({
    entryPoints: [join(WEB, "src", "filter", "query-state.ts")],
    outfile: join(OUT, "filter.mjs"),
    format: "esm",
    bundle: true,
    platform: "neutral",
  });
  F = await import(pathToFileURL(join(OUT, "filter.mjs")).href);
});

/** 固定「现在」：2026-10-05（Asia/Bangkok = UTC+7）。 */
const NOW = new Date("2026-10-05T10:00:00+07:00");

test("preset mtd → 当月 1 日 ~ 今天", () => {
  const r = F.resolvePreset("mtd", "Asia/Bangkok", NOW);
  assert.deepEqual(r, { from: "2026-10-01", to: "2026-10-05" });
});

test("preset last7d → 前 7 天含今天", () => {
  const r = F.resolvePreset("last7d", "Asia/Bangkok", NOW);
  assert.deepEqual(r, { from: "2026-09-29", to: "2026-10-05" });
});

test("preset last_full_month → 上一个完整自然月", () => {
  const r = F.resolvePreset("last_full_month", "Asia/Bangkok", NOW);
  assert.deepEqual(r, { from: "2026-09-01", to: "2026-09-30" });
});

test("preset ytd → 年初 ~ 今天", () => {
  const r = F.resolvePreset("ytd", "Asia/Bangkok", NOW);
  assert.deepEqual(r, { from: "2026-01-01", to: "2026-10-05" });
});

test("preset all → 不编造区间（from/to 均缺省）", () => {
  const r = F.resolvePreset("all", "Asia/Bangkok", NOW);
  assert.deepEqual(r, {}, "all 不应编造区间，应交由 M-QUERY 决定");
});

test("makeTimeRange preset 模式带上 grain 与时区", () => {
  const tr = F.makeTimeRange("preset", { preset: "mtd", grain: "month" }, NOW);
  assert.equal(tr.mode, "preset");
  assert.equal(tr.preset, "mtd");
  assert.equal(tr.grain, "month");
  assert.equal(tr.timezone, "Asia/Bangkok");
  assert.equal(tr.from, "2026-10-01");
});

test("makeTimeRange custom 缺 from/to 必须报错（不静默）", () => {
  assert.throws(() => F.makeTimeRange("custom", { from: "2026-10-01" }), /必须提供 from 与 to/);
});

test("buildQueryState 版本号恒为 1.0 且 JSON 安全", () => {
  const qs = F.buildQueryState({
    time: F.makeTimeRange("preset", { preset: "mtd" }, NOW),
    dims: F.makeDims("store", { brand: ["KONVY"] }),
  });
  assert.equal(qs.v, "1.0");
  // JSON 序列化后不得出现 undefined（会被丢字段 → 协议不一致）
  const json = JSON.stringify(qs);
  assert.ok(!json.includes("undefined"), "QueryState 不应含 undefined");
  assert.deepEqual(Object.keys(qs).sort(), ["dims", "filters", "time", "v"]);
});

test("filters 去重：同 field+op 保留最后一条", () => {
  const qs = F.buildQueryState({
    time: F.makeTimeRange("preset", { preset: "mtd" }, NOW),
    dims: F.makeDims("store"),
    filters: [
      F.makeFilter("brand", "eq", "A"),
      F.makeFilter("brand", "eq", "B"), // 覆盖 A
      F.makeFilter("channelCode", "in", ["TK-TH"]),
    ],
  });
  assert.equal(qs.filters.length, 2);
  const brand = qs.filters.find((f) => f.field === "brand");
  assert.equal(brand.value, "B", "应保留最后一条");
});

test("makeFilter 拒绝非法算子", () => {
  assert.throws(() => F.makeFilter("brand", "like", "x"), /非法算子/);
});

test("makePage 钳制超大 limit", () => {
  const p = F.makePage(0, 999999);
  assert.equal(p.limit, F.MAX_PAGE_SIZE, "limit 应被钳制到上限");
});

test("makePage 负数 offset 归零", () => {
  const p = F.makePage(-5, 50);
  assert.equal(p.offset, 0);
});

test("describeQueryState 产出可读摘要", () => {
  const qs = F.buildQueryState({
    time: F.makeTimeRange("preset", { preset: "mtd" }, NOW),
    dims: F.makeDims("store"),
    filters: [F.makeFilter("brand", "eq", "A")],
  });
  const s = F.describeQueryState(qs);
  assert.match(s, /预设:mtd/);
  assert.match(s, /层级:store/);
  assert.match(s, /筛选:1条/);
});
