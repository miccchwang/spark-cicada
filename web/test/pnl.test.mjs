/**
 * test/pnl.test.mjs —— M-PNL 口径切换：归属 / 不变量 / 缺失传播 / 默认口径
 *
 * ★ 本测试要钉死的核心语义：
 *
 *   1. **★★★ net_revenue 在两口径下必须完全相同。**
 *      这是 P&L 契约最重要的不变量。KODP 实测里，同一份数据 A 口径显示
 *      巨大亏损、B 口径显示微利 —— 两口径都没错，但净收入**不能**不同。
 *      若某天它不同了，那是公式被改坏了，不是「新口径」。
 *
 *   2. **口径切换只搬「折扣」，不碰别的行**（除营销费用按定义并入折扣）。
 *
 *   3. **非法口径：主动切换必须报错，页面加载用模块默认并告知** ——
 *      静默按 A 展示会让财务拿着 A 的数字去开会。
 *
 *   4. **缺失保持 null（含折扣缺失时 A 口径的营销费用），绝不补 0。**
 */

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL, fileURLToPath } from "node:url";

const WEB = fileURLToPath(new URL("..", import.meta.url));
const OUT = join(WEB, ".test-build");

let P;

before(async () => {
  const esbuild = await import("esbuild");
  if (!existsSync(OUT)) mkdirSync(OUT, { recursive: true });
  await esbuild.build({
    entryPoints: [join(WEB, "src", "pnl", "statement.ts")],
    outfile: join(OUT, "pnl-statement.mjs"),
    format: "esm",
    bundle: true,
    platform: "neutral",
  });
  P = await import(pathToFileURL(join(OUT, "pnl-statement.mjs")).href);
});

/** 构造一份完整输入（默认两口径下营销费用会不同，因为含折扣）。 */
function inputs(over = {}) {
  return {
    grossListing: 1_000_000,
    sellerDiscount: 60_000,
    platformDiscount: 20_000,
    // net_revenue = 1_000_000 − 60_000 − 20_000 = 920_000（两口径相同）
    netRevenue: 920_000,
    cogs: 600_000,
    gp: 320_000,
    gmp: 320_000 / 920_000,
    marketing: 100_000, // 不含折扣
    cm1: 220_000,
    platformFee: 92_000,
    cm2: 128_000,
    overheadAlloc: 40_000,
    netContrib: 88_000,
    ...over,
  };
}

// ───────────────────────────── 口径解析 ─────────────────────────────

test("isCaliber / parseCaliber: 合法值通过，非法抛 CaliberError", () => {
  assert.equal(P.isCaliber("A"), true);
  assert.equal(P.isCaliber("B"), true);
  assert.equal(P.isCaliber("C"), false);
  assert.equal(P.isCaliber(undefined), false);

  assert.equal(P.parseCaliber("A"), "A");
  assert.throws(() => P.parseCaliber("C"), (e) => e.name === "CaliberError");
  assert.throws(() => P.parseCaliber(undefined), (e) => e.name === "CaliberError");
});

test("resolveCaliber: 请求值合法 → 原样采用，不降级", () => {
  const r = P.resolveCaliber("A", "module.pnl");
  assert.equal(r.caliber, "A");
  assert.equal(r.downgraded, false);
  assert.equal(r.source, "requested");
});

test("resolveCaliber: 缺省 → 按模块默认（D10 分模块各自默认）", () => {
  const pnl = P.resolveCaliber(undefined, "module.pnl");
  assert.equal(pnl.caliber, "B", "★ P&L 默认财务口径 B");
  assert.equal(pnl.source, "module_default");
  assert.equal(pnl.downgraded, false, "「未指定」不是降级，是正常取默认");
  assert.match(pnl.reason, /分模块各自默认/);

  const rep = P.resolveCaliber(undefined, "module.report");
  assert.equal(rep.caliber, "A", "★ 经营报表默认运营口径 A");
});

test("resolveCaliber: 脏值 → 回退模块默认且标记降级并给出原因", () => {
  const r = P.resolveCaliber("garbage", "module.pnl");
  assert.equal(r.caliber, "B");
  assert.equal(r.downgraded, true);
  assert.equal(r.source, "module_default");
  assert.match(r.reason, /非法/);
});

test("resolveCaliber: 未登记模块 → 兜底口径 B（财务口径更保守）", () => {
  const r = P.resolveCaliber(undefined, "module.unknown");
  assert.equal(r.caliber, "B");
  assert.equal(r.source, "fallback");
  assert.equal(r.downgraded, true);
});

test("caliberMeta / discountTargetLine: 与契约一致", () => {
  assert.equal(P.caliberMeta("A").sellerDiscountRole, "marketing_expense");
  assert.equal(P.caliberMeta("B").sellerDiscountRole, "contra_revenue");
  assert.equal(P.discountTargetLine("A"), "marketing");
  assert.equal(P.discountTargetLine("B"), "seller_discount");
});

