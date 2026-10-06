/**
 * src/strategy/lab.ts —— M-STRATEGY 策略实验室
 *
 * 职责（docs/01 §7、docs/02 M-STRATEGY）：
 *   把「自迭代闭环」中的 Decide 阶段做成**纯前端可穷举的选型交互**，
 *   并保证用户侧只有三种动作：选 A / 选 B(…) / 保持现状。
 *
 * ══════════════════════════════════════════════════════════════════════════
 * ★★ 边界纪律（由 scripts/layering-gate.mjs 静态强制）：
 *
 *   1. **不发起网络请求。** 卡片的来源、历史的落库、参数的应用
 *      全由装配层经 M-QUERY 完成。本模块只产出「要提交什么」。
 *      这样选型交互可以被纯单测穷举，不必起 fetch 替身。
 *
 *   2. **不做业务换算。** 本模块只处理**结构与状态**：
 *      哪张卡、选哪个、影响哪些桶、能不能回滚。
 *      所有 expectedEffect 都是后端算好的人话字符串，本模块**不拼**。
 *      （★ 若让前端拼「净利率 34.45% → 36.14%」，同一变化在不同页面
 *        会出现不同措辞，用户会以为看到了两个不同的结论。）
 *
 *   3. **不含权限判断。** D6「仅管理层」由后端 ScopeGuard 决定。
 *      前端的隐藏仅用于**减少误触**，绝不作为访问控制 ——
 *      那是典型的「前端权限」反模式：改一行 JS 就能绕过。
 * ══════════════════════════════════════════════════════════════════════════
 *
 * ★ 四条纪律（都对应真实会踩的坑）：
 *
 *   1. **绝不提供自由输入。** 决策只有 `choose(optionKey)` 与
 *      `keep_current` 两种动作。本模块的公开 API 里**不存在**
 *      接收公式/数值的函数 —— 这不是靠校验堵住，是刻意不给入口。
 *      理由见 backend/internal/strategy/strategy.go 包注释。
 *
 *   2. **「保持现状」必须显式可点，且必须留痕。** 它不是一个
 *      「关闭弹窗」的副作用，而是一个**有意义的决策**：它记录了
 *      「评估过，决定不改」。若不给这个按钮，用户为了继续就只能随便选一个。
 *
 *   3. **不可回滚的选项必须在选之前就说清楚。** 高风险 + 不可逆
 *      要在卡片上带确认门槛；若选完才告知「这个回不去」，
 *      用户已经在无退路的状态里了。
 *
 *   4. **回滚按钮的存在性由历史决定，不由当前值推断。**
 *      「有没有可回滚的版本」只有服务端从 append-only 历史里算得准；
 *      前端若自己推（例如「current != 初始值就可回滚」），
 *      在「不可逆变更」场景下会给出一个点了必然失败（409）的按钮。
 */

import {
  STRATEGY_CHOICE_VERSION,
  type ChoiceOption,
  type DecisionAction,
  type ImpactPreview,
  type StrategyChoice,
  type StrategyHistoryEntry,
} from "../contracts/strategy-choice.js";

/** 策略契约版本（与后端 strategy.Version 对齐）。 */
export const STRATEGY_VERSION = STRATEGY_CHOICE_VERSION;

/** 候选数量上下界（与后端 strategy.MinOptions / MaxOptions 对齐）。 */
export const MIN_OPTIONS = 2;
export const MAX_OPTIONS = 4;

/** 风险等级展示元数据（仅用于 UI 着色与文案，**不是**访问控制）。 */
export const RISK_META: Record<ChoiceOption["risk"], { label: string; hint: string }> = {
  low: { label: "低风险", hint: "影响面小，可随时回滚" },
  medium: { label: "中风险", hint: "会改变部分指标，仍可回滚" },
  high: { label: "高风险", hint: "影响面大，可能不可回滚" },
};

// ───────────────────────────── 校验 ─────────────────────────────

