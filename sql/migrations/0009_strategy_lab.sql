-- 0009_strategy_lab.sql —— M-STRATEGY 策略实验室（自迭代闭环 + 选型式决策）
--
-- ══════════════════════════════════════════════════════════════════════════
-- ★★ 本迁移的纪律与 0008 完全一致：**增量补强既有表，绝不另起一套**。
--
--   策略实验室需要三样东西，逐一说明它们住在哪张既有表里：
--
--   ① **待决策的选型卡**（Propose 阶段的产物）
--      → 住 registry_rule_set。一张卡本质就是「一条带版本、带 scope、
--        带 items 的规则候选集」：items 里放 2–4 个候选选项。
--        再建 strategy_choice 表就是「两个真源」反模式 ——
--        候选集既能从规则集算出来、又能在新表里被改，两者必然漂移，
--        而漂移的后果是**用户看到的选项与实际生效的参数不是一回事**。
--
--   ② **决策历史**（append-only，docs/01 §7.5 的七要素）
--      → 住 audit_log。audit_log 已有 UPDATE/DELETE 拦截触发器（0001），
--        正是「任何数值变化都能解释」所需的不可篡改性。
--        单独建 strategy_history 表反而要自己再写一遍 append-only 触发器，
--        且会分裂成「审计表 + 业务历史表」两份记录，对不上时无人能判谁对。
--
--   ③ **当前生效值**
--      → 就是规则集 items 里的 is_current=true 那一项（见下方种子）。
--        用数据字段而不是加一列：加列会让「同一规则集的多个版本」
--        出现多个 current 列值，无法表达「历史版本里哪个当值是值」。
--
-- ★ 与 0008 的另一个共同点：**不变量写成数据**。
--   策略实验室最容易出的错是「改了参数但预计算桶没重算」——
--   报表会用旧参数的数字回答新口径的问题，且**不会报错**。
--   因此把「变更必须标记受影响桶」这条纪律也登记进库（rule.strategy.apply），
--   让它有可被 SQL 查询的权威出处。
-- ══════════════════════════════════════════════════════════════════════════
--
-- 对应文档：
--   docs/01 §7.2 自迭代闭环（Observe→Propose→Decide→Apply→Evaluate）
--   docs/01 §7.3 选型式交互契约（contracts/strategy-choice.ts v1.0）
--   docs/01 §7.4 内置能力（参数调优/口径对照/异常自检/采纳学习/回滚）
--   docs/01 §7.5 策略历史七要素
--   决策 D6：策略实验室范围 = 仅管理层（T1–T2）
--
-- 代码侧实现：
--   contracts/strategy-choice.ts            契约真源
--   web/src/strategy/lab.ts                 前端（选型卡 + 三动作 + 回滚）
--   backend/internal/strategy/strategy.go   纯逻辑（候选集守卫/回滚/采纳学习）
--   backend/internal/api/strategy.go        HTTP 层（D6 守卫 + 审计）
--   backend/internal/store/strategy.go      持久化

BEGIN;