// ───────────────────────────── ★ 不变量 ─────────────────────────────

test("★★★ 不变量: net_revenue 与 gross_listing 两口径完全相同", () => {
  const cmp = P.compareCalibers(inputs(), { module: "module.pnl" });
  assert.equal(cmp.invariant.ok, true, `不变量被破坏：${cmp.invariant.violations.join("; ")}`);
  assert.equal(P.valueOf(cmp.A, "net_revenue"), 920_000);
  assert.equal(P.valueOf(cmp.B, "net_revenue"), 920_000);
  assert.equal(P.valueOf(cmp.A, "gross_listing"), 1_000_000);
  assert.equal(P.valueOf(cmp.B, "gross_listing"), 1_000_000);
});

test("★ 不变量被破坏时必须报告（构造一个「错把折扣算进净收入」的输入）", () => {
  // 故意造一个违反不变量的场景：口径 B 的净收入被多扣了一次折扣。
  // 真实世界里这来自「有人在 B 分支里又减了一次 seller_discount」。
  const bad = inputs();
  const a = P.statementFor("A", bad);
  const b = P.statementFor("B", { ...bad, netRevenue: 920_000 - 60_000 });
  const res = P.assertNetRevenueInvariant(a, b);
  assert.equal(res.ok, false, "★ 净收入不同必须被判定为违规，不能轻轻放过");
  assert.match(res.violations.join(" "), /net_revenue/);
  assert.match(res.violations.join(" "), /不变量被破坏/);
});

test("不变量：两张都缺失同一个值时视为一致（null 语义）", () => {
  const a = P.statementFor("A", inputs({ netRevenue: null }));
  const b = P.statementFor("B", inputs({ netRevenue: null }));
  const res = P.assertNetRevenueInvariant(a, b);
  assert.equal(res.ok, true, "两边都缺 = 一致（缺的是同一件事）");
});

test("不变量：一边缺失一边有值 → 违规", () => {
  const a = P.statementFor("A", inputs({ netRevenue: null }));
  const b = P.statementFor("B", inputs());
  assert.equal(P.assertNetRevenueInvariant(a, b).ok, false);
});

test("不变量：传错口径组合（B, A）→ 违规提示", () => {
  const a = P.statementFor("A", inputs());
  const b = P.statementFor("B", inputs());
  const res = P.assertNetRevenueInvariant(b, a);
  assert.equal(res.ok, false);
  assert.match(res.violations.join(" "), /实得 \(B, A\)/);
});

// ───────────────────────────── 归属搬运 ─────────────────────────────

test("口径 A: 营销费用 = 核销费用 + 卖家折扣（折扣计入费用）", () => {
  const a = P.statementFor("A", inputs());
  assert.equal(P.valueOf(a, "marketing"), 160_000, "100,000 + 60,000");
});

test("口径 B: 营销费用保持内核原值（折扣去收入侧）", () => {
  const b = P.statementFor("B", inputs());
  assert.equal(P.valueOf(b, "marketing"), 100_000);
});

test("★ 口径切换的差异**只有两行**：marketing 与（按定义）折扣的归属", () => {
  const cmp = P.compareCalibers(inputs());
  assert.deepEqual(cmp.differingLines, ["marketing"], "除营销费用外，其余行数值必须完全一致");
});

test("两口径下 seller_discount 行的**数值相同**（差异在归属与解读，不在数字）", () => {
  const a = P.statementFor("A", inputs());
  const b = P.statementFor("B", inputs());
  assert.equal(P.valueOf(a, "seller_discount"), P.valueOf(b, "seller_discount"));
});

test("无折扣时两口径完全一致（切换无可见差异）", () => {
  const cmp = P.compareCalibers(inputs({ sellerDiscount: 0 }));
  assert.deepEqual(cmp.differingLines, []);
  assert.match(P.caliberDiffSummary(cmp), /无可见差异/);
});

// ───────────────────────────── 缺失传播（不补 0） ─────────────────────────────

test("★ 折扣缺失 ⇒ 口径 A 的营销费用也缺失（不把折扣当 0 加）", () => {
  const a = P.statementFor("A", inputs({ sellerDiscount: null }));
  assert.equal(P.valueOf(a, "marketing"), null, "折扣缺失时 A 口径营销费用不得虚低");
  const m = P.lineOf(a, "marketing");
  assert.ok(m.gapReason, "缺失必须带原因");
});

test("营销费用缺失 ⇒ 口径 A 结果缺失（不把费用当 0）", () => {
  const a = P.statementFor("A", inputs({ marketing: null }));
  assert.equal(P.valueOf(a, "marketing"), null);
});

