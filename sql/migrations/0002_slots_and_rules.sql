-- 0002_slots_and_rules.sql —— 数据槽注册表 + 规则版本（M1 数据层）
--
-- 数据槽（slot）= 「数据从哪来」的唯一声明（docs/03）。
-- 规则（rule）= 费率/口径的唯一事实源，带版本与生效时间。
--
-- 关键不变量：
--   * 槽的 coverage_gate 决定「可用性」；未达标 ⇒ 依赖它的算法 skip（fail-closed）。
--   * 槽的 status=MISSING ⇒ 直接不可用（如 slot.affiliate）。
--   * 规则多版本共存，按 effective_from/to 选版本；升级触发受影响桶 stale。

BEGIN;

-- ───────────────────────────── 数据槽 ─────────────────────────────
CREATE TABLE IF NOT EXISTS registry_slot (
    id              text        PRIMARY KEY,        -- slot.cogs
    name            text        NOT NULL,
    source_kind     text        NOT NULL CHECK (source_kind IN ('api','master_table','db','file')),
    source_ref      text        NOT NULL,
    key_strategy    text        NOT NULL,
    coverage_gate   numeric(5,4) NOT NULL DEFAULT 0.80 CHECK (coverage_gate BETWEEN 0 AND 1),
    freshness       text        NOT NULL,           -- 如 7d / 1d
    permission      text        NOT NULL CHECK (permission IN ('L1','L2','L3','L4')),
    -- ACTIVE / MISSING / DEGRADED / DISABLED
    status          text        NOT NULL DEFAULT 'ACTIVE'
                                CHECK (status IN ('ACTIVE','MISSING','DEGRADED','DISABLED')),
    notes           text,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- 槽覆盖率实测（每次采集/校验后写入；用于 coverage_gate 判定）
CREATE TABLE IF NOT EXISTS registry_slot_coverage (
    slot_id     text        NOT NULL REFERENCES registry_slot(id) ON DELETE CASCADE,
    scope       text        NOT NULL,          -- 如 month=2026-09 或 global
    coverage    numeric(5,4) NOT NULL CHECK (coverage BETWEEN 0 AND 1),
    sample_n    bigint      NOT NULL DEFAULT 0,
    measured_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (slot_id, scope)
);

-- ───────────────────────────── 算法注册表 ─────────────────────────────
CREATE TABLE IF NOT EXISTS registry_algorithm (
    id                text        PRIMARY KEY,      -- algo.gp
    name              text        NOT NULL,
    version           integer     NOT NULL,
    formula           text        NOT NULL,         -- 纯公式；不得出现数据源
    unit              text        NOT NULL,
    permission        text        NOT NULL CHECK (permission IN ('L1','L2','L3','L4')),
    depends_on_slots  text[]      NOT NULL,         -- 引用 registry_slot.id
    writes_bucket     text        NULL,
    missing_policy    text        NOT NULL DEFAULT 'skip'
                                  CHECK (missing_policy IN ('skip','null','error')),
    trace             boolean     NOT NULL DEFAULT true,
    notes             text,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- ───────────────────────────── 规则集（费率/口径） ─────────────────────────────
CREATE TABLE IF NOT EXISTS registry_rule_set (
    id          text        PRIMARY KEY,            -- rule.tk.fee
    version     integer     NOT NULL,
    scope       jsonb       NOT NULL DEFAULT '{}',  -- {platform, country, brand, channel}
    items       jsonb       NOT NULL DEFAULT '[]',  -- [{id,name,rate,flat_per_order,effective_from,...}]
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_registry_rule_set_spark_cicada_id_ver UNIQUE (id, version)
);

-- ───────────────────────────── 预计算桶注册 ─────────────────────────────
CREATE TABLE IF NOT EXISTS registry_bucket (
    id              text        PRIMARY KEY,        -- pnl_month
    grain           text[]      NOT NULL,           -- [month, channel_code, shop_id, brand]
    produced_by     text[]      NOT NULL,
    refresh         text        NOT NULL,           -- monthly_incremental | daily | manual
    algo_versions   jsonb       NOT NULL DEFAULT '{}',
    rule_versions   jsonb       NOT NULL DEFAULT '{}',
    indexes         jsonb       NOT NULL DEFAULT '[]',
    -- FRESH / STALE / BUILDING / FAILED
    state           text        NOT NULL DEFAULT 'STALE'
                                CHECK (state IN ('FRESH','STALE','BUILDING','FAILED')),
    last_built_at   timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

COMMIT;
