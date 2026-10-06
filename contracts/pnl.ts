/**
 * contracts/pnl.ts —— P&L 契约 v1.0
 *
 * 唯一真源。`web/src/contracts/pnl.ts` 是构建期镜像，二者必须逐字一致
 * （由 web/test/contracts.test.mjs 断言，防止漂移）。
 *
 * ══════════════════════════════════════════════════════════════════════════
 * ★★ 本契约的核心是「口径」（caliber）——docs/03 §4.3 承袭 KODP 实测结论。
 *
 *   口径 A（参考口径）：Seller Discount 计入**营销费用**。
 *       → 营销费用虚高，容易被误读为「费用失控」。
 *   口径 B（Finance 口径）：Seller Discount 作 **contra-revenue（收入抵减）**。
 *       → 揭示真实问题是「折扣压缩毛利」。
 *
 * ★★★ 最关键的一条不变量（本文件的纪律核心）：
 *
 *     **净收入（net_revenue）在两口径下完全相同。**
 *
 *   原因（不是巧合，是定义决定的，见 docs/03 §3.2）：
 *     algo.net_revenue = gross_listing − seller_disc − platform_disc
 *   折扣**本来就从 net_revenue 里扣掉了**。两口径的区别只是把
 *   「折扣」这件事**在报表上的呈现位置**从费用侧搬到收入侧 ——
 *   它是**展示侧归属**问题，不是计算问题。
 *
 *   因此：口径切换**不得**改变 net_revenue。若某天变了，那不是「口径差异」，
 *   而是有人改错了一处公式 —— 必须当成 bug 而不是当成新口径。
 *   （web/src/pnl/statement.ts 的 assertNetRevenueInvariant 就是钉这条。）
 * ══════════════════════════════════════════════════════════════════════════
 */

export const PNL_CONTRACT_VERSION = "1.0" as const;

/** 折扣口径。 */
export type PnlCaliber = "A" | "B";

/** 全部合法口径（有序：A 在前，与文档一致）。 */
export const PNL_CALIBERS: readonly PnlCaliber[] = ["A", "B"];

/** 口径元数据（供 UI 切换器与说明文案使用）。 */
export interface CaliberMeta {
  id: PnlCaliber;
  /** 短名 */
  short: string;
  /** 全名 */
  label: string;
  /** 一句话定义（切换器上直接展示，避免用户凭名字猜） */
  definition: string;
  /**
   * 该口径下最容易被误读的点 —— 必须**主动提示**用户，
   * 否则用户会拿 A 的结论去指导经营（KODP 实测正是这么翻车的）。
   */
  misreadingRisk: string;
  /**
   * Seller Discount 在该口径下的归属。
   * 这是两口径唯一的差异点（结构性，不是数值性的）。
   */
  sellerDiscountRole: "marketing_expense" | "contra_revenue";
}

/** 口径元数据表。 */
export const CALIBER_META: Readonly<Record<PnlCaliber, CaliberMeta>> = Object.freeze({
  A: {
    id: "A",
    short: "口径 A",
    label: "A · 参考口径",
    definition: "Seller Discount 计入营销费用",
    misreadingRisk: "营销费用会虚高，容易被误读为「费用失控」——实际问题可能是折扣过深",
    sellerDiscountRole: "marketing_expense",
  },
  B: {
    id: "B",
    short: "口径 B",
    label: "B · Finance 口径",
    definition: "Seller Discount 作 contra-revenue（收入抵减）",
    misreadingRisk: "收入侧被压低，容易被误读为「卖不动」——实际问题可能是毛利率被折扣压缩",
    sellerDiscountRole: "contra_revenue",
  },
});

/**
 * P&L 分层（docs/02 M-PNL：收入→COGS→GP→费用→CM1→CM2→净贡献）。
 *
 * 顺序即展示顺序；`section` 用于分组高亮（收入段 / 毛利段 / 贡献段）。
 */
export type PnlLineId =
  | "gross_listing" // 挂牌总额（GMV）
  | "seller_discount" // 卖家折扣（口径差异点）
  | "platform_discount" // 平台补贴/折扣
  | "net_revenue" // ★ 净收入（两口径恒等）
  | "cogs" // 商品成本
  | "gp" // 毛利
  | "gmp" // 毛利率
  | "marketing" // 营销费用（含广告/达人/样品；口径 A 下再加折扣）
  | "cm1" // 贡献毛利 1 = gp − marketing
  | "platform_fee" // 平台费用（按规则集逐项计提）
  | "cm2" // 贡献毛利 2 = cm1 − platform_fee
  | "overhead_alloc" // 摊分管理费
  | "net_contrib"; // ★ 净贡献 = cm2 − overhead_alloc

/** 分层归属分组。 */
export type PnlSection = "revenue" | "gross" | "contribution";