/** 卡片校验问题（人话，直接可展示）。 */
export type ChoiceProblem = string;

/**
 * 校验一张选型卡是否可安全渲染与决策。
 *
 * ★ 与后端 strategy.Choice.Validate 同源但**不重复实现语义**：
 *   这里只做前端渲染所必需的检查（结构自洽 + 数量界）。
 *   若前端也实现一份「候选集守卫」，两处迟早漂移 ——
 *   真正的守卫在后端，前端这份只为「坏卡不渲染」。
 */
export function validateChoice(c: StrategyChoice): ChoiceProblem[] {
  const problems: ChoiceProblem[] = [];
  if (!c) return ["选型卡为空"];
  if (!c.id) problems.push("id 不能为空");
  if (!c.title) problems.push("title 不能为空（用户靠标题判断在决策什么）");
  if (!c.context) problems.push("context 不能为空（必须说明为什么要决策）");
  if (c.options.length < MIN_OPTIONS || c.options.length > MAX_OPTIONS) {
    problems.push(
      `候选选项数 ${c.options.length} 越界（应为 ${MIN_OPTIONS}–${MAX_OPTIONS} 个）`,
    );
  }
  const keys = new Set<string>();
  for (const o of c.options) {
    if (!o.key) problems.push("存在空 key 的选项");
    else if (keys.has(o.key)) problems.push(`选项 key 「${o.key}」重复`);
    keys.add(o.key);
    if (!o.expectedEffect) {
      problems.push(`选项「${o.label || o.key}」缺 expectedEffect（每个选项都必须给出预期影响）`);
    }
  }
  if (!c.current?.key) {
    problems.push("缺 current（当前生效值）");
  } else if (!keys.has(c.current.key)) {
    problems.push(`current「${c.current.key}」不在候选集内`);
  }
  return problems;
}

/** 卡片是否可渲染（无问题）。 */
export function isRenderable(c: StrategyChoice): boolean {
  return validateChoice(c).length === 0;
}

// ───────────────────────────── 视图模型 ─────────────────────────────

/** 一个选项的展示态。 */
export interface OptionVM {
  key: string;
  label: string;
  description: string;
  expectedEffect: string;
  risk: ChoiceOption["risk"];
  riskLabel: string;
  riskHint: string;
  /** 是否为当前生效值。 */
  isCurrent: boolean;
  /** 是否被系统推荐。 */
  recommended: boolean;
  /** 选中该选项是否有实际改动。 */
  changes: boolean;
  /**
   * 是否需要二次确认才能选。
   * 判据：高风险 **或** 不可回滚 —— 两者都意味着「点下去要停一下」。
   */
  needsConfirm: boolean;
  /** 不可回滚时给出的明确提示（避免选完才发现无退路）。 */
  irreversibleNote: string | null;
}

/** 影响预览的展示态。 */
export interface ImpactVM {
  metrics: string[];
  /** optionKey → (metricKey → 变化描述)。 */
  delta: Record<string, Record<string, string>>;
  /** affectedBuckets 的人话说明。 */
  bucketNote: string;
  /** 是否有需要重算的桶（false 时说明「本次无需重算」）。 */
  requiresRecompute: boolean;
}

/** 一张选型卡的展示态。 */
export interface ChoiceVM {
  id: string;
  title: string;
  context: string;
  /** 当前生效选项的展示文案。 */
  currentLabel: string;
  options: OptionVM[];
  impact: ImpactVM;
  blocking: boolean;
  /** 阻塞提示（blocking=true 时非空）。 */
  blockingNote: string | null;
  decideBy: string | null;
  /** 是否已逾期（decideBy 早于 now）。 */
  overdue: boolean;
  problems: ChoiceProblem[];
  renderable: boolean;
}

