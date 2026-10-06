/**
 * test/report.test.mjs —— M-REPORT 五层阅览：深链 / 降级 / 面包屑 / 默认收起
 *
 * ★ 本测试要钉死的核心语义：
 *   1. **URL 是唯一真源**：parse → toQuery 往返无损。
 *   2. **不完整路径必须降级到最近可达层** —— 绝不「硬闯」到缺参数的层。
 *      这是安全相关：`?level=store` 缺 channel 若硬闯，会命中全量数据，
 *      用户看到「像是某店铺、其实是全公司」的数字，比报错危险得多。
 *   3. **面包屑回退到 id 而非省略** —— 宁可显示丑 id，不让用户少一级可返回。
 *   4. **默认收起契约**：仅 L0 展开，且 L0 不可被覆盖为收起。
 */

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL, fileURLToPath } from "node:url";

const WEB = fileURLToPath(new URL("..", import.meta.url));
const OUT = join(WEB, ".test-build");

let R;

before(async () => {
  const esbuild = await import("esbuild");
  if (!existsSync(OUT)) mkdirSync(OUT, { recursive: true });
  await esbuild.build({
    entryPoints: [join(WEB, "src", "report", "levels.ts")],
    outfile: join(OUT, "report-levels.mjs"),
    format: "esm",
    bundle: true,
    platform: "neutral",
  });
  R = await import(pathToFileURL(join(OUT, "report-levels.mjs")).href);
});

// ───────────────────────────── 层级元数据 ─────────────────────────────

test("LEVEL_META: 五层、顺序与深度正确", () => {
  assert.deepEqual(
    R.LEVEL_META.map((m) => m.id),
    ["overview", "domain", "channel", "store", "sku"],
  );
  assert.deepEqual(
    R.LEVEL_META.map((m) => m.depth),
    [0, 1, 2, 3, 4],
  );
});

test("depthOf / metaOf: 深度与元数据", () => {
  assert.equal(R.depthOf("overview"), 0);
  assert.equal(R.depthOf("sku"), 4);
  assert.equal(R.metaOf("channel").label, "渠道 (Channel)");
  // 未知层级回退 L0（不抛错）
  assert.equal(R.depthOf("bogus").valueOf(), 0);
});

// ───────────────────────────── 深链往返 ─────────────────────────────

test("parseLevelPath → levelPathToQuery: 往返无损（逐层）", () => {
  const cases = [
    "?level=overview",
    "?level=domain&domain=housebrand",
    "?level=channel&domain=housebrand&channel=TK-TH",
    "?level=store&domain=housebrand&channel=TK-TH&store=shop_9",
  ];
  for (const q of cases) {
    const p = R.parseLevelPath(q);
    const again = R.parseLevelPath(`?${R.levelPathToQuery(p)}`);
    assert.deepEqual(again, p, `往返不一致：${q}`);
  }
});

test("parseLevelPath: 无 level 参数 ⇒ L0", () => {
  assert.deepEqual(R.parseLevelPath(""), { level: "overview" });
  assert.deepEqual(R.parseLevelPath("?foo=bar"), { level: "overview" });
});

test("parseLevelPath: 非法 level ⇒ L0（不抛错）", () => {
  assert.deepEqual(R.parseLevelPath("?level=nope"), { level: "overview" });
});

test("levelPathToQuery: 只带当前层及祖先的参数", () => {
  // L1 却带着 channel ⇒ 序列化时不应输出 channel（避免深链语义混淆）
  const q = R.levelPathToQuery({ level: "domain", domain: "hb", channel: "TK-TH" });
  assert.ok(!q.includes("channel="), `★ L1 不应带 channel 参数：${q}`);
  assert.ok(q.includes("domain=hb"));
});

// ───────────────────────────── ★ 降级（最重要的正确性保证） ─────────────────────────────

test("normalizePath: L3 缺 channel ⇒ 退到 L1（不硬闯）", () => {
  const p = R.normalizePath({ level: "store", domain: "hb", store: "s1" });
  assert.equal(p.level, "domain", "★ 缺 channel 必须降级，不得硬闯 L3（会命中全量数据）");
  assert.equal(p.domain, "hb");
  assert.equal(p.store, undefined, "降级后应清掉用不上的 store 参数");
});

test("normalizePath: L2 缺 domain ⇒ 退到 L0", () => {
  const p = R.normalizePath({ level: "channel", channel: "TK-TH" });
  assert.equal(p.level, "overview", "★ 缺 domain 必须退到 L0");
});

