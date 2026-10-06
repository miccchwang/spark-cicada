/**
 * src/pnl/statement.ts —— M-PNL 损益表分层与口径切换
 *
 * 职责（docs/02 M-PNL、docs/03 §4.3）：把「一坨已算好的输入」组织成
 * 某个口径下的分层损益表，并把口径切换做成**纯展示侧归属调整**。
 *
 * ══════════════════════════════════════════════════════════════════════════
 * ★★ 边界纪律（由 scripts/layering-gate.mjs 静态强制）：
 *
 *   1. **本模块不实现业务公式。** 「rev − cogs」「gp / rev」这些一律由
 *      compute 内核（Rust）产出；本模块只做**组织与归属**：
 *      把内核给的数字放到哪一行、折扣算收入侧还是费用侧。
 *      ★ 边界很细但很重要：把折扣「从营销费里扣除、加到收入抵减上」是
 *        **记账归属**，不是重算 —— 两个数字都来自内核。
 *
 *   2. **不发起网络请求。** 取数由装配层经 M-QUERY 完成。
 *
 *   3. **不引用 M-RENDER 的 VM。** 本模块产出纯数据（PnlLine[]），
 *      渲染由装配层决定。
 * ══════════════════════════════════════════════════════════════════════════
 *
 * ★ 三条纪律（都对应真实会踩的坑）：
 *
 *   1. **口径切换只动「折扣」这一件事，绝不动净收入。**
 *      KODP 实测：口径 A 下 8 月净贡献 −152,567.19、口径 B 下 +123.17。
 *      同一份数据，两个结论，符号都反了 —— 这就是为什么默认口径必须
 *      按模块分别定（D10），且切换时必须给用户**风险提示**。
 *
 *   2. **任何一行为 null 都必须保持 null，绝不补 0。**
 *      口径 B 会把折扣搬走后「营销费用」可能变得与 A 不同 ——
 *      若此时用 0 兜底，用户会看到「营销费用 0」这种荒谬结论。
 *
 *   3. **口径必须显式、可回溯、可审计。** 非法口径值一律**报错而非回退**，
 *      因为「静默按 A 展示」会让财务同事拿着 A 的数字去开会。
 */

import {
  CALIBER_INVARIANT_LINES,
  CALIBER_META,
  DEFAULT_CALIBER_BY_MODULE,
  DISCOUNT_TARGET_LINE,
  FALLBACK_CALIBER,
  PNL_CALIBERS,
  PNL_CONTRACT_VERSION,
  PNL_LINES,
  type CaliberMeta,
  type PnlCaliber,
  type PnlLine,
  type PnlLineId,
  type PnlStatement,
} from "../contracts/pnl.js";

/** 口径合法性判别（类型守卫）。 */
export function isCaliber(v: unknown): v is PnlCaliber {
  return v === "A" || v === "B";
}

/** 非法口径异常。★ 刻意抛错而不是回退 —— 见文件头纪律 3。 */
export class CaliberError extends Error {
  readonly received: unknown;
  constructor(received: unknown) {
    super(`M-PNL: 非法口径 ${JSON.stringify(received)}（合法值 ${PNL_CALIBERS.join(" / ")}）`);
    this.name = "CaliberError";
    this.received = received;
  }
}

/**
 * 解析口径：合法即返回，非法抛 CaliberError。
 *
 * 若确实需要「尽力而为」的降级，用 resolveCaliber（它会记录降级原因）。
 */
export function parseCaliber(v: unknown): PnlCaliber {
  if (isCaliber(v)) return v;
  throw new CaliberError(v);
}

/** 解析结果（带降级原因，便于 UI 提示「为什么显示的是 B」）。 */
export interface ResolvedCaliber {
  caliber: PnlCaliber;
  /** 是否为降级（请求的口径非法或缺失） */
  downgraded: boolean;
  /** 降级原因（downgraded=true 时必有） */
  reason?: string;
  /** 该口径的来源：请求 / 模块默认 / 兜底 */
  source: "requested" | "module_default" | "fallback";
}

