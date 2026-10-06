/**
 * test/strategy.test.mjs —— M-STRATEGY 前端（选型交互 + 回滚 + 历史）
 *
 * 重点覆盖四类**真实会出事**的边界：
 *  1. 用户不可能构造出「卡外选项」的动作（越权/脏数据）；
 *  2. 「保持现状」是一等公民，不被当成「取消」；
 *  3. 不可回滚的选项必须**在选之前**就有明确提示；
 *  4. 回滚可用性只认服务端的 lastStableOption，不由前端推断。
 */

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { build } from "esbuild";
import { join } from "node:path";
import { pathToFileURL, fileURLToPath } from "node:url";

const WEB = fileURLToPath(new URL("..", import.meta.url));
let lab;

before(async () => {
  const out = join(WEB, ".test-build", "strategy.mjs");
  await build({
    entryPoints: [join(WEB, "src", "strategy", "lab.ts")],
    outfile: out,
    bundle: true,
    format: "esm",
    platform: "neutral",
    logLevel: "silent",
  });
  lab = await import(pathToFileURL(out).href);
});

// ───────────────────────────── 夹具 ─────────────────────────────

/** 一张合法卡：3 个选项（含一个不可回滚的高风险项），current = "a"。 */
function makeCard(over = {}) {
  return {
    id: "ch.fee",
    title: "渠道费率口径",
    context: "Shopee 费率采集暂缓，当前用估算值，需决定是否切换口径",
    current: opt("a", "low", true),
    options: [
      opt("a", "low", true),
      opt("b", "medium", true),
      opt("c", "high", false), // 高风险 + 不可回滚
    ],
    impactPreview: {
      metrics: ["net_rate", "gp"],
      delta: {
        a: { net_rate: "不变" },
        b: { net_rate: "34.45% → 36.14%" },
        c: { net_rate: "34.45% → 38.90%" },
      },
      affectedBuckets: ["pnl_month"],
    },
    blocking: true,
    ...over,
  };
}

function opt(key, risk, reversible, over = {}) {
  return {
    key,
    label: `选项 ${key.toUpperCase()}`,
    description: `说明 ${key}`,
    expectedEffect: "净利率 34.45% → 36.14%",
    risk,
    reversible,
    ...over,
  };
}

// ───────────────────────────── 校验 ─────────────────────────────

test("合法卡无问题且可渲染", () => {
  const c = makeCard();
  assert.deepEqual(lab.validateChoice(c), []);
  assert.equal(lab.isRenderable(c), true);
});

test("nil 卡报问题", () => {
  assert.ok(lab.validateChoice(null).length > 0);
});

test("缺 context 被拦下", () => {
  const c = makeCard({ context: "" });
  assert.ok(lab.validateChoice(c).some((p) => p.includes("context")));
  assert.equal(lab.isRenderable(c), false);
});

test("选项数越界被拦下", () => {
  assert.ok(
    lab.validateChoice(makeCard({ options: [opt("a", "low", true)] })).some((p) =>
      p.includes("越界"),
    ),
  );
  const five = ["a", "b", "c", "d", "e"].map((k) => opt(k, "low", true));
  assert.ok(lab.validateChoice(makeCard({ options: five })).some((p) => p.includes("越界")));
});

test("current 不在候选集内被拦下", () => {
  const c = makeCard({ current: opt("zzz", "low", true) });
  assert.ok(lab.validateChoice(c).some((p) => p.includes("候选集")));
});

test("重复 key 被拦下", () => {
  const c = makeCard({ options: [opt("a", "low", true), opt("a", "medium", true)] });
  assert.ok(lab.validateChoice(c).some((p) => p.includes("重复")));
});

test("缺 expectedEffect 被拦下", () => {
  const c = makeCard();
  c.options[1].expectedEffect = "";
  assert.ok(lab.validateChoice(c).some((p) => p.includes("expectedEffect")));
});

// ───────────────────────────── 视图模型 ─────────────────────────────

test("buildChoiceVM 标出当前值", () => {
  const vm = lab.buildChoiceVM(makeCard());
  const cur = vm.options.filter((o) => o.isCurrent);
  assert.equal(cur.length, 1);
  assert.equal(cur[0].key, "a");
  assert.equal(vm.options.find((o) => o.key === "a").changes, false);
  assert.equal(vm.options.find((o) => o.key === "b").changes, true);
});

test("buildChoiceVM 不修改入参", () => {
  const c = makeCard();
  const before = JSON.stringify(c);
  lab.buildChoiceVM(c);
  assert.equal(JSON.stringify(c), before, "卡片被原地修改了");
});