/** 影响预览 → VM。 */
export function impactVM(impact: ImpactPreview | undefined): ImpactVM {
  const metrics = impact?.metrics ?? [];
  const delta = impact?.delta ?? {};
  const buckets = impact?.affectedBuckets ?? [];
  return {
    metrics,
    delta,
    // ★ 人话说明而不是让前端各自拼：桶名对用户毫无意义，
    //   必须给出「会影响多少东西、要不要重算」的结论。
    bucketNote:
      buckets.length === 0
        ? "本次选择不影响任何预计算数据"
        : `选定后需重算 ${buckets.length} 个预计算桶（${buckets.join("、")}），重算期间相关报表会显示「待更新」`,
    requiresRecompute: buckets.length > 0,
  };
}

/**
 * 把一张卡转成展示态。
 *
 * ★ 不修改入参（返回全新对象）：卡片可能同时被多处渲染，
 *   若原地补字段，两处会互相污染。
 */
export function buildChoiceVM(c: StrategyChoice, now: Date = new Date()): ChoiceVM {
  const problems = validateChoice(c);
  const renderable = problems.length === 0;
  const cur = c?.current?.key ?? "";

  // ★ 用 contract 的 options 而非从 current 反查：current 可能是
  //   「一张只读的当前值快照」，而 options 才是完整候选集。
  const options: OptionVM[] = (c?.options ?? []).map((o) => {
    const changes = o.key !== cur;
    return {
      key: o.key,
      label: o.label,
      description: o.description,
      expectedEffect: o.expectedEffect,
      risk: o.risk,
      riskLabel: RISK_META[o.risk]?.label ?? o.risk,
      riskHint: RISK_META[o.risk]?.hint ?? "",
      isCurrent: o.key === cur,
      recommended: o.recommended === true,
      changes,
      // ★ 高风险 **或** 不可回滚都要二次确认：
      //   高风险 = 「可能后悔」，不可回滚 = 「后悔了也回不去」。
      //   后者更需要停一下，因为它的代价不可撤销。
      needsConfirm: o.risk === "high" || o.reversible === false,
      // ★ 不可回滚必须**提前**说清楚（纪律 3）：
      //   若等选完才告知「这个回不去」，用户已在无退路状态里了。
      irreversibleNote:
        o.reversible === false
          ? "此选项不可回滚 —— 选定后将无法通过「回滚」恢复，需走人工流程"
          : null,
    };
  });

  const decideBy = c?.decideBy ?? null;
  let overdue = false;
  if (decideBy) {
    const t = Date.parse(decideBy);
    // 无法解析的时间戳不当作逾期（宁可少提示，不要误报）
    if (!Number.isNaN(t)) overdue = t < now.getTime();
  }

  return {
    id: c?.id ?? "",
    title: c?.title ?? "",
    context: c?.context ?? "",
    currentLabel: c?.current?.label ?? c?.current?.key ?? "（未设置）",
    options,
    // ★ 字段名必须与契约一致：contracts/strategy-choice.ts 里叫 impactPreview
    //   （后端 Go 结构体是 Impact，但 JSON tag 也是 impactPreview）。
    //   写成 c.impact 会静默拿到 undefined —— 预览面板整块消失但不报错。
    impact: impactVM(c?.impactPreview),
    blocking: c?.blocking === true,
    blockingNote: c?.blocking
      ? "此项决策会阻塞其他模块落地，建议尽快处理"
      : null,
    decideBy,
    overdue,
    problems,
    renderable,
  };
}

// ───────────────────────────── 动作构造 ─────────────────────────────

/** 动作构造失败（对应后端 422）。 */
export class DecisionError extends Error {
  readonly reason: "unknown_option" | "invalid_choice";
  constructor(reason: DecisionError["reason"], message: string) {
    super(message);
    this.name = "DecisionError";
    this.reason = reason;
  }
}

/**
 * 构造「选某选项」的动作。
 *
 * ★ 这是本模块的**唯一**构造变更类动作的入口。它只接受 optionKey，
 *   且必须命中候选集 —— 结构上就没有「传公式」的可能。
 */
