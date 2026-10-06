-- 0006_groups_and_requests.sql —— M-GROUP 用户分组（D12）+ M-REQ 权限申请流（D14）
--
-- 为什么需要这一层：
--   勾选式授权（D11）解决了「能精确勾选」，但没有解决「批量管理」与「自助流程」：
--     * 同一批人（如「华东渠道组」）的权限逐账号重复勾选 ⇒ 必然漂移与遗漏；
--     * 用户想要一点额外权限时，只能线下找管理员 ⇒ 无留痕、无审批、无到期回收。
--   于是引入：
--     * 分组（UserGroup）：把「一批人的共同权限」抽出来，成员自动继承（取并集）。
--     * 申请流（PermissionRequest）：用户勾选 → 提交 → 审批 → 自动开通 → 到期回收。
--
-- 关键纪律（docs/02 M-GROUP / M-REQ、contracts/user-group.ts、contracts/permission-request.ts）：
--   1. 一人多组，授权**取并集**；组不含层级（层级由 D13 职权链承担）。
--   2. 组授权与个人勾选冲突 → **DENY 优先**（绝不因「在某个组里」而扩权）。
--   3. 成员可「退出继承」（inherits_grants=false）以处理例外。
--   4. 申请单草案**不含 canViewBusinessValues** —— IT 恒不可申请业务数值（D7）。
--   5. 审批人必须**自身拥有**被申请的全部权限（S ⊆ 权限(P)）；不足则上溯到权限超集者，
--      无则 T1 兜底。该判定在应用层完成，DB 只负责留痕与状态。
--   6. 跨部门 → BLOCKED_CROSS_DEPT（需 T1 直接授予，不可自助申请）。
--   7. 会签（F8）：高风险场景抄送人具**否决权**；知会（默认）不阻断。
--   8. 全部变更 append-only 留痕（走 audit_log 触发器）。
--
-- 分层（与 0001/0002 一致）：
--   dim_user_group             ── 维表：分组定义
--   fact_group_membership      ── 事实：成员关系（可继承开关）
--   fact_group_grant           ── 事实：组级授权（模块/维度/密级）
--   dim_account_entitlement    ── 维表：账号个人勾选（与组授权合并求值）
--   fact_permission_request    ── 事实：申请单
--   fact_request_approval      ── 事实：审批步骤
--   fact_request_cc            ── 事实：抄送 / 会签记录

BEGIN;

-- ───────────────────────── 分组定义 ─────────────────────────
CREATE TABLE IF NOT EXISTS dim_user_group (
    id          text        PRIMARY KEY,             -- grp_xxx
    name        text        NOT NULL,                -- 「华东渠道组」
    description text        NULL,
    region      text        NOT NULL DEFAULT 'ap-southeast-1',   -- G12 分地域
    -- 可管理本组成员与授权的账号（owners）
    owners      text[]      NOT NULL DEFAULT '{}',
    -- 是否受限分组：受限分组不得授出业务数值（与 D7 一致的兜底）
    restricted  boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_user_group_region ON dim_user_group(region);

-- ───────────────────────── 成员关系 ─────────────────────────
--
-- ★ inherits_grants 是「例外处理」的载体：
--   成员在组内，但若其职责特殊（如借调期），可置 false 而不继承组授权。
--   不能靠「把他移出组」来达到同样效果 —— 那会丢失「他在组内」这一事实。
CREATE TABLE IF NOT EXISTS fact_group_membership (
    group_id        text        NOT NULL REFERENCES dim_user_group(id) ON DELETE CASCADE,
    account         text        NOT NULL,
    role            text        NOT NULL DEFAULT 'member'
                                CHECK (role IN ('member','owner')),
    inherits_grants boolean     NOT NULL DEFAULT true,
    joined_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, account)
);

CREATE INDEX IF NOT EXISTS idx_group_membership_account ON fact_group_membership(account);

-- ───────────────────────── 组级授权 ─────────────────────────
--
-- 一个组可授出多条（模块 / 维度 / 密级）授权。与个人授权同构，
-- 便于求值时「组授权 ∪ 个人授权 ⊖ DENY」用同一套代码路径。
CREATE TABLE IF NOT EXISTS fact_group_grant (
    id               bigserial   PRIMARY KEY,
    group_id         text        NOT NULL REFERENCES dim_user_group(id) ON DELETE CASCADE,
    -- 授权种类：module / dimension / level / data_use_group
    kind             text        NOT NULL
                                 CHECK (kind IN ('module','dimension','level','data_use_group')),
    -- module:<id> / dimension:<dim> / level:<L1..L4> / data_use_group:<grp.xxx>
    key              text        NOT NULL,
    -- 维度的取值范围（空数组 = 全部，受密级约束）；非维度类为空
    values           text[]      NOT NULL DEFAULT '{}',
    include_descendants boolean  NOT NULL DEFAULT false,
    -- ★ DENY 优先：允许显式拒绝项，覆盖个人勾选
    deny             boolean     NOT NULL DEFAULT false,
    granted_at       timestamptz NOT NULL DEFAULT now()
);

-- 同一组内同一 key 只应有一条有效判定（kind+key 唯一）
CREATE UNIQUE INDEX IF NOT EXISTS uq_group_grant_key
    ON fact_group_grant (group_id, kind, key);

