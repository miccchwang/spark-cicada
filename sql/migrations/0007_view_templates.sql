-- 0007_view_templates.sql —— M-TEMPLATE 视图模板（可自命名，需求第 10 条）
--
-- 设计要点（见 docs/01 §10）：
--   * 模板**就是**一个被持久化的 QueryState + 视图偏好（列偏好 + 布局）。
--     正因如此它天然可深链（§9.3）：套用模板 = 重建整个 QueryState。
--   * 三档可见范围：personal（仅本人）/ team（本组、本部门）/ system（全体默认视图）。
--     档位不是「一个字段」而是**授权边界**：读取必须按档位过滤，
--     因此这里既落库也落读取策略的约束（在 store 层实现，见 groupstore/templatestore）。
--   * 「设为本页默认」= 同一 (owner/scope, page) 下唯一 is_default，
--     用**部分唯一索引**在 DB 层强制，而不是靠应用层自觉。
--
-- ★ 为什么不把模板塞进 fact_entitlement / 别的新表：
--   模板是**用户产物**，不是权限定义；它引用 QueryState（前端契约），
--   与权限求值无关。混进权限表会让「权限」与「偏好」互相污染。

BEGIN;

-- ───────────────────────────── 视图模板 ─────────────────────────────
CREATE TABLE IF NOT EXISTS dim_view_template (
    id            text        PRIMARY KEY,              -- tpl_xxx
    name          text        NOT NULL,                 -- 用户自命名
    scope         text        NOT NULL
                              CHECK (scope IN ('personal','team','system')),
    owner         text        NOT NULL REFERENCES dim_org(account),
    -- 所属页面路由（如 /report /pnl /strategy）。模板按页隔离，
    -- 避免在报表页套用 P&L 页的筛选造成「看似套上了、其实口径错了」。
    page          text        NOT NULL,
    -- 完整筛选状态（QueryState 的 JSON 快照）。
    -- ★ 存 jsonb 而非拍平：QueryState 会随协议演进新增维度，
    --   jsonb 让新增维度**自动兼容**旧模板（缺字段即用默认），无需迁表。
    query_state   jsonb       NOT NULL,
    -- 列显示 / 顺序 / 宽度 / 冻结
    columns       jsonb       NOT NULL DEFAULT '[]',
    -- 层级展开状态、面板位置等
    layout        jsonb       NOT NULL DEFAULT '{}',
    -- 使用次数：用于「我的常用」排序与智能推荐（§10.3）
    use_count     bigint      NOT NULL DEFAULT 0 CHECK (use_count >= 0),
    -- 是否为该页默认模板（同 scope+owner+page 下唯一，见下方部分唯一索引）
    is_default    boolean     NOT NULL DEFAULT false,
    -- 模板契约版本（QueryState 协议版本）。套用时若发现版本差异，
    -- 由前端决定是否做兼容处理，而非静默按新协议解读旧数据。
    template_ver  text        NOT NULL DEFAULT '1.0',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- 按「档位 + 归属 + 页面」取列表（模板中心的三个主要查询之一）
CREATE INDEX IF NOT EXISTS idx_view_template_scope_page
    ON dim_view_template(scope, owner, page);

-- 按页面取「该页所有可见模板」（含 system 档，与 owner 无关）
CREATE INDEX IF NOT EXISTS idx_view_template_page
    ON dim_view_template(page);

-- 常用排序：同 owner+page 下按 use_count 倒序，避免全表排序
CREATE INDEX IF NOT EXISTS idx_view_template_use
    ON dim_view_template(owner, page, use_count DESC);

-- ★ 「本页默认」唯一性下沉到 DB：
--   同一 (scope, owner, page) 下最多一个 is_default=true。
--   仅约束 is_default=true 的行（部分索引），false 的行不受限。
--   这样「设默认」只需在事务里先清旧、后置新，且并发下也不可能出现两个默认。
CREATE UNIQUE INDEX IF NOT EXISTS uq_view_template_default_per_owner_page
    ON dim_view_template(scope, owner, page)
    WHERE is_default;

-- ───────────────────────────── 模板块（团队档成员） ─────────────────────────────
--
-- team 档模板可指定「哪些组/部门可见」。
-- 不指定（表内无行）= 跟随 owner 所在主部门（最省事的默认）。
-- 这与 D9 矩阵型组织链路一致：可见性按组织归属解析，而非硬编码名单。
CREATE TABLE IF NOT EXISTS dim_view_template_share (
    template_id   text        NOT NULL REFERENCES dim_view_template(id) ON DELETE CASCADE,
    -- 可见对象：组（grp_xxx）或部门（dept code）。用 kind 区分，避免命名空间冲突。
    subject_kind  text        NOT NULL CHECK (subject_kind IN ('group','dept')),
    subject_id    text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (template_id, subject_kind, subject_id)
);

CREATE INDEX IF NOT EXISTS idx_view_template_share_subject
    ON dim_view_template_share(subject_kind, subject_id);

-- ───────────────────────────── 套用台账（append-only 审计） ─────────────────────────────
--
-- ★ 与权限无关，但**必须留痕**：模板是「批量改口径」的载体，
--   若一个 system 档模板被人改成错误筛选，事后要能查出「谁在何时套用过」。
--   因此套用行为写 audit_log（沿用 0001 的 append-only 表），此处不另建表，
--   仅建一个便于按模板聚合的**视图**。
CREATE OR REPLACE VIEW v_view_template_usage AS
SELECT
    (detail->>'templateId') AS template_id,
    actor,
    count(*)                AS apply_count,
    max(at)                 AS last_used_at
FROM audit_log
WHERE action = 'view_template.apply'
GROUP BY 1, 2;

COMMIT;