export function chooseAction(c: StrategyChoice, optionKey: string): DecisionAction {
  const problems = validateChoice(c);
  if (problems.length > 0) {
    throw new DecisionError("invalid_choice", `选型卡不合法：${problems.join("；")}`);
  }
  if (!c.options.some((o) => o.key === optionKey)) {
    // 不回显候选集：调用方已经持有卡片，回显只会让错误信息变长
    throw new DecisionError("unknown_option", `所选选项「${optionKey}」不在候选集内`);
  }
  return { kind: "choose", optionKey };
}

/**
 * 构造「保持现状」动作。
 *
 * ★ 这是一个**一等公民**动作，不是「取消」的同义词：
 *   它会被作为一条决策记录落库（from == to），用于事后回答
 *   「你们评估过这个信号吗？当时为什么不改？」
 *
 * ★ 但它**不**应触发预计算重算（后端已保证）：否则每次点它都会
 *   让全月数据变 STALE，用户会看到「什么都没改，报表却全在重算」。
 */
export function keepCurrentAction(): DecisionAction {
  return { kind: "keep_current" };
}

/** 动作是否构成实质变更。 */
export function isChangeAction(c: StrategyChoice, a: DecisionAction): boolean {
  if (a.kind === "keep_current") return false;
  return a.optionKey !== (c?.current?.key ?? "");
}

/** 动作的目标选项 key（keep_current ⇒ 当前值）。 */
export function actionTargetKey(c: StrategyChoice, a: DecisionAction): string {
  return a.kind === "keep_current" ? (c?.current?.key ?? "") : a.optionKey;
}

// ───────────────────────────── 回滚 ─────────────────────────────

/** 回滚计划（与后端 strategy.RollbackPlan 对应）。 */
export interface RollbackPlan {
  choiceId: string;
  fromOption: string;
  toOption: string;
  undoOfEntryId: string;
  affectedBuckets: string[];
  note: string;
}

/** 回滚可行性（由**服务端历史**决定，不由前端推断 —— 见纪律 4）。 */
export interface RollbackAvailability {
  /** 是否可回滚。 */
  available: boolean;
  /** 可回滚到的目标选项 key（不可回滚时为 null）。 */
  targetKey: string | null;
  /** 按钮文案（不可回滚时说明原因）。 */
  label: string;
  /** 不可回滚时的原因说明。 */
  reason: string | null;
}

/**
 * 依据**服务端返回的历史**判断回滚可用性。
 *
 * @param lastStableOption 服务端 /api/strategy/history 返回的 lastStableOption
 *                         （null 表示「无可回滚版本」）
 * @param currentKey       当前生效选项 key
 *
 * ★ 关键设计：**不接受「历史数组」当输入**，只接受服务端算好的
 *   lastStableOption。前端自己从历史里推「上一稳定版本」必然与后端
 *   漂移 —— 尤其「遇到不可逆变更要停住」这条规则很容易被漏掉，
 *   结果是前端给出一个点了必然 409 的按钮。
 */