test("normalizePath: L4/SKU 需要 store", () => {
  const p = R.normalizePath({ level: "sku", domain: "hb", channel: "TK-TH" });
  assert.equal(p.level, "channel", "★ 缺 store 应退到 L2");
});

test("normalizePath: 完整路径原样保留", () => {
  const full = { level: "store", domain: "hb", channel: "TK-TH", store: "s1" };
  assert.deepEqual(R.normalizePath(full), full);
});

test("wasDowngraded: 报告是否被降级", () => {
  const orig = { level: "store", domain: "hb" };
  const norm = R.normalizePath(orig);
  assert.equal(R.wasDowngraded(orig, norm), true);

  const ok = { level: "domain", domain: "hb" };
  assert.equal(R.wasDowngraded(ok, R.normalizePath(ok)), false);
});

test("parseLevelPath: 不完整 URL 也走降级", () => {
  const p = R.parseLevelPath("?level=store&store=s1");
  assert.equal(p.level, "overview", "★ 缺 domain/channel 应一路降到 L0");
});

test("★ 回归: parseRawPath 保留原始 level —— 否则降级永远报不出来", () => {
  // 这是实际踩过的坑：若用已降级的 parseLevelPath 去比 normalizePath，
  // 两者恒等，wasDowngraded 恒 false ⇒ 「链接不完整」的提示永远不出现（静默）。
  const raw = R.parseRawPath("?level=store&store=s1");
  assert.equal(raw.level, "store", "parseRawPath 必须保留原始 level 以便识别降级");
  assert.equal(raw.store, "s1");

  const norm = R.normalizePath(raw);
  assert.equal(norm.level, "overview");
  assert.equal(R.wasDowngraded(raw, norm), true, "★ 用 raw 比较必须能识别出降级");
});

test("parseRawPath: 非法 level 回退 overview（不抛错）", () => {
  assert.equal(R.parseRawPath("?level=nope").level, "overview");
  assert.equal(R.parseRawPath("").level, "overview");
});

test("parseRawPath: 完整路径与 parseLevelPath 一致", () => {
  const q = "?level=store&domain=hb&channel=TK-TH&store=s1";
  assert.deepEqual(R.parseRawPath(q), R.parseLevelPath(q));
});

// ───────────────────────────── 下钻 / 上钻 / 跳级 ─────────────────────────────

test("drillDown: 逐层下钻", () => {
  let p = { level: "overview" };
  p = R.drillDown(p, { id: "hb" });
  assert.deepEqual(p, { level: "domain", domain: "hb" });

  p = R.drillDown(p, { id: "TK-TH" });
  assert.deepEqual(p, { level: "channel", domain: "hb", channel: "TK-TH" });

  p = R.drillDown(p, { id: "shop_9" });
  assert.deepEqual(p, { level: "store", domain: "hb", channel: "TK-TH", store: "shop_9" });
});

test("drillDown: L3 之下无可下钻（SKU 层返回原路径）", () => {
  const atStore = { level: "store", domain: "hb", channel: "TK-TH", store: "s1" };
  // L3 下钻到 L4 需要 store（已有），但本实现约定「L3 点击 → 抽屉展示 L4 不跳页」，
  // 因此 drillDown 在 L3 返回原路径（不改变层级）。
  assert.deepEqual(R.drillDown(atStore, { id: "sku_1" }), atStore);
});

test("drillUp: 保留上层已选参数", () => {
  const atStore = { level: "store", domain: "hb", channel: "TK-TH", store: "s1" };
  // L3 → L2 应停在「当前渠道」，而不是跳回全部渠道
  assert.deepEqual(R.drillUp(atStore), { level: "channel", domain: "hb", channel: "TK-TH" });

  // L2 → L1 停在当前板块
  assert.deepEqual(R.drillUp({ level: "channel", domain: "hb", channel: "TK-TH" }), {
    level: "domain",
    domain: "hb",
  });

  // L1 → L0
  assert.deepEqual(R.drillUp({ level: "domain", domain: "hb" }), { level: "overview" });

  // L0 上钻不动
  assert.deepEqual(R.drillUp({ level: "overview" }), { level: "overview" });
});

test("jumpToDepth: 跳到任意祖先层并保留对应参数", () => {
  const p = { level: "sku", domain: "hb", channel: "TK-TH", store: "s1" };
  assert.deepEqual(R.jumpToDepth(p, 1), { level: "domain", domain: "hb" });
  assert.deepEqual(R.jumpToDepth(p, 3), { level: "store", domain: "hb", channel: "TK-TH", store: "s1" });
  assert.deepEqual(R.jumpToDepth(p, 0), { level: "overview" });
});

