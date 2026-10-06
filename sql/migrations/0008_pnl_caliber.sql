-- 0008_pnl_caliber.sql —— M-PNL 折扣口径（A/B 可切换，需求承袭 KODP 经验）
--
-- ══════════════════════════════════════════════════════════════════════════
-- ★★ 本迁移的纪律：**增量补强 0001/0002，绝不另起一套表**。
--
--   0002 已经建好 registry_rule_set（id, version, scope, items）——
--   口径（rule.discount.caliber）正是「一条规则集」，就该住在那张表里。
--   再建一张 pnl_caliber / dim_pnl_caliber 就是本仓库反复强调的
--   「两个真源」反模式：两处都能改口径，迟早互相矛盾。
-- ══════════════════════════════════════════════════════════════════════════
--
-- 口径语义（docs/03 §4.3，承袭 KODP 实测）：
--   * 口径 A（参考口径）：Seller Discount 计入**营销费用**
--       → 营销费用虚高，易被误读为「费用失控」。
--   * 口径 B（Finance 口径）：Seller Discount 作 **contra-revenue**（收入抵减）
--       → 揭示真实问题是「折扣压缩毛利」。
--
-- ★ 实测结论（务必留在库里，供后人溯源）：
--     **净收入两口径完全相同** —— 折扣本就从 GMV 扣除，
--     口径切换只把它从「费用侧」搬到「收入侧」展示。
--     KODP 实测：同一份数据，口径 A 净贡献 −152,567.19、口径 B +123.17。
--     两者都没错，但**净收入不该不同**。若不同 ⇒ 公式被改坏（详见
--     backend/internal/pnl/pnl.go 的 CheckNetRevenueInvariant）。
--
-- ★ 决策依据 D10：「**分模块各自默认**」（不是全局一个默认）——
--   经营报表=运营口径 A，P&L=财务口径 B。两个模块的读者不是同一批人。

BEGIN;

-- ───────────────────────────── 口径规则集（增量：只插入一行数据） ─────────────────────────────
--
-- 形状与 0002 的 registry_rule_set 完全一致，不新增列。
-- items 里把「两个口径各自的定义 + 折扣归属 + 模块默认」都声明为**数据**
-- （而不是散在代码 if 里），这样口径表可被 SQL 查询、可被审计、可被导出。
INSERT INTO registry_rule_set (id, version, scope, items)
VALUES (
    'rule.discount.caliber',
    1,
    '{"global":true}'::jsonb,
    '[
      {
        "id": "A",
        "name": "A · 参考口径",
        "definition": "Seller Discount 计入营销费用",
        "seller_discount_role": "marketing_expense",
        "discount_target_line": "marketing",
        "is_default_for": ["module.report"],
        "misreading_risk": "营销费用会虚高，容易被误读为「费用失控」——实际问题可能是折扣过深"
      },
      {
        "id": "B",
        "name": "B · Finance 口径",
        "definition": "Seller Discount 作 contra-revenue（收入抵减）",
        "seller_discount_role": "contra_revenue",
        "discount_target_line": "seller_discount",
        "is_default_for": ["module.pnl"],
        "misreading_risk": "收入侧被压低，容易被误读为「卖不动」——实际问题可能是毛利率被折扣压缩"
      }
    ]'::jsonb
)
ON CONFLICT (id, version) DO UPDATE SET
    scope = EXCLUDED.scope,
    items = EXCLUDED.items,
    updated_at = now();

-- ───────────────────────────── 口径不变量（写成数据，供审计与 CI 引用） ─────────────────────────────
--
-- ★ 把「net_revenue 两口径必须相同」这条**不变量本身**登记进库，
--   是为了让它有一个可被 SQL 查询的权威出处：
--   审计脚本、数据质量巡检、以及「为什么这张报表被拦下了」的排障，
--   都能指向同一行，而不是各自在代码注释里写一份。
INSERT INTO registry_rule_set (id, version, scope, items)
VALUES (
    'rule.pnl.invariant',
    1,
    '{"global":true}'::jsonb,
    '[
      {
        "id": "caliber_equality",
        "name": "口径不变量：两口径下必须相同的行",
        "lines": ["gross_listing", "net_revenue"],
        "reason": "折扣本就从净收入扣除；口径只改展示归属，不改数值口径。若此处不成立，说明公式被改坏，而非新口径。",
        "on_violation": "reject",
        "ref": "docs/03 §4.3 / backend/internal/pnl/pnl.go#CheckNetRevenueInvariant"
      }
    ]'::jsonb
)
ON CONFLICT (id, version) DO UPDATE SET
    scope = EXCLUDED.scope,
    items = EXCLUDED.items,
    updated_at = now();

-- ───────────────────────────── 口径切换台账（append-only 视图） ─────────────────────────────
--
-- ★ 复用 0001 的 audit_log，不另建表（audit_log 已有 UPDATE/DELETE 拦截触发器）。
--   口径切换由 backend/internal/api/pnl.go 写 action='pnl.caliber.change'。
--
--   为什么一个「展示选项」值得审计：口径决定同一份数据被读成「巨亏」还是「微利」。
--   两个部门拿着不一致的截图争吵时，唯一能厘清的就是这张台账。
CREATE OR REPLACE VIEW v_pnl_caliber_change AS
SELECT
    audit_log.at                                              AS at,
    audit_log.actor                                           AS actor,
    COALESCE(audit_log.detail->>'module', 'module.pnl')       AS module,
    audit_log.detail->>'from'                                 AS from_caliber,
    audit_log.detail->>'to'                                   AS to_caliber,
    audit_log.detail->>'source'                               AS source,
    COALESCE((audit_log.detail->>'dirty')::boolean, false)    AS dirty,
    audit_log.detail->>'requested'                            AS requested_raw,
    audit_log.region                                          AS region
FROM audit_log
WHERE audit_log.action = 'pnl.caliber.change';

COMMIT;