export function rollbackAvailability(
  lastStableOption: string | null,
  currentKey: string,
): RollbackAvailability {
  // ★ 区分三种「没有可回滚版本」，因为它们的处置完全不同：
  //
  //   null      → 契约内的**正常**回答（服务端明确说「无稳定版本」）。
  //              这是最需要给出准确原因的情况：用户会问「为什么回不去」。
  //   undefined → 字段缺失 ⇒ 调用方没接服务端的 lastStableOption，
  //              是**客户端集成 bug**，必须说出来而不是伪装成业务状态。
  //   其他类型  → 有人把 history 数组传进来了（正是要防的用法）。
  if (lastStableOption === null || lastStableOption === undefined) {
    if (lastStableOption === undefined) {
      return {
        available: false,
        targetKey: null,
        label: "无可回滚版本",
        reason:
          "未取得可回滚版本信息（应由服务端 /api/strategy/history 返回 lastStableOption）",
      };
    }
    return {
      available: false,
      targetKey: null,
      label: "无可回滚版本",
      reason:
        "该卡尚无变更历史，或最近一次变更不可回滚（例如口径重算已覆写历史数据）",
    };
  }
  if (typeof lastStableOption !== "string" || lastStableOption.length === 0 || !currentKey) {
    return {
      available: false,
      targetKey: null,
      label: "无可回滚版本",
      reason: "可回滚版本信息格式异常（期望选项 key 字符串）",
    };
  }
  if (lastStableOption === currentKey) {
    // 已经在稳定版本上：再回滚是空操作，明确禁用而不是让用户点了没反应
    return {
      available: false,
      targetKey: null,
      label: "已是稳定版本",
      reason: `当前值 ${currentKey} 本身即上一稳定版本，无需回滚`,
    };
  }
  return {
    available: true,
    targetKey: lastStableOption,
    label: `回滚到 ${lastStableOption}`,
    reason: null,
  };
}

/** 回滚计划的展示态。 */
export interface RollbackPlanVM {
  fromOption: string;
  toOption: string;
  note: string;
  bucketNote: string;
  /** 二次确认提示语。 */
  confirmText: string;
}

/**
 * 回滚计划 → 展示态。
 *
 * ★ 回滚也必须提示「要重算」：很多人以为「回滚 = 恢复原状 = 数据本来就在」，
 *   但桶里存的是**用变更后参数算出来的**数。只改参数不重算，
 *   报表会拿旧参数去回答已按新参数算好的桶 —— 数字错得毫无征兆。
 */
export function buildRollbackPlanVM(p: RollbackPlan): RollbackPlanVM {
  const n = p.affectedBuckets?.length ?? 0;
  return {
    fromOption: p.fromOption,
    toOption: p.toOption,
    note: p.note,
    bucketNote:
      n === 0
        ? "本次回滚不影响预计算数据"
        : `回滚同样需要重算 ${n} 个预计算桶（${p.affectedBuckets.join("、")}）—— 桶里存的是变更后参数算出的数`,
    confirmText: `将「${p.fromOption}」回滚到「${p.toOption}」，确定继续？`,
  };
}

// ───────────────────────────── 历史展示 ─────────────────────────────

/** 一条历史记录的展示态。 */
export interface HistoryRowVM {
  id: string;
  /** 「A → B」或「保持 A」。 */
  transition: string;
  /** 是否是「保持现状」记录。 */
  kept: boolean;
  decidedBy: string;
  decidedAt: string;
  /** 决策依据（快照哈希，短形便于展示）。 */
  snapshot: string;
  /** 实际效果（未回填时的占位文案）。 */
  effect: string;
  actionLabel: string;
}

/** 快照哈希的展示短形（保留前 12 位，避免撑破表格）。 */
export function shortSnapshot(hash: string): string {
  if (!hash) return "—";
  return hash.length <= 12 ? hash : `${hash.slice(0, 12)}…`;
}

/**
 * 历史记录 → 展示行。
 *
 * ★ 「保持现状」记录不显示成「A → A」：那看起来像 bug。
 *   显示成「保持 A」并给出标记，用户才知道这是一次**有意的决策**。
 */
export function buildHistoryRows(h: readonly StrategyHistoryEntry[]): HistoryRowVM[] {
  return (h ?? []).map((e) => {
    const kept = e.fromOption === e.toOption;
    return {
      id: e.id,
      transition: kept ? `保持 ${e.fromOption}` : `${e.fromOption} → ${e.toOption}`,
      kept,
      decidedBy: e.decidedBy,
      decidedAt: e.decidedAt,
      snapshot: shortSnapshot(e.dataSnapshotHash),
      // ★ 不编造效果：未回填时明说「待评估」，而不是留空
      //   （留空会让用户以为数据丢了）。
      effect: e.actualEffect && e.actualEffect.length > 0 ? e.actualEffect : "待评估",
      actionLabel: kept ? "保持现状" : "变更",
    };
  });
}