/**
 * 宽松解析：非法/缺失 → 按模块默认（D10）→ 再兜底。
 *
 * ★ 与 parseCaliber 的分工：
 *   * 用户**主动切**口径时用 parseCaliber（非法必须报错，不能默默换）；
 *   * 页面**首次加载**时用 resolveCaliber（URL 可能没有 / 可能被手改成脏值，
 *     这时应该用模块默认并**明确告诉用户**，而不是白屏或报错）。
 */
export function resolveCaliber(requested: unknown, module: string): ResolvedCaliber {
  if (isCaliber(requested)) {
    return { caliber: requested, downgraded: false, source: "requested" };
  }
  const def = DEFAULT_CALIBER_BY_MODULE[module];
  if (def) {
    return {
      caliber: def,
      downgraded: requested !== undefined && requested !== null && requested !== "",
      reason: requested === undefined || requested === null || requested === ""
        ? `未指定口径，按模块「${module}」的默认口径（${def}，D10 分模块各自默认）`
        : `口径参数非法（${JSON.stringify(requested)}），已回退到模块「${module}」的默认口径（${def}）`,
      source: "module_default",
    };
  }
  return {
    caliber: FALLBACK_CALIBER,
    downgraded: true,
    reason: `模块「${module}」未登记默认口径，已回退到保守口径（${FALLBACK_CALIBER}）`,
    source: "fallback",
  };
}

/** 取口径元数据（供 UI 展示定义与误读风险）。 */
export function caliberMeta(c: PnlCaliber): CaliberMeta {
  return CALIBER_META[c];
}

/** 该口径下折扣计入哪一行。 */
export function discountTargetLine(c: PnlCaliber): PnlLineId {
  return DISCOUNT_TARGET_LINE[c];
}

// ───────────────────────────── 输入 ─────────────────────────────

/**
 * 损益表的原始输入 —— **全部由 compute 内核产出**，本模块不重算。
 *
 * ★ 关键设计：折扣（sellerDiscount）是**独立一项**传进来的，
 *   而不是「已经算进营销费用」或「已经算进收入抵减」。
 *   若输入就已经选定了一边，那口径切换就成了「解释性文字」，
 *   而不是真正的归属调整 —— 用户切了也看不到任何变化，会以为功能坏了。
 */
export interface PnlInputs {
  /** 挂牌总额（GMV） */
  grossListing: number | null;
  /** 卖家折扣（正数表示折扣金额）。★ 口径差异的唯一来源 */
  sellerDiscount: number | null;
  /** 平台折扣/补贴 */
  platformDiscount: number | null;
  /** 净收入（★ 由内核按 algo.net_revenue 算出，本模块**从不**自算它） */
  netRevenue: number | null;
  /** 商品成本 */
  cogs: number | null;
  /** 毛利 */
  gp: number | null;
  /** 毛利率 */
  gmp: number | null;
  /**
   * 营销费用（**不含**卖家折扣）。
   * 口径 A 下展示为 marketing + sellerDiscount；口径 B 下保持原值。
   */
  marketing: number | null;
  /** 贡献毛利 1（内核按 gp − marketing 算，此处 marketing 为内核口径） */
  cm1: number | null;
  /** 平台费用 */
  platformFee: number | null;
  /** 贡献毛利 2 */
  cm2: number | null;
  /** 摊分管理费 */
  overheadAlloc: number | null;
  /** 净贡献（★ 内核按 cm2 − overhead_alloc 算） */
  netContrib: number | null;
}

/** 输入里可选的「缺失原因 + 数据槽」元信息（缺失时必须给出，便于渲染「待接入」的理由）。 */
export type LineMeta = Partial<Record<PnlLineId, { dataSlots?: string[]; gapReason?: string }>>;

// ───────────────────────────── 组装 ─────────────────────────────