test("jumpToDepth: 越界深度被钳制", () => {
  const p = { level: "channel", domain: "hb", channel: "TK-TH" };
  assert.equal(R.jumpToDepth(p, 99).level, "sku");
  assert.equal(R.jumpToDepth(p, -5).level, "overview");
});

// ───────────────────────────── 面包屑 ─────────────────────────────

test("breadcrumb: 始终含 L0 根，逐级到当前层", () => {
  const p = { level: "channel", domain: "hb", channel: "TK-TH" };
  const bc = R.breadcrumb(p);
  assert.deepEqual(bc.map((c) => c.level), ["overview", "domain", "channel"]);
  assert.equal(bc[0].text, "总览");
  // 祖先层回退到 id
  assert.equal(bc[1].text, "hb");
  assert.equal(bc[2].text, "TK-TH");
});

test("breadcrumb: 当前层不可点，祖先层可点", () => {
  const bc = R.breadcrumb({ level: "store", domain: "hb", channel: "TK-TH", store: "s1" });
  assert.equal(bc.at(-1).current, true);
  assert.equal(bc.at(-1).clickable, false);
  for (const c of bc.slice(0, -1)) {
    assert.equal(c.clickable, true, `${c.level} 应可点`);
    assert.equal(c.current, false);
  }
});

test("breadcrumb: 用 LabelResolver 解析展示名", () => {
  const resolve = (lvl, id) => ({ domain: { hb: "Housebrand" }, channel: { "TK-TH": "TikTok 泰国" } })[lvl]?.[id];
  const bc = R.breadcrumb({ level: "channel", domain: "hb", channel: "TK-TH" }, resolve);
  assert.deepEqual(bc.map((c) => c.text), ["总览", "Housebrand", "TikTok 泰国"]);
});

test("breadcrumb: resolver 解析不到时**回退到 id**（不省略）", () => {
  const bc = R.breadcrumb({ level: "domain", domain: "unknown_x" }, () => undefined);
  assert.equal(bc.length, 2, "★ 绝不省略祖先层（否则用户少一级可返回）");
  assert.equal(bc[1].text, "unknown_x");
});

test("breadcrumb: target 路径可用于点击返回", () => {
  const p = { level: "store", domain: "hb", channel: "TK-TH", store: "s1" };
  const bc = R.breadcrumb(p);
  // 点 L1 ⇒ 回到 L1 并保留 domain
  assert.deepEqual(bc[1].target, { level: "domain", domain: "hb" });
  // 点 L0 ⇒ 回到总览
  assert.deepEqual(bc[0].target, { level: "overview" });
});

test("breadcrumbText: 纯文本形式", () => {
  const t = R.breadcrumbText({ level: "channel", domain: "hb", channel: "TK-TH" });
  assert.equal(t, "总览 / hb / TK-TH");
});

// ───────────────────────────── 默认收起契约（§9.4） ─────────────────────────────

test("DEFAULT_EXPANDED: 仅 L0 展开", () => {
  assert.equal(R.DEFAULT_EXPANDED.overview, true);
  for (const lv of ["domain", "channel", "store", "sku"]) {
    assert.equal(R.DEFAULT_EXPANDED[lv], false, `★ ${lv} 必须默认收起`);
  }
});

test("checkDefaultCollapsedContract: 无违规", () => {
  assert.deepEqual(R.checkDefaultCollapsedContract(), []);
});

test("initialExpanded: L0 常驻 + 所在一层展开 + 其余收起", () => {
  const e = R.initialExpanded({ level: "channel", domain: "hb", channel: "TK-TH" });
  assert.equal(e.overview, true, "L0 常驻");
  assert.equal(e.channel, true, "所在一层应展开");
  assert.equal(e.domain, false, "其余默认收起");
  assert.equal(e.store, false);
  assert.equal(e.sku, false);
});

test("initialExpanded: L0 不可被 overrides 覆盖为收起", () => {
  const e = R.initialExpanded({ level: "overview" }, { overview: false });
  assert.equal(e.overview, true, "★ L0 恒展开是硬性契约，覆盖应被忽略");
});

test("initialExpanded: overrides 可展开其它层", () => {
  const e = R.initialExpanded({ level: "overview" }, { store: true });
  assert.equal(e.store, true);
  assert.equal(e.sku, false);
});

// ───────────────────────────── 与 ReadingLevel 一致 ─────────────────────────────

test("READING_LEVELS 与 contracts 的 ReadingLevel 联合值一致", () => {
  // 顺序即深度语义，必须是 overview → sku
  assert.deepEqual([...R.READING_LEVELS], ["overview", "domain", "channel", "store", "sku"]);
});