test("高风险或不可回滚需要二次确认", () => {
  const vm = lab.buildChoiceVM(makeCard());
  const byKey = Object.fromEntries(vm.options.map((o) => [o.key, o]));
  assert.equal(byKey.a.needsConfirm, false, "低风险可回滚不该要确认");
  assert.equal(byKey.b.needsConfirm, false, "中风险可回滚不该要确认");
  assert.equal(byKey.c.needsConfirm, true, "高风险+不可回滚必须确认");
});

test("★ 不可回滚必须在选项中提前说明", () => {
  const vm = lab.buildChoiceVM(makeCard());
  const c = vm.options.find((o) => o.key === "c");
  assert.ok(c.irreversibleNote, "不可回滚的选项必须有提示文案");
  assert.match(c.irreversibleNote, /不可回滚/);
  assert.equal(vm.options.find((o) => o.key === "a").irreversibleNote, null);
});

test("风险展示元数据齐备", () => {
  const vm = lab.buildChoiceVM(makeCard());
  for (const o of vm.options) {
    assert.ok(o.riskLabel, `${o.key} 缺 riskLabel`);
    assert.ok(o.riskHint, `${o.key} 缺 riskHint`);
  }
  assert.equal(lab.RISK_META.high.label, "高风险");
});

test("blocking 卡给出阻塞提示", () => {
  const vm = lab.buildChoiceVM(makeCard({ blocking: true }));
  assert.ok(vm.blockingNote);
  assert.match(vm.blockingNote, /阻塞/);
  assert.equal(lab.buildChoiceVM(makeCard({ blocking: false })).blockingNote, null);
});

test("impactVM 说明是否需要重算", () => {
  const vm = lab.buildChoiceVM(makeCard());
  assert.equal(vm.impact.requiresRecompute, true);
  assert.match(vm.impact.bucketNote, /重算 1 个/);
  assert.match(vm.impact.bucketNote, /pnl_month/);
});

test("无受影响桶时明确说「不影响」", () => {
  const c = makeCard({
    impactPreview: { metrics: [], delta: {}, affectedBuckets: [] },
  });
  const vm = lab.buildChoiceVM(c);
  assert.equal(vm.impact.requiresRecompute, false);
  assert.match(vm.impact.bucketNote, /不影响/);
});

test("缺 impactPreview 不崩", () => {
  const c = makeCard({ impactPreview: undefined });
  const vm = lab.buildChoiceVM(c);
  assert.equal(vm.impact.requiresRecompute, false);
  assert.deepEqual(vm.impact.metrics, []);
});

test("decideBy 逾期判定", () => {
  const past = lab.buildChoiceVM(
    makeCard({ decideBy: "2020-01-01T00:00:00Z" }),
    new Date("2026-01-01T00:00:00Z"),
  );
  assert.equal(past.overdue, true);
  const future = lab.buildChoiceVM(
    makeCard({ decideBy: "2030-01-01T00:00:00Z" }),
    new Date("2026-01-01T00:00:00Z"),
  );
  assert.equal(future.overdue, false);
});

test("非法 decideBy 不误报逾期", () => {
  const vm = lab.buildChoiceVM(makeCard({ decideBy: "not-a-date" }));
  assert.equal(vm.overdue, false, "无法解析的时间戳不应算逾期");
});

test("坏卡仍可产出 VM 但标记不可渲染", () => {
  const vm = lab.buildChoiceVM(makeCard({ context: "" }));
  assert.equal(vm.renderable, false);
  assert.ok(vm.problems.length > 0);
});

// ───────────────────────────── 动作构造 ─────────────────────────────

test("chooseAction 构造合法动作", () => {
  const a = lab.chooseAction(makeCard(), "b");
  assert.deepEqual(a, { kind: "choose", optionKey: "b" });
});

test("★ chooseAction 拒绝卡外选项", () => {
  assert.throws(
    () => lab.chooseAction(makeCard(), "zzz"),
    (e) => e.name === "DecisionError" && e.reason === "unknown_option",
  );
});

test("★ chooseAction 拒绝坏卡", () => {
  assert.throws(
    () => lab.chooseAction(makeCard({ context: "" }), "b"),
    (e) => e.name === "DecisionError" && e.reason === "invalid_choice",
  );
});

test("★ 不存在任何「自由输入」的动作构造入口", () => {
  // 公开 API 里不该有接收公式/数值的函数
  const names = Object.keys(lab).filter((k) => typeof lab[k] === "function");
  for (const n of names) {
    assert.doesNotMatch(
      n,
      /formula|setValue|applyRaw|freeform|writeExpr/i,
      `不应暴露自由输入入口：${n}`,
    );
  }
});