/** 取某个 line 的原始输入值。 */
function rawValue(input: PnlInputs, id: PnlLineId): number | null {
  switch (id) {
    case "gross_listing":
      return input.grossListing;
    case "seller_discount":
      return input.sellerDiscount;
    case "platform_discount":
      return input.platformDiscount;
    case "net_revenue":
      return input.netRevenue;
    case "cogs":
      return input.cogs;
    case "gp":
      return input.gp;
    case "gmp":
      return input.gmp;
    case "marketing":
      return input.marketing;
    case "cm1":
      return input.cm1;
    case "platform_fee":
      return input.platformFee;
    case "cm2":
      return input.cm2;
    case "overhead_alloc":
      return input.overheadAlloc;
    case "net_contrib":
      return input.netContrib;
  }
}

/**
 * 口径 A 下营销费用的**展示值** = 内核给的 marketing + 卖家折扣。
 *
 * ★ 这是「归属搬运」而不是「重算」：两个加数都来自内核，
 *   本模块只是把它们放到同一行里展示（A 口径的定义就是「折扣计入营销费用」）。
 *   若折扣缺失 ⇒ 结果缺失（不把折扣当 0 加）——否则 A 口径会虚低。
 */
function marketingShownInA(input: PnlInputs): number | null {
  if (input.marketing === null || input.sellerDiscount === null) return null;
  return input.marketing + input.sellerDiscount;
}

/**
 * 组装某口径下的损益表。
 *
 * ★ 口径 B 下 `seller_discount` 行**照常显示**（它就是 contra-revenue 的
 *   抵减项，本来就该在收入段）；口径 A 下它也显示，只是**归到费用侧**去理解。
 *   两口径下这一行的**数值**其实是同一个数（都是内核给的 sellerDiscount）——
 *   差异在「它去哪、怎么读」，不在数值本身。这正是 docs/03 说
 *   「口径切换会完全改变经营解读」的机制。
 */
export function statementFor(
  caliber: PnlCaliber,
  input: PnlInputs,
  opts: { module?: string; meta?: LineMeta; generatedAt?: string } = {},
): PnlStatement {
  const meta = opts.meta ?? {};
  const lines: PnlLine[] = PNL_LINES.map((def) => {
    let value = rawValue(input, def.id);

    // ── 唯一的归属调整：口径 A 把折扣并入营销费用 ──
    if (def.id === "marketing" && caliber === "A") {
      value = marketingShownInA(input);
    }

    const line: PnlLine = {
      id: def.id,
      label: def.label,
      section: def.section,
      kind: def.kind,
      value,
      algoId: def.algoId,
      dataSlots: meta[def.id]?.dataSlots ?? [],
    };
    if (value === null) {
      // 缺失必须有理由（G3：显示「待接入」而非 0）
      line.gapReason = meta[def.id]?.gapReason ?? "依赖数据不足，已跳过（fail-closed）";
    }
    return line;
  });

  return {
    v: PNL_CONTRACT_VERSION,
    caliber,
    module: opts.module ?? "module.pnl",
    lines,
    hasGaps: lines.some((l) => l.value === null),
    generatedAt: opts.generatedAt ?? new Date().toISOString(),
  };
}

/** 按 id 取一行（找不到返回 undefined，不抛 —— 调用方自行决定）。 */
export function lineOf(stmt: PnlStatement, id: PnlLineId): PnlLine | undefined {
  return stmt.lines.find((l) => l.id === id);
}

/** 取某行的数值（缺失 → null）。 */
export function valueOf(stmt: PnlStatement, id: PnlLineId): number | null {
  return lineOf(stmt, id)?.value ?? null;
}

// ───────────────────────────── ★ 不变量 ─────────────────────────────

/** 不变量检查结果。 */
export interface InvariantResult {
  ok: boolean;
  /** 违规描述（ok=true 时为空数组） */
  violations: string[];
}

/** 判定两个金额是否「相等」（含 null 语义：都 null 视为相等）。 */
function sameAmount(a: number | null, b: number | null, eps = 1e-6): boolean {
  if (a === null && b === null) return true;
  if (a === null || b === null) return false;
  return Math.abs(a - b) <= eps;
}