-- ───────────────────────────── ① 选型卡（增量：只插入数据行） ─────────────────────────────
--
-- 形状与 0002 的 registry_rule_set 一致，不新增列。
--
-- items 的形状对齐 contracts/strategy-choice.ts：
--   { key, label, description, expected_effect, risk, reversible, recommended? }
-- current_key 指向 items 里的一项 —— 与 Go 侧 strategy.Choice.CurrentKey 同义。
--
-- ★ 为什么 current_key 放在 scope 里而不是 items 里：
--   items 在 0002 的语义是「这条规则集有哪些可选项」，是**集合**；
--   而「当前生效的是哪一个」是这条规则集的**状态**。混进 items
--   会让「遍历候选项」的代码必须跳过一项特殊元素 —— 那类代码
--   最容易在后续迭代里被漏改。
--
-- ★ 为什么 title / context / affected_buckets 也放在 scope 里：
--   它们是**卡的元数据**（这张卡在决策什么、为什么、影响哪些桶），
--   与 items 的「候选项集合」是不同维度。scope 在 0002 的定义就是
--   「这条规则集的适用范围」，扩成「规则集的元数据」是语义内的延伸，
--   不新增列。context 尤其不可省 —— 缺了它 Validate 会直接拒绝这张卡
--   （见 backend/internal/strategy/strategy.go 的 Validate）。
INSERT INTO registry_rule_set (id, version, scope, items)
VALUES (
    'strategy.choice.fee_caliber',
    1,
    jsonb_build_object(
        'global', true,
        'current_key', 'A',
        'module', 'module.report',
        'blocking', true,
        'title', '渠道费率口径',
        'context', 'Shopee 费率采集暂缓（F2），当前用估算值参与净贡献计算。经营报表与 P&L 两个模块的口径不一致，需要决定是否统一。',
        'metrics', jsonb_build_array('net_rate', 'gp', 'net_contrib'),
        'affected_buckets', jsonb_build_array('pnl_month')
    ),
    '[
      {
        "key": "A",
        "label": "A · 沿用运营口径",
        "description": "Seller Discount 计入营销费用，与经营报表现行口径一致",
        "expected_effect": "净利率 34.45% 保持不变",
        "risk": "low",
        "reversible": true
      },
      {
        "key": "B",
        "label": "B · 切换财务口径",
        "description": "Seller Discount 作 contra-revenue，与 P&L 口径对齐",
        "expected_effect": "净利率 34.45% → 36.14%（毛利口径上移）",
        "risk": "medium",
        "reversible": true
      }
    ]'::jsonb
)
ON CONFLICT (id, version) DO UPDATE SET
    scope = EXCLUDED.scope,
    items = EXCLUDED.items,
    updated_at = now();

-- 第二张卡：演示「不可回滚」的选项，用于验证 strategy.LastStable 的守卫。
--
-- ★ 这张卡**故意**包含一个 reversible=false 的选项：
--   口径切换会触发历史数据重算，重算后无法用改参数的方式回到旧值
--   （旧值的前提数据已不存在）。这不是缺陷，是必须被如实告知的事实 ——
--   若系统假装它可回滚，用户点了回滚会得到一个「参数回去了、
--   数据没回去」的错位状态，比不许回滚危险得多。
INSERT INTO registry_rule_set (id, version, scope, items)
VALUES (
    'strategy.choice.rate_threshold',
    1,
    jsonb_build_object(
        'global', true,
        'current_key', 'balanced',
        'module', 'module.report',
        'blocking', false,
        'title', '覆盖率门限',
        'context', '近 30 天异常自检报告数据分布漂移（渠道 A 覆盖率低于门限 3 个标准差）。需要决定是否调整门限，或按新口径全量重算。',
        'metrics', jsonb_build_array('coverage', 'anomaly_count'),
        'affected_buckets', jsonb_build_array('pnl_month', 'precomp_coverage')
    ),
    '[
      {
        "key": "conservative",
        "label": "保守 · 阈值收紧",
        "description": "覆盖率门限提高到 98%，异常卡更少但可能漏报",
        "expected_effect": "异常卡 −42%",
        "risk": "low",
        "reversible": true
      },
      {
        "key": "balanced",
        "label": "均衡 · 维持现状",
        "description": "覆盖率门限 95%，当前生效",
        "expected_effect": "保持不变",
        "risk": "low",
        "reversible": true
      },
      {
        "key": "recompute",
        "label": "激进 · 全量重算口径",
        "description": "按新口径重算历史桶（覆盖 180 天）",
        "expected_effect": "覆盖率 95% → 99%，但历史数据按新口径覆写",
        "risk": "high",
        "reversible": false
      }
    ]'::jsonb
)
ON CONFLICT (id, version) DO UPDATE SET
    scope = EXCLUDED.scope,
    items = EXCLUDED.items,
    updated_at = now();