/** 一行的静态定义。 */
export interface PnlLineDef {
  id: PnlLineId;
  label: string;
  section: PnlSection;
  /** 金额 / 比率 / 数量 */
  kind: "currency" | "percent";
  /** 是否参与「加总」（金额类）或仅为派生比率 */
  isRatio: boolean;
  /**
   * 该行在**哪个口径下**才有值 / 才有意义。
   * `both` = 两口径都有且数值必须相同（net_revenue 就是它）。
   */
  caliber: "both" | "A" | "B";
  /** 来源算法 ID（docs/03 §5：每个金额都必须可回溯算法与数据槽） */
  algoId: string;
}

/** 分层定义表（顺序即展示顺序 —— 不要重排，报表阅读顺序是契约的一部分）。 */
export const PNL_LINES: readonly PnlLineDef[] = [
  { id: "gross_listing", label: "挂牌总额 (GMV)", section: "revenue", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.rev" },
  { id: "seller_discount", label: "卖家折扣", section: "revenue", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.net_revenue" },
  { id: "platform_discount", label: "平台折扣/补贴", section: "revenue", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.net_revenue" },
  // ★ 两口径恒等的一行 —— 契约里用 caliber:"both" 明示
  { id: "net_revenue", label: "净收入", section: "revenue", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.net_revenue" },
  { id: "cogs", label: "商品成本 (COGS)", section: "gross", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.cogs" },
  { id: "gp", label: "毛利 (GP)", section: "gross", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.gp" },
  { id: "gmp", label: "毛利率", section: "gross", kind: "percent", isRatio: true, caliber: "both", algoId: "algo.gmp" },
  { id: "marketing", label: "营销费用", section: "contribution", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.marketing" },
  { id: "cm1", label: "贡献毛利 1 (CM1)", section: "contribution", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.cm1" },
  { id: "platform_fee", label: "平台费用", section: "contribution", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.platform_fee" },
  { id: "cm2", label: "贡献毛利 2 (CM2)", section: "contribution", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.cm2" },
  { id: "overhead_alloc", label: "摊分管理费", section: "contribution", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.net_contrib" },
  { id: "net_contrib", label: "净贡献", section: "contribution", kind: "currency", isRatio: false, caliber: "both", algoId: "algo.net_contrib" },
];

/**
 * ★ 两口径下 net_revenue **必须相等** 的行 id。
 *
 * 单独导出成一个常量（而不是让调用方自己记得），是为了让这个不变量
 * 有一个**单一可引用的名字** —— 测试、后端断言、前端自检都指向它，
 * 谁改坏了都指向同一处契约。
 */
export const CALIBER_INVARIANT_LINES: readonly PnlLineId[] = ["net_revenue", "gross_listing"];

/**
 * 口径 → 该口径下「折扣」计入的报表行。
 *
 * ★ 这是两口径**唯一**的结构性差异。把它显式写成数据（而不是散在 if 里），
 *   是为了让「折扣搬到哪一行」这件事可被断言、可被审计。
 */
export const DISCOUNT_TARGET_LINE: Readonly<Record<PnlCaliber, PnlLineId>> = Object.freeze({
  A: "marketing", // 计入营销费用
  B: "seller_discount", // 作收入抵减
});

/**
 * 模块 → 默认口径（决策项 D10：「分模块各自默认」）。
 *
 * 经营报表要跟运营对话（GMV 为主）→ 运营视角；P&L 要跟财务对话 → 财务口径。
 * 给的是**模块名**而不是「全局默认」：D10 明确否决了「全局一个默认口径」，
 * 因为两个模块的读者根本不是同一批人。
 */
export const DEFAULT_CALIBER_BY_MODULE: Readonly<Record<string, PnlCaliber>> = Object.freeze({
  "module.report": "A", // 经营报表 = 运营口径
  "module.pnl": "B", // P&L = 财务口径（Finance）
});

/** 兜底默认口径（模块未登记时）。取 B：财务口径更保守，不会让费用看起来失控。 */
export const FALLBACK_CALIBER: PnlCaliber = "B";

/** P&L 单元（一行一值）。 */
export interface PnlLine {
  id: PnlLineId;
  label: string;
  section: PnlSection;
  kind: "currency" | "percent";
  /** 金额/比率；null = 依赖缺失被跳过（渲染「待接入」），**绝不用 0 占位** */
  value: number | null;
  algoId: string;
  dataSlots: string[];
  /** 缺失原因（value 为 null 时必填） */
  gapReason?: string;
}

/** 一张 P&L 报表（某个口径下的完整分层）。 */
export interface PnlStatement {
  v: typeof PNL_CONTRACT_VERSION;
  caliber: PnlCaliber;
  /** 模块（决定默认口径的来源，便于排障「为什么这里是 B」） */
  module: string;
  lines: PnlLine[];
  /** 是否有任一行因缺失被跳过 */
  hasGaps: boolean;
  generatedAt: string;
}