test("keepCurrentAction 是一等公民动作", () => {
  assert.deepEqual(lab.keepCurrentAction(), { kind: "keep_current" });
});

test("isChangeAction 区分变更与保持", () => {
  const c = makeCard();
  assert.equal(lab.isChangeAction(c, lab.keepCurrentAction()), false);
  assert.equal(lab.isChangeAction(c, lab.chooseAction(c, "a")), false, "选当前值不算变更");
  assert.equal(lab.isChangeAction(c, lab.chooseAction(c, "b")), true);
});

test("actionTargetKey 解析目标", () => {
  const c = makeCard();
  assert.equal(lab.actionTargetKey(c, lab.keepCurrentAction()), "a");
  assert.equal(lab.actionTargetKey(c, lab.chooseAction(c, "b")), "b");
});

test("动作结构里只有 kind/optionKey", () => {
  const cases = [lab.keepCurrentAction(), lab.chooseAction(makeCard(), "b")];
  for (const a of cases) {
    const keys = Object.keys(a).sort();
    assert.ok(
      keys.join(",") === "kind" || keys.join(",") === "kind,optionKey",
      `动作含多余字段：${keys.join(",")}`,
    );
  }
});

// ───────────────────────────── 回滚可用性 ─────────────────────────────

test("★ 回滚可用性：有稳定版本则可回滚", () => {
  const r = lab.rollbackAvailability("a", "b");
  assert.equal(r.available, true);
  assert.equal(r.targetKey, "a");
  assert.equal(r.label, "回滚到 a");
  assert.equal(r.reason, null);
});

test("★ 回滚不可用：无稳定版本", () => {
  const r = lab.rollbackAvailability(null, "b");
  assert.equal(r.available, false);
  assert.equal(r.targetKey, null);
  assert.match(r.reason, /不可回滚|尚无变更历史/);
});

test("★ 回滚不可用：字段缺失（客户端集成 bug）要说清楚", () => {
  const r = lab.rollbackAvailability(undefined, "b");
  assert.equal(r.available, false);
  assert.match(r.reason, /未取得可回滚版本信息/);
});

test("★ 回滚不可用：当前已是稳定版本（避免空操作）", () => {
  const r = lab.rollbackAvailability("a", "a");
  assert.equal(r.available, false);
  assert.match(r.label, /已是稳定版本/);
  assert.match(r.reason, /无需回滚/);
});

test("回滚可用性不接受历史数组推断", () => {
  // ★ 刻意只收 lastStableOption：若传数组进来，说明调用方想自己推，
  //   那正是要防止的漂移源。函数签名决定了这种用法会算出错误结果。
  const r = lab.rollbackAvailability([], "b");
  assert.equal(r.available, false, "空数组（真值）不应被当作「可回滚」");
});

test("buildRollbackPlanVM 提示重算", () => {
  const vm = lab.buildRollbackPlanVM({
    choiceId: "ch.fee",
    fromOption: "b",
    toOption: "a",
    undoOfEntryId: "h1",
    affectedBuckets: ["pnl_month"],
    note: "将「渠道费率口径」从 b 回滚到 a",
  });
  assert.equal(vm.fromOption, "b");
  assert.equal(vm.toOption, "a");
  assert.match(vm.bucketNote, /重算 1 个/);
  assert.match(vm.confirmText, /b.*a/);
});

test("无桶的回滚计划明确说不影响", () => {
  const vm = lab.buildRollbackPlanVM({
    choiceId: "x",
    fromOption: "b",
    toOption: "a",
    undoOfEntryId: "",
    affectedBuckets: [],
    note: "",
  });
  assert.match(vm.bucketNote, /不影响/);
});

// ───────────────────────────── 历史展示 ─────────────────────────────

test("★ 保持现状记录显示为「保持 X」而不是「X → X」", () => {
  const rows = lab.buildHistoryRows([
    {
      id: "h1",
      choiceId: "ch.fee",
      fromOption: "a",
      toOption: "a",
      decidedBy: "ceo",
      decidedAt: "2026-01-01T00:00:00Z",
      dataSnapshotHash: "sha256:abcdef1234567890",
    },
  ]);
  assert.equal(rows[0].transition, "保持 a");
  assert.equal(rows[0].kept, true);
  assert.equal(rows[0].actionLabel, "保持现状");
});

test("变更记录显示为 A → B", () => {
  const rows = lab.buildHistoryRows([
    {
      id: "h1",
      choiceId: "ch.fee",
      fromOption: "a",
      toOption: "b",
      decidedBy: "ceo",
      decidedAt: "2026-01-01T00:00:00Z",
      dataSnapshotHash: "sha256:abc",
    },
  ]);
  assert.equal(rows[0].transition, "a → b");
  assert.equal(rows[0].kept, false);
  assert.equal(rows[0].actionLabel, "变更");
});

