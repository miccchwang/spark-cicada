/**
 * contracts/strategy-choice.ts —— v1.0
 *
 * 策略实验室（M-STRATEGY）的选型式决策契约。
 *
 * 纪律：决策项**一律以选项呈现**，用户只有「选 A / 选 B(…) / 保持现状」三种动作，
 *       绝不提供让用户填写公式或任意数字的输入框。
 */

export const STRATEGY_CHOICE_VERSION = "1.0" as const;

export interface StrategyChoice {
  id: string;
  /** 决策标题，如「渠道费率口径」 */
  title: string;
  /** 为什么要决策（背景说明） */
  context: string;
  /** 当前生效选项 */
  current: ChoiceOption;
  /** 候选选项 2–4 个（含 current） */
  options: ChoiceOption[];
  /** 每个选项的预期影响预览 */
  impactPreview: ImpactPreview;
  /** 是否阻塞其他模块落地 */
  blocking: boolean;
  /** 建议决策截止时间（ISO） */
  decideBy?: string;
}

export interface ChoiceOption {
  key: string;
  label: string;
  description: string;
  /** 预期影响，如「净利率 34.45% → 36.14%」 */
  expectedEffect: string;
  risk: "low" | "medium" | "high";
  reversible: boolean;
  recommended?: boolean;
}

export interface ImpactPreview {
  /** 受影响的指标列表 */
  metrics: string[];
  /** 每个指标在各选项下的变化（optionKey → metricKey → 变化描述） */
  delta: Record<string, Record<string, string>>;
  /** 受影响的预计算桶（选定后需重算） */
  affectedBuckets: string[];
}

/** 用户决策动作（只有这三种） */
export type DecisionAction =
  | { kind: "choose"; optionKey: string }
  | { kind: "keep_current" };

/** 策略历史记录（append-only） */
export interface StrategyHistoryEntry {
  id: string;
  choiceId: string;
  fromOption: string;
  toOption: string;
  decidedBy: string;
  decidedAt: string;
  /** 决策当时的数据快照哈希，保证可解释 */
  dataSnapshotHash: string;
  /** 事后评估的实际效果（可空，异步回填） */
  actualEffect?: string;
}
