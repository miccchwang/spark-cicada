-- 0003_precompute.sql —— 预计算桶物化（M-PRECOMP）
--
-- 原则（docs/01 §5、docs/05 G4）：
--   * 请求时只读不算 ⇒ 基础毫秒级。
--   * 桶内每行记录 algo_version / rule_version，便于版本漂移检测与重算触发。
--   * 派生量允许 NULL：代表「依赖缺失被跳过」，前端渲染「待接入」。
--   * 绝不用 0 / 均值 / 摊分填补缺失（fail-closed）。

BEGIN;

-- 月度损益桶（对应 buckets/pnl_month.yaml）
-- grain: [month, channel_code, shop_id, brand]
CREATE TABLE IF NOT EXISTS bucket_pnl_month (
    id              bigserial   PRIMARY KEY,
    month           date        NOT NULL,   -- 该月 1 日
    channel_code    text        NOT NULL,
    shop_id         text        NOT NULL,
    brand           text        NULL,
    -- ── 派生量（全部允许 NULL；NULL=依赖缺失跳过）──
    rev             numeric(18,4) NULL,
    cogs            numeric(18,4) NULL,
    gp              numeric(18,4) NULL,
    gmp             numeric(7,4)  NULL,     -- 毛利率（分母 0 → NULL）
    net_contrib     numeric(18,4) NULL,     -- 净贡献（依赖 slot.affiliate，当前多被跳过）
    -- ── 覆盖率与追溯 ──
    cov_cogs        numeric(5,4) NULL,      -- 该行成本覆盖率
    cov_affiliate   numeric(5,4) NULL,
    skipped_fields  text[]      NOT NULL DEFAULT '{}',  -- 因缺失被跳过的字段
    -- ── 版本（漂移检测）──
    algo_versions   jsonb       NOT NULL DEFAULT '{}',
    rule_versions   jsonb       NOT NULL DEFAULT '{}',
    built_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_bucket_pnl_month_spark_cicada_grain
        UNIQUE (month, channel_code, shop_id, brand)
);

CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_month   ON bucket_pnl_month(month);
CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_channel ON bucket_pnl_month(channel_code, month);
CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_shop    ON bucket_pnl_month(shop_id, month);
CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_brand   ON bucket_pnl_month(brand, month);

-- ── 构建 ledger：每次桶构建的记录（可比对版本、可回滚） ──
CREATE TABLE IF NOT EXISTS precomp_build_log (
    id              bigserial   PRIMARY KEY,
    bucket_id       text        NOT NULL,
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    rows_written    bigint,
    rows_skipped    bigint,
    algo_versions   jsonb       NOT NULL DEFAULT '{}',
    rule_versions   jsonb       NOT NULL DEFAULT '{}',
    status          text        NOT NULL DEFAULT 'RUNNING'
                                CHECK (status IN ('RUNNING','SUCCESS','FAILED')),
    error           text
);

CREATE INDEX IF NOT EXISTS idx_precomp_build_log_bucket ON precomp_build_log(bucket_id, started_at DESC);

COMMIT;
