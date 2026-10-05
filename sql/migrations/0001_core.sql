-- 0001_core.sql —— spark-cicada 核心数据层（M1 基础）
--
-- 纪律（见 docs/01 §5、docs/03）：
--   1. 算法与数据源分离：事实表只存「原子量」，派生量进预计算桶。
--   2. 缺失必跳过：金额/数量列允许 NULL，且 NULL 表示「无数据」而非 0。
--   3. 唯一键红线：所有唯一约束显式带库名/表名，避免跨库歧义（承袭 KODP 教训）。
--   4. 审计表 append-only，永不回滚（docs/04 §6.3）。
--
-- 说明：本迁移只建「结构」；预计算桶由 M-PRECOMP 依 buckets/*.yaml 另建（0003）。

BEGIN;

-- 说明（重要）：此处**不再** CREATE EXTENSION "pgcrypto"。
--
-- 原因：整套迁移没有任何语句使用 pgcrypto 的函数（无 gen_random_uuid/digest/crypt），
-- 而 CREATE EXTENSION 在多数托管 Postgres 上需要**超级用户**权限。
-- 应用账号 spark 是普通账号 ⇒ 该语句会以
--   ERROR: permission denied to create extension "pgcrypto"
-- 失败，导致整个 0001 回滚、迁移链断在第一环。
--
-- 纪律：**不要申请用不到的权限**。将来真正需要时再新增一个独立迁移，
-- 并在其中显式说明所需的权限前提（而非默认假设超级用户）。
-- 主键一律用 bigserial/text，不依赖 pgcrypto 的随机 UUID。

-- ───────────────────────────── 组织维表（F9=A 单主属 / F10=C 混合） ─────────────────────────────
-- 每人唯一 primaryDept + 唯一 supervisor；虚线上级仅抄送（dotted_line_supervisors）。
CREATE TABLE IF NOT EXISTS dim_org (
    account                     text        PRIMARY KEY,
    display_name                text        NOT NULL,
    supervisor                  text        NULL REFERENCES dim_org(account),
    tier                        text        NOT NULL CHECK (tier IN ('T1','T2','T3','T4','T5','T6')),
    primary_dept                text        NOT NULL,
    -- DERIVED=组织架构派生（默认）；MANUAL=人工覆盖（F10 人工优先）
    dept_source                 text        NOT NULL DEFAULT 'DERIVED'
                                            CHECK (dept_source IN ('DERIVED','MANUAL')),
    -- 虚线汇报上级：仅进抄送，不入审批链（F9）
    dotted_line_supervisors     text[]      NOT NULL DEFAULT '{}',
    can_approve                 boolean     NOT NULL DEFAULT false,
    active                      boolean     NOT NULL DEFAULT true,
    updated_at                  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_dim_org_supervisor   ON dim_org(supervisor);
CREATE INDEX IF NOT EXISTS idx_dim_org_primary_dept ON dim_org(primary_dept);

-- ───────────────────────────── 数据链（谁负责这份数据） ─────────────────────────────
CREATE TABLE IF NOT EXISTS dim_data_chain (
    id              bigserial   PRIMARY KEY,
    -- 数据范围，如 channel:TK-TH / store:S123 / brand:KONVY
    scope           text        NOT NULL,
    owner_account   text        NOT NULL REFERENCES dim_org(account),
    dept            text        NOT NULL,
    -- 责任链缓存：[owner, owner.supervisor, …, T1]
    owner_chain     text[]      NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_dim_data_chain_spark_cicada_scope UNIQUE (scope)
);

CREATE INDEX IF NOT EXISTS idx_dim_data_chain_owner ON dim_data_chain(owner_account);

-- ───────────────────────────── 用户分组（D12） ─────────────────────────────
CREATE TABLE IF NOT EXISTS dim_group (
    id          text        PRIMARY KEY,             -- grp_xxx
    name        text        NOT NULL,
    description text,
    grants      jsonb       NOT NULL DEFAULT '{}',   -- GroupGrants
    owners      text[]      NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS dim_group_member (
    group_id        text        NOT NULL REFERENCES dim_group(id) ON DELETE CASCADE,
    account         text        NOT NULL REFERENCES dim_org(account),
    role            text        NOT NULL DEFAULT 'member' CHECK (role IN ('member','owner')),
    -- false = 在组内但不继承组授权（例外处理）
    inherits_grants boolean     NOT NULL DEFAULT true,
    joined_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, account)
);

-- ───────────────────────────── 账号权限（D11 逐项勾选） ─────────────────────────────
CREATE TABLE IF NOT EXISTS fact_entitlement (
    account                 text        PRIMARY KEY REFERENCES dim_org(account),
    base_template           text,
    groups                  text[]      NOT NULL DEFAULT '{}',
    supervisor              text,
    data_use_groups         jsonb       NOT NULL DEFAULT '[]',  -- GroupScopeGrant[]
    modules                 jsonb       NOT NULL DEFAULT '{"enabled":[],"disabled":[]}',
    dimensions              jsonb       NOT NULL DEFAULT '[]',
    max_level               text        NOT NULL DEFAULT 'L1' CHECK (max_level IN ('L1','L2','L3','L4')),
    field_overrides         jsonb       NOT NULL DEFAULT '[]',
    -- D7=取消：IT 恒 false 且不可勾选；非 IT 默认 true
    can_view_business_values boolean    NOT NULL DEFAULT true,
    temp_grants             jsonb       NOT NULL DEFAULT '[]',
    grants                  jsonb       NOT NULL DEFAULT '[]',
    updated_at              timestamptz NOT NULL DEFAULT now(),
    -- 硬约束：IT 角色绝不允许 can_view_business_values=true（fail-closed 到 DB 层）
    CONSTRAINT ck_fact_entitlement_spark_cicada_it_no_business
        CHECK (NOT (base_template = 'tpl.it' AND can_view_business_values))
);

-- ───────────────────────────── 权限申请单（D14） ─────────────────────────────
CREATE TABLE IF NOT EXISTS fact_permission_request (
    id               text        PRIMARY KEY,        -- req_xxx
    applicant        text        NOT NULL REFERENCES dim_org(account),
    draft            jsonb       NOT NULL,            -- RequestDraft
    target_group_id  text        REFERENCES dim_group(id),
    purpose          text        NOT NULL,
    requested_expiry timestamptz,
    status           text        NOT NULL CHECK (status IN (
                        'DRAFT','SUBMITTED','APPROVING','COSIGN_PENDING',
                        'APPROVED','REJECTED','WITHDRAWN','BLOCKED_CROSS_DEPT')),
    approvals        jsonb       NOT NULL DEFAULT '[]',   -- ApprovalStep[]
    ccs              jsonb       NOT NULL DEFAULT '[]',   -- CcRecord[]
    cross_dept       boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    resolved_at      timestamptz
);

CREATE INDEX IF NOT EXISTS idx_fact_perm_req_applicant ON fact_permission_request(applicant);
CREATE INDEX IF NOT EXISTS idx_fact_perm_req_status    ON fact_permission_request(status);

-- ───────────────────────────── 审计（append-only，永不回滚） ─────────────────────────────
CREATE TABLE IF NOT EXISTS audit_log (
    id          bigserial   PRIMARY KEY,
    at          timestamptz NOT NULL DEFAULT now(),
    actor       text        NOT NULL,
    action      text        NOT NULL,
    target      text,
    -- 结构化上下文（不落任何密钥明文，符合 docs/09）
    detail      jsonb       NOT NULL DEFAULT '{}',
    request_id  text,
    -- 地域（分地域审计，见 docs/04 §6）
    region      text        NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_log_at     ON audit_log(at);
CREATE INDEX IF NOT EXISTS idx_audit_log_actor  ON audit_log(actor);
CREATE INDEX IF NOT EXISTS idx_audit_log_action ON audit_log(action);

-- 审计表防篡改：禁止 UPDATE / DELETE（append-only 铁律）
CREATE OR REPLACE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only (spark-cicada): % not allowed', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_log_no_update ON audit_log;
CREATE TRIGGER trg_audit_log_no_update
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();

-- ───────────────────────────── 事实表：原子量（不预置派生量） ─────────────────────────────
-- 设计要点：本表只存「直接可采的原子事实」；GP/GMP/净贡献等一律由 M-ALGO 产出进桶。
CREATE TABLE IF NOT EXISTS fact_sales_daily (
    id              bigserial   PRIMARY KEY,
    -- 业务键（显式带表名，避免跨库歧义）
    stat_date       date        NOT NULL,
    channel_code    text        NOT NULL,
    shop_id         text        NOT NULL,
    brand           text        NULL,
    spu             text        NULL,
    sku             text        NULL,
    -- 原子量：NULL = 无数据（不得写 0 冒充）
    qty             numeric(18,4) NULL CHECK (qty IS NULL OR qty >= 0),
    revenue         numeric(18,4) NULL,
    -- 采集溯源
    source          text        NOT NULL,
    ingested_at     timestamptz NOT NULL DEFAULT now(),
    region          text        NOT NULL DEFAULT 'ap-southeast-1',
    CONSTRAINT uq_fact_sales_daily_spark_cicada_bizkey
        UNIQUE (stat_date, channel_code, shop_id, spu, sku)
);

CREATE INDEX IF NOT EXISTS idx_fact_sales_daily_key  ON fact_sales_daily(stat_date, channel_code);
CREATE INDEX IF NOT EXISTS idx_fact_sales_daily_shop ON fact_sales_daily(shop_id, stat_date);

COMMIT;
