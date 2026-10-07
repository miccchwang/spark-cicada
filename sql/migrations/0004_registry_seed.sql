-- 0004_registry_seed.sql —— 注册表种子（槽 / 算法 / 规则 / 桶）。
--
-- 目的：让 spark-cicada 首次部署即具备**完整可运行**的注册表，
-- 且这些声明与仓库内 algorithms/*.yaml、slots/*.yaml、buckets/*.yaml 保持一致。
--
-- 纪律：
--   1. 算法公式**不含**数据源（G4 静态门）；数据来源一律经 depends_on_slots 声明。
--   2. 槽 status 如实标注：未接入的槽必须是 MISSING（而不是假装 ACTIVE）——
--      这样依赖它的算法会被 fail-closed 拦住，而不是算出假的 0。
--   3. 桶初始 state = STALE（必须先构桶才可查询，避免读到空桶当成「无数据」）。

BEGIN;

-- ───────────────────────────── 数据槽 ─────────────────────────────
-- slot.revenue：营收（平台订单事实）
INSERT INTO registry_slot
    (id, name, source_kind, source_ref, key_strategy, coverage_gate, freshness, permission, status, notes)
VALUES
    ('slot.revenue', '营收（平台订单）', 'db', 'fact_sales_daily.revenue',
     'date+channel+shop+spu+sku', 0.95, '1d', 'L2', 'ACTIVE',
     '来自订单事实表；缺失记为 NULL，不得补 0'),
    ('slot.cogs', '商品成本', 'master_table', 'master.sku_cost.unit_cost',
     'sku+month', 0.80, '7d', 'L3', 'ACTIVE',
     '主数据表；未维护 SKU 成本 ⇒ 该行 cogs 为 NULL'),
    ('slot.platform_fee', '平台佣金', 'db', 'rules/platform_fee.tk.yaml',
     'channel+country+month', 0.90, '7d', 'L3', 'ACTIVE',
     '规则化费率，多版本按生效期选取'),
    ('slot.affiliate', '联盟佣金', 'api', 'affiliate.v1',
     'date+shop', 0.80, '1d', 'L4', 'MISSING',
     '★ 尚未接入：依赖它的 net_contrib 一律跳过（fail-closed），绝不按 0 计'),
    ('slot.inventory_snap', '库存快照', 'db', 'fact_inventory_daily',
     'date+shop+sku', 0.85, '1d', 'L3', 'ACTIVE',
     '日快照；用于库存周转与滞销识别')
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name, source_kind = EXCLUDED.source_kind,
    source_ref = EXCLUDED.source_ref, key_strategy = EXCLUDED.key_strategy,
    coverage_gate = EXCLUDED.coverage_gate, freshness = EXCLUDED.freshness,
    permission = EXCLUDED.permission, status = EXCLUDED.status,
    notes = EXCLUDED.notes, updated_at = now();

-- ───────────────────────────── 算法 ─────────────────────────────
-- 注意公式列是**纯公式**，无 FROM/表名（G4）。
--
-- ★ 2026-10-08 单位口径对齐（G4 第九侧闸门「种子 ↔ 算法 YAML」同源检查抓出）：
--   本段此前与 `algorithms/*.yaml` **逐字不同** ——
--     种子 `algo.rev`/`cogs`/`gp`/`net_contrib` 写 `'CNY'`，而 YAML 写 `THB`；
--     种子 `algo.gmp` 写 `'ratio'`，而 YAML 写 `percent`。
--   这不是「种子错了」，而是**两侧各写一份词表**的必然结果：谁也没错、
--   就是不一致。而 `unit` 列在 0002 里是 `text NOT NULL`（**无 CHECK**），
--   加载期此前也无人校验 ⇒ 两侧分叉可以无限期共存。
--   现按 docs/03 §4.2「本位币 THB（rule.fx.base）」统一为 `THB`，
--   比率统一为 `%`（与 YAML 的 percent 归一同一形）。
INSERT INTO registry_algorithm
    (id, name, version, formula, unit, permission, depends_on_slots,
     writes_bucket, missing_policy, trace, notes)
VALUES
    ('algo.rev', '营收合计', 1, 'sum(revenue)', 'THB', 'L2',
     ARRAY['slot.revenue'], 'pnl_month', 'skip', true,
     '营业收入的原子汇总；缺失按跳过处理'),
    ('algo.cogs', '商品成本合计', 2, 'sum(unit_cost * qty)', 'THB', 'L3',
     ARRAY['slot.cogs'], 'pnl_month', 'skip', true,
     '成本 = 单位成本 × 数量；任一为 NULL 则该行跳过'),
    ('algo.gp', '毛利', 3, 'rev - cogs', 'THB', 'L3',
     ARRAY['slot.revenue', 'slot.cogs'], 'pnl_month', 'skip', true,
     '毛利 = 营收 - 成本（依赖两个槽）'),
    ('algo.gmp', '毛利率', 1, 'safe_div(gp, rev)', '%', 'L3',
     ARRAY['slot.revenue', 'slot.cogs'], 'pnl_month', 'null', true,
     '分母为 0 或缺失 ⇒ NULL（绝不写 Infinity / 0）'),
    ('algo.net_contrib', '净贡献', 1, 'gp - platform_fee - affiliate_fee', 'THB', 'L4',
     ARRAY['slot.platform_fee', 'slot.affiliate'], 'pnl_month', 'skip', true,
     '★ 依赖 slot.affiliate（当前 MISSING）⇒ 该列全为 NULL，前端显示「待接入」')
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name, version = EXCLUDED.version, formula = EXCLUDED.formula,
    unit = EXCLUDED.unit, permission = EXCLUDED.permission,
    depends_on_slots = EXCLUDED.depends_on_slots, writes_bucket = EXCLUDED.writes_bucket,
    missing_policy = EXCLUDED.missing_policy, trace = EXCLUDED.trace,
    notes = EXCLUDED.notes, updated_at = now();

-- ───────────────────────────── 规则集 ─────────────────────────────
-- 平台费率：Shopee 先略过（按用户要求），TK 按国别给版本。
--
-- ★ 2026-10-07 版本对齐（规则注册表闸门抓出的真缺陷）：
--   本处种子的 version 此前是 1 / 2，而仓库事实源 `rules/platform_fee.tk.yaml`
--   的 version 是 **6** —— 两侧都「各自自洽」，静态读单个文件发现不了。
--   后果不是「少一条断言」，而是 **G6 的规则漂移检测静默失效**：
--   桶的 rule_versions 声明 `{"rule.tk.fee": 2}`，平台按 (id,version) 取规则，
--   而仓库事实源已到 6 ⇒ 无论费率怎么改，桶都认为「规则没变」⇒ **永不重算**。
--   现与仓库对齐为 6（保持 ON CONFLICT 幂等）。
INSERT INTO registry_rule_set (id, version, scope, items)
VALUES
    ('rule.tk.fee', 1,
     '{"platform":"tiktok","country":"TH"}'::jsonb,
     '[{"id":"tk_th_default","name":"TK 泰国标准佣金","rate":0.05,"flat_per_order":0,"effective_from":"2026-01-01"}]'::jsonb),
    ('rule.tk.fee', 6,
     '{"platform":"tiktok","country":"TH"}'::jsonb,
     '[{"id":"platform_fee.tk","name":"TK 平台费率规则集（明细见 rules/platform_fee.tk.yaml）","rate":0,"flat_per_order":0}]'::jsonb),
    ('rule.shopee.fee', 1,
     '{"platform":"shopee","country":"TH"}'::jsonb,
     '[]'::jsonb)
ON CONFLICT (id, version) DO UPDATE SET
    scope = EXCLUDED.scope, items = EXCLUDED.items, updated_at = now();

-- ───────────────────────────── 预计算桶 ─────────────────────────────
INSERT INTO registry_bucket
    (id, grain, produced_by, refresh, algo_versions, rule_versions, indexes, state, updated_at)
VALUES
    ('pnl_month',
     ARRAY['month','channel_code','shop_id','brand'],
     ARRAY['algo.rev','algo.cogs','algo.gp','algo.gmp','algo.net_contrib'],
     'monthly_incremental',
     '{"algo.rev":1,"algo.cogs":2,"algo.gp":3,"algo.gmp":1,"algo.net_contrib":1}'::jsonb,
     '{"rule.tk.fee":2}'::jsonb,
     '[{"cols":["month"]},{"cols":["channel_code","month"]}]'::jsonb,
     'STALE', now()),
    ('inventory_snap',
     ARRAY['date','shop_id','sku'],
     ARRAY['algo.inventory_stock'],
     'daily',
     '{"algo.inventory_stock":1}'::jsonb,
     '{}'::jsonb,
     '[{"cols":["date"]}]'::jsonb,
     'STALE', now())
ON CONFLICT (id) DO UPDATE SET
    grain = EXCLUDED.grain, produced_by = EXCLUDED.produced_by,
    refresh = EXCLUDED.refresh, algo_versions = EXCLUDED.algo_versions,
    rule_versions = EXCLUDED.rule_versions, indexes = EXCLUDED.indexes,
    updated_at = now();

-- pnl_month 已备好种子数据前置条件时，可在此把 state 改为 FRESH；
-- 默认保持 STALE，强制走「先构桶再查询」的正确顺序（fail-closed）。

COMMIT;