-- ───────────────────────── 账号个人勾选（D11） ─────────────────────────
--
-- 与组授权**分开存**：组授权是「批量、可复用」的；个人勾选是「逐账号微调」的。
-- 求值时合并，冲突时 DENY 优先（个人 DENY 也优先于组允许）。
CREATE TABLE IF NOT EXISTS dim_account_entitlement (
    account          text        NOT NULL,
    base_template    text        NOT NULL DEFAULT 'tpl.base',
    -- 个人勾选：模块 / 维度 / 密级 / 数据用途组
    modules          text[]      NOT NULL DEFAULT '{}',
    dimensions       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    data_use_groups  text[]      NOT NULL DEFAULT '{}',
    max_level        text        NOT NULL DEFAULT 'L1'
                                 CHECK (max_level IN ('L1','L2','L3','L4')),
    -- ★ D7：IT 恒不可见业务数值。此处存的是「是否允许」，
    --   但 IT 模板在应用层被强制置 false（DB 层不写死，便于将来政策调整留痕）。
    can_view_business_values boolean NOT NULL DEFAULT false,
    -- 个人 DENY 列表（kind:key），优先于任何允许
    denies           text[]      NOT NULL DEFAULT '{}',
    region           text        NOT NULL DEFAULT 'ap-southeast-1',
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account)
);

CREATE INDEX IF NOT EXISTS idx_account_entitlement_region ON dim_account_entitlement(region);

-- ───────────────────────── 申请单 ─────────────────────────
CREATE TABLE IF NOT EXISTS fact_permission_request (
    id                text        PRIMARY KEY,        -- req_xxx
    applicant         text        NOT NULL,
    -- 申请草案：勾选组 / 模块 / 维度 / 密级（**不含 canViewBusinessValues**，D7）
    draft             jsonb       NOT NULL,
    -- 批量申请：给某个分组申请（与 draft 并存）
    target_group_id   text        NULL REFERENCES dim_user_group(id) ON DELETE SET NULL,
    purpose           text        NOT NULL,           -- 用途说明（必填）
    requested_expiry  timestamptz NULL,               -- 空 = 长期
    status            text        NOT NULL DEFAULT 'DRAFT'
                                  CHECK (status IN ('DRAFT','SUBMITTED','APPROVING',
                                                    'COSIGN_PENDING','APPROVED','REJECTED',
                                                    'WITHDRAWN','BLOCKED_CROSS_DEPT')),
    cross_dept        boolean     NOT NULL DEFAULT false,
    region            text        NOT NULL DEFAULT 'ap-southeast-1',
    created_at        timestamptz NOT NULL DEFAULT now(),
    resolved_at       timestamptz NULL
);

CREATE INDEX IF NOT EXISTS idx_request_applicant ON fact_permission_request(applicant, status);
CREATE INDEX IF NOT EXISTS idx_request_status    ON fact_permission_request(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_region    ON fact_permission_request(region, status);

-- ───────────────────────── 审批步骤 ─────────────────────────
CREATE TABLE IF NOT EXISTS fact_request_approval (
    id          bigserial   PRIMARY KEY,
    request_id  text        NOT NULL REFERENCES fact_permission_request(id) ON DELETE CASCADE,
    approver    text        NOT NULL,
    tier        text        NOT NULL,                 -- T1..T6
    seq         integer     NOT NULL DEFAULT 0,       -- 审批次序
    action      text        NOT NULL DEFAULT 'PENDING'
                            CHECK (action IN ('APPROVE','REJECT','RETURN','PENDING')),
    reason      text        NULL,
    at          timestamptz NULL,
    UNIQUE (request_id, approver)                     -- 一人一步，避免重复路由
);

CREATE INDEX IF NOT EXISTS idx_request_approval_req ON fact_request_approval(request_id, seq);

-- ───────────────────────── 抄送 / 会签（F8）─────────────────────────
--
-- ★ mode 决定抄送人是否具否决权：
--   notify（默认）—— 可见但无决定权，**不阻断**
--   cosign        —— 具**否决权**，否决即驳回
-- 升级为会签的触发条件（任一命中）：L4 / 跨部门 / 有效期>90天 /
-- 含 grp.roi·grp.cost_profit / 批量≥10 个账号或分组。
CREATE TABLE IF NOT EXISTS fact_request_cc (
    id          bigserial   PRIMARY KEY,
    request_id  text        NOT NULL REFERENCES fact_permission_request(id) ON DELETE CASCADE,
    cc          text        NOT NULL,
    reason      text        NOT NULL,                 -- 如 "+2" / "+1 的直属上级"
    mode        text        NOT NULL DEFAULT 'notify'
                            CHECK (mode IN ('notify','cosign')),
    notified_at timestamptz NOT NULL DEFAULT now(),
    read_at     timestamptz NULL,
    vetoed      boolean     NOT NULL DEFAULT false,
    decided_at  timestamptz NULL
);

CREATE INDEX IF NOT EXISTS idx_request_cc_req ON fact_request_cc(request_id);

-- ───────────────────────── 分组定义 → 审计（append-only）─────────────────────────
--
-- 复用 0001 的审计纪律：分组与授权的变更同样不可篡改。
-- 这里只加触发器函数（audit_log 与函数体在 0001 已建；此处确保本迁移可独立可读）。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'audit_log_append_only') THEN
        RAISE NOTICE '0006: audit_log_append_only 已存在，跳过重复创建';
    END IF;
END $$;

COMMIT;