/**
 * ★★★ 校验「净收入两口径相同」——本模块存在的最重要理由。
 *
 * 若此断言失败，**绝不允许**继续展示：
 *   要么是输入构造有误（折扣被算进了净收入），要么是内核公式回归。
 *   两种都是 bug。**不能**把它当成「新口径的差异」轻轻放过 ——
 *   那样财务会拿到两套互不相容的净收入，进而失去对整张表的信任。
 *
 * @param a 口径 A 的报表
 * @param b 口径 B 的报表
 */
export function assertNetRevenueInvariant(a: PnlStatement, b: PnlStatement): InvariantResult {
  const violations: string[] = [];

  if (a.caliber !== "A" || b.caliber !== "B") {
    violations.push(`不变量比较要求 (A, B) 两张表，实得 (${a.caliber}, ${b.caliber})`);
  }

  for (const id of CALIBER_INVARIANT_LINES) {
    const va = valueOf(a, id);
    const vb = valueOf(b, id);
    if (!sameAmount(va, vb)) {
      violations.push(
        `★ 口径不变量被破坏：${id} 在 A=${fmt(va)}、B=${fmt(vb)} —— ` +
          `该行在两口径下必须完全相同（折扣本就从净收入扣除，口径只改展示归属）`,
      );
    }
  }

  return { ok: violations.length === 0, violations };
}

/** 两条口径下的完整对照（供「切换前预览影响」用）。 */
export interface CaliberComparison {
  A: PnlStatement;
  B: PnlStatement;
  /** 哪些行在两口径下数值不同（即口径差异的实际落点） */
  differingLines: PnlLineId[];
  /** 不变量校验结果 */
  invariant: InvariantResult;
}

/**
 * 生成两口径对照。
 *
 * ★ 「哪些行会变」是用户切口径前最想知道的事 —— 直接算出来给他，
 *   比让他自己来回切换目测要可靠得多（人眼对两列数字的差异不敏感）。
 */
export function compareCalibers(
  input: PnlInputs,
  opts: { module?: string; meta?: LineMeta; generatedAt?: string } = {},
): CaliberComparison {
  const a = statementFor("A", input, opts);
  const b = statementFor("B", input, opts);
  const differing: PnlLineId[] = [];
  for (const def of PNL_LINES) {
    if (!sameAmount(valueOf(a, def.id), valueOf(b, def.id))) differing.push(def.id);
  }
  return { A: a, B: b, differingLines: differing, invariant: assertNetRevenueInvariant(a, b) };
}

/** 数字展示（诊断消息用；不参与报表渲染）。 */
function fmt(v: number | null): string {
  return v === null ? "null" : String(v);
}

// ───────────────────────────── 用户提示 ─────────────────────────────

/**
 * 口径切换提示（供切换器旁的大字提示）。
 *
 * ★ 必须主动给出「这个口径容易被误读成什么」——
 *   KODP 的教训就是：同一份数据，A 口径显示巨亏、B 口径显示微利，
 *   而两者都没错。不提示用户，就会有人拿着符号相反的数字去决策。
 */
export function caliberWarning(c: PnlCaliber): string {
  const m = CALIBER_META[c];
  return `${m.label}：${m.definition}。注意：${m.misreadingRisk}`;
}

/**
 * 口径差异摘要（切换后展示「变了什么、没变什么」）。
 *
 * ★ 刻意把「没变的」也说清楚：用户最需要的安慰是
 *   「净收入没变，只是呈现位置变了」——否则会怀疑数据被篡改。
 */
export function caliberDiffSummary(cmp: CaliberComparison): string {
  const changed = cmp.differingLines
    .map((id) => PNL_LINES.find((d) => d.id === id)?.label ?? id)
    .join("、");
  if (cmp.differingLines.length === 0) {
    return "两口径下所有行数值相同（当前数据未涉及折扣，口径切换无可见差异）";
  }
  return `口径切换改变了：${changed}；净收入与挂牌总额**保持不变**（口径只改展示归属，不改数值口径）`;
}