-- ───────────────────────────── ② 应用纪律（不变量写成数据） ─────────────────────────────
--
-- ★ 这条纪律必须有权威出处：策略变更改的是**参数**，而报表读的是
--   **预计算桶**。两者不同步时不会报错，只会静默给出错数。
--   把它登记进库，审计脚本与 CI 就能指向同一行，而不是各自在注释里写一份。
INSERT INTO registry_rule_set (id, version, scope, items)
VALUES (
    'rule.strategy.apply',
    1,
    '{"global":true}'::jsonb,
    '[
      {
        "id": "mark_affected_buckets_stale",
        "name": "变更必须标记受影响预计算桶为 STALE",
        "on": ["strategy.decide", "strategy.rollback"],
        "exemption": "keep_current（未实际改动，不触发重算）",
        "reason": "只改参数不重算 ⇒ 报表用旧参数回答已按新参数算好的桶，静默给出错数。",
        "on_violation": "reject",
        "ref": "backend/internal/strategy/strategy.go#Choice.IsChange"
      },
      {
        "id": "history_append_only",
        "name": "策略历史只增不改",
        "reason": "回滚不是删记录而是再记一条 —— 抹掉「曾经激进过又退回来」这段事实，等于丢掉最宝贵的信号。",
        "on_violation": "reject",
        "ref": "backend/internal/strategy/strategy.go 包注释第三纪律"
      },
      {
        "id": "no_free_form_input",
        "name": "决策只接受选型，不接受自由输入",
        "allowed_actions": ["choose", "keep_current"],
        "reason": "开放填公式入口会让 impactPreview 失效（无法预知影响面），并可能写出让毛利归零的表达式。",
        "on_violation": "reject",
        "ref": "contracts/strategy-choice.ts DecisionAction"
      }
    ]'::jsonb
)
ON CONFLICT (id, version) DO UPDATE SET
    scope = EXCLUDED.scope,
    items = EXCLUDED.items,
    updated_at = now();

-- ───────────────────────────── ③ 决策历史视图（复用 audit_log，不另建表） ─────────────────────────────
--
-- ★ 复用 0001 的 audit_log：它已有 UPDATE/DELETE 拦截触发器，
--   天然满足「策略历史 append-only」的要求，无需重写一遍触发器。
--   决策由 backend/internal/api/strategy.go 写
--   action ∈ {'strategy.decide', 'strategy.rollback'}。
--
-- 列与 docs/01 §7.5 的七要素一一对应：
--   策略 ID / 旧值 / 新值 / 决策人 / 时间 / 决策依据(快照哈希) / 实际效果
--
-- ★ actual_effect 目前恒为 NULL：它是**事后回填**的（Evaluate 阶段 A/B 对比），
--   而回填走的是「再插一条评估记录」而不是 UPDATE 历史行 ——
--   因为历史行不可改。这样「当时为什么这么决策」与
--   「事后看效果如何」是两条独立、都不可篡改的事实。
CREATE OR REPLACE VIEW v_strategy_history AS
SELECT
    audit_log.id                                          AS audit_id,
    audit_log.at                                          AS decided_at,
    audit_log.actor                                       AS decided_by,
    audit_log.action                                      AS action,
    COALESCE(audit_log.detail->>'choiceId', audit_log.target) AS choice_id,
    audit_log.detail->>'from'                             AS from_option,
    audit_log.detail->>'to'                               AS to_option,
    audit_log.detail->>'kind'                             AS kind,
    -- 「是否实质变更」由记录自身携带，不从 from/to 推断 ——
    -- 否则「恰好改回原值」会被误判为 keep_current。
    COALESCE((audit_log.detail->>'changed')::boolean, false) AS changed,
    audit_log.detail->>'snapshotHash'                     AS data_snapshot_hash,
    audit_log.detail->'affectedBuckets'                   AS affected_buckets,
    -- 事后评估效果由独立的 'strategy.evaluate' 记录承载（见下）
    NULL::text                                            AS actual_effect,
    audit_log.region                                      AS region
FROM audit_log
WHERE audit_log.action IN ('strategy.decide', 'strategy.rollback');

-- ───────────────────────────── ④ 回滚台账（append-only 视图） ─────────────────────────────
--
-- ★ 单独一张视图而不是从 v_strategy_history 里 filter：
--   回滚需要额外的 undo_of（被撤销的那条记录）字段，
--   而 decide 记录没有这个字段。混在一张视图里会让读方必须
--   「猜哪几列对这条记录有意义」—— 那是错误的温床。
CREATE OR REPLACE VIEW v_strategy_rollback AS
SELECT
    audit_log.at                      AS at,
    audit_log.actor                   AS actor,
    audit_log.detail->>'choiceId'     AS choice_id,
    audit_log.detail->>'from'         AS from_option,
    audit_log.detail->>'to'           AS to_option,
    audit_log.detail->>'undoOf'       AS undo_of_entry_id,
    audit_log.detail->'affectedBuckets' AS affected_buckets,
    audit_log.region                  AS region
FROM audit_log
WHERE audit_log.action = 'strategy.rollback';

COMMIT;