/** 历史统计（用于卡片上的「已决策 N 次」。） */
export interface HistoryStats {
  total: number;
  changes: number;
  kept: number;
}

export function historyStats(h: readonly StrategyHistoryEntry[]): HistoryStats {
  let changes = 0;
  let kept = 0;
  for (const e of h ?? []) {
    if (e.fromOption === e.toOption) kept++;
    else changes++;
  }
  return { total: (h ?? []).length, changes, kept };
}

// ───────────────────────────── 采纳学习 ─────────────────────────────

/** 风险偏好（与后端 strategy.Preference 对应）。 */
export interface Preference {
  account: string;
  riskAppetite: number;
  samples: number;
  note: string;
}

/** 对候选选项按「偏好」重排展示顺序（**不改变** any 卡片自身顺序）。 */
export function orderByPreference(
  c: StrategyChoice,
  p: Preference | null,
): ChoiceOption[] {
  const opts = [...(c?.options ?? [])];
  if (!p || p.samples < 3) {
    // ★ 样本不足时**不重排**：基于 1–2 次决策的「学习」本质是过拟合，
    //   会让列表顺序莫名其妙地跳，用户会以为出了 bug。
    return opts;
  }
  const appetite = p.riskAppetite;
  const score = (o: ChoiceOption): number => {
    const r = o.risk === "low" ? -1 : o.risk === "high" ? 1 : 0;
    // 偏好越激进，越倾向把高风险排前；越保守，越倾向低风险
    return r * appetite;
  };
  // 稳定排序：先按推荐，再按偏好得分，最后按 key（保证确定）
  return opts.sort((a, b) => {
    const ar = a.recommended === true ? 1 : 0;
    const br = b.recommended === true ? 1 : 0;
    if (ar !== br) return br - ar;
    const sa = score(a);
    const sb = score(b);
    if (sa !== sb) return sb - sa;
    return a.key < b.key ? -1 : a.key > b.key ? 1 : 0;
  });
}

// ───────────────────────────── 闭环阶段 ─────────────────────────────

/** 自迭代闭环的五个阶段（docs/01 §7.2）。 */
export const LOOP_STAGES = ["observe", "propose", "decide", "apply", "evaluate"] as const;
export type LoopStage = (typeof LOOP_STAGES)[number];

/** 阶段展示元数据。 */
export const LOOP_STAGE_META: Record<LoopStage, { label: string; hint: string }> = {
  observe: { label: "观测", hint: "算法表现：命中率 / 覆盖度 / 采纳率 / 预测偏差" },
  propose: { label: "生成候选", hint: "基于历史数据自动生成 2–4 个方案" },
  decide: { label: "用户决策", hint: "选型：选 A / 选 B(…) / 保持现状" },
  apply: { label: "应用", hint: "更新规则集，标记受影响预计算桶 STALE" },
  evaluate: { label: "评估", hint: "A/B 对比实际表现，回填策略历史" },
};

/**
 * 依据「是否有待决策卡」推断当前处于闭环的哪个阶段。
 *
 * ★ 只用于给用户一个进度感，**不参与任何逻辑判断**：
 *   真正的阶段由后端决定（例如「已决定但桶未重算完」仍是 apply 阶段）。
 *   若前端据此驱动行为（如「decide 阶段才允许提交」），
 *   就会出现「后端说还在 apply、前端说不让点」的死锁。
 */
export function currentStage(
  choices: readonly StrategyChoice[],
  hasPendingDecisions: boolean,
): LoopStage {
  if (hasPendingDecisions) return "decide";
  return (choices?.length ?? 0) > 0 ? "observe" : "propose";
}