test("未回填效果时明说「待评估」而非留空", () => {
  const rows = lab.buildHistoryRows([
    {
      id: "h1",
      choiceId: "ch.fee",
      fromOption: "a",
      toOption: "b",
      decidedBy: "ceo",
      decidedAt: "t",
      dataSnapshotHash: "sha256:abc",
    },
  ]);
  assert.equal(rows[0].effect, "待评估");
});

test("已回填效果如实展示", () => {
  const rows = lab.buildHistoryRows([
    {
      id: "h1",
      choiceId: "ch.fee",
      fromOption: "a",
      toOption: "b",
      decidedBy: "ceo",
      decidedAt: "t",
      dataSnapshotHash: "sha256:abc",
      actualEffect: "净利率实际 35.9%",
    },
  ]);
  assert.equal(rows[0].effect, "净利率实际 35.9%");
});

test("快照哈希短形", () => {
  assert.equal(lab.shortSnapshot(""), "—");
  assert.equal(lab.shortSnapshot("sha256:abcd"), "sha256:abcd");
  assert.equal(lab.shortSnapshot("sha256:abcdef1234567890"), "sha256:abcde…");
});

test("历史统计区分变更与保持", () => {
  const s = lab.historyStats([
    { fromOption: "a", toOption: "b" },
    { fromOption: "b", toOption: "b" },
    { fromOption: "b", toOption: "c" },
  ]);
  assert.deepEqual(s, { total: 3, changes: 2, kept: 1 });
});

test("空历史统计为 0", () => {
  assert.deepEqual(lab.historyStats([]), { total: 0, changes: 0, kept: 0 });
});

// ───────────────────────────── 采纳学习 ─────────────────────────────

test("★ 样本不足时不重排选项", () => {
  const c = makeCard();
  const opts = lab.orderByPreference(c, { account: "ceo", riskAppetite: 1, samples: 2, note: "" });
  assert.deepEqual(
    opts.map((o) => o.key),
    ["a", "b", "c"],
    "样本 <3 时不应重排（过拟合会让列表莫名跳动）",
  );
});

test("推荐项优先", () => {
  const c = makeCard();
  c.options[1].recommended = true;
  const opts = lab.orderByPreference(c, { account: "ceo", riskAppetite: 0, samples: 10, note: "" });
  assert.equal(opts[0].key, "b");
});

test("激进偏好把高风险排前", () => {
  const c = makeCard();
  const opts = lab.orderByPreference(c, { account: "ceo", riskAppetite: 1, samples: 10, note: "" });
  assert.equal(opts[0].key, "c", "激进偏好应把高风险项排前");
});

test("保守偏好把低风险排前", () => {
  const c = makeCard();
  const opts = lab.orderByPreference(c, { account: "ceo", riskAppetite: -1, samples: 10, note: "" });
  assert.equal(opts[0].key, "a", "保守偏好应把低风险项排前");
});

test("无偏好时保持原顺序", () => {
  const c = makeCard();
  const opts = lab.orderByPreference(c, null);
  assert.deepEqual(opts.map((o) => o.key), ["a", "b", "c"]);
});

test("orderByPreference 不修改入参", () => {
  const c = makeCard();
  const before = JSON.stringify(c.options);
  lab.orderByPreference(c, { account: "x", riskAppetite: 1, samples: 10, note: "" });
  assert.equal(JSON.stringify(c.options), before);
});

// ───────────────────────────── 闭环阶段 ─────────────────────────────

test("闭环五阶段齐备", () => {
  assert.deepEqual([...lab.LOOP_STAGES], ["observe", "propose", "decide", "apply", "evaluate"]);
  for (const s of lab.LOOP_STAGES) {
    assert.ok(lab.LOOP_STAGE_META[s].label, `${s} 缺 label`);
    assert.ok(lab.LOOP_STAGE_META[s].hint, `${s} 缺 hint`);
  }
});

test("currentStage 有决策 ⇒ decide", () => {
  assert.equal(lab.currentStage([makeCard()], true), "decide");
});

test("currentStage 无决策有卡 ⇒ observe", () => {
  assert.equal(lab.currentStage([makeCard()], false), "observe");
});

test("currentStage 无卡 ⇒ propose", () => {
  assert.equal(lab.currentStage([], false), "propose");
});

test("契约版本为 1.0", () => {
  assert.equal(lab.STRATEGY_VERSION, "1.0");
});