test("口径 B 下营销费用缺失不影响折扣行（两件事分开）", () => {
  const b = P.statementFor("B", inputs({ marketing: null }));
  assert.equal(P.valueOf(b, "marketing"), null);
  assert.equal(P.valueOf(b, "seller_discount"), 60_000);
});

test("净贡献缺失（达人佣金 MISSING）⇒ null，且 hasGaps=true", () => {
  const st = P.statementFor("B", inputs({ netContrib: null }));
  assert.equal(P.valueOf(st, "net_contrib"), null);
  assert.equal(st.hasGaps, true);
  const line = P.lineOf(st, "net_contrib");
  assert.match(line.gapReason, /fail-closed/);
});

test("全部缺失 ⇒ 所有行 null、hasGaps=true、绝不出现 0", () => {
  const empty = P.statementFor("A", {
    grossListing: null, sellerDiscount: null, platformDiscount: null, netRevenue: null,
    cogs: null, gp: null, gmp: null, marketing: null, cm1: null, platformFee: null,
    cm2: null, overheadAlloc: null, netContrib: null,
  });
  assert.equal(empty.hasGaps, true);
  for (const l of empty.lines) {
    assert.equal(l.value, null, `${l.id} 应为 null 而非 0`);
  }
});

test("★ 真实 0 不被误判为缺失（0 是合法值）", () => {
  const st = P.statementFor("B", inputs({ sellerDiscount: 0, grossListing: 0 }));
  assert.equal(P.valueOf(st, "seller_discount"), 0);
  assert.equal(P.lineOf(st, "seller_discount").gapReason, undefined);
});

// ───────────────────────────── 结构与回溯 ─────────────────────────────

test("分层顺序与契约一致（收入→毛利→贡献），不重排", () => {
  const st = P.statementFor("B", inputs());
  const ids = st.lines.map((l) => l.id);
  assert.deepEqual(ids, [
    "gross_listing", "seller_discount", "platform_discount", "net_revenue",
    "cogs", "gp", "gmp", "marketing", "cm1", "platform_fee", "cm2",
    "overhead_alloc", "net_contrib",
  ]);
});

test("每行都带 algoId（docs/03 §5 可回溯）", () => {
  const st = P.statementFor("B", inputs());
  for (const l of st.lines) {
    assert.ok(l.algoId && l.algoId.startsWith("algo."), `${l.id} 缺少 algoId`);
  }
});

test("meta 里的 dataSlots 被带入（口径追溯）", () => {
  const st = P.statementFor("B", inputs(), {
    meta: { net_revenue: { dataSlots: ["slot.revenue", "slot.discount"] } },
  });
  assert.deepEqual(P.lineOf(st, "net_revenue").dataSlots, ["slot.revenue", "slot.discount"]);
});

test("报表带契约版本、口径、模块", () => {
  const st = P.statementFor("B", inputs(), { module: "module.pnl" });
  assert.equal(st.v, "1.0");
  assert.equal(st.caliber, "B");
  assert.equal(st.module, "module.pnl");
});

// ───────────────────────────── 用户提示 ─────────────────────────────

test("caliberWarning: 两口径都给出「容易被误读成什么」", () => {
  assert.match(P.caliberWarning("A"), /费用失控/);
  assert.match(P.caliberWarning("B"), /收入侧被压低|卖不动/);
  assert.match(P.caliberWarning("A"), /计入营销费用/);
  assert.match(P.caliberWarning("B"), /contra-revenue/);
});

test("caliberDiffSummary: 说清「变了什么」，也明说「净收入没变」", () => {
  const cmp = P.compareCalibers(inputs());
  const s = P.caliberDiffSummary(cmp);
  assert.match(s, /营销费用/);
  assert.match(s, /保持不变/, "★ 必须主动安抚：净收入未变，否则用户怀疑数据被改");
});

test("KODP 实测场景可复现：折扣很深时 A 口径净贡献与 B 口径可能符号不同", () => {
  // 承袭 docs/03 §4.3 的实测结构：折扣除以费用侧后 CM 段被显著压低。
  // 这里只验证「营销费用按口径变化 → 下游 CM1/CM2 会随口径解读不同」，
  // 但**净收入仍恒等**（不变量）。
  const deep = inputs({ sellerDiscount: 400_000, marketing: 80_000, netRevenue: 580_000 });
  const a = P.statementFor("A", deep);
  const b = P.statementFor("B", deep);
  assert.equal(P.valueOf(a, "marketing"), 480_000, "A 口径营销费用虚高");
  assert.equal(P.valueOf(b, "marketing"), 80_000);
  assert.equal(P.valueOf(a, "net_revenue"), P.valueOf(b, "net_revenue"), "★ 净收入仍恒等");
  assert.equal(P.assertNetRevenueInvariant(a, b).ok, true);
});
