-- 0006_groups_and_requests.sql —— M-GROUP（D12）+ M-REQ（D14）**增量补强**
--
-- ══════════════════════════════════════════════════════════════════════════
-- ★ 本迁移的由来（一次真实的「两套真相」事故，值得写下来）
--
--   初版 0006 是**另起一套**结构：dim_user_group / fact_group_membership /
--   fact_group_grant / dim_account_entitlement / fact_request_approval /
--   fact_request_cc —— 七张全新的表。
--
--   真库一跑就炸：`ERROR: column "region" does not exist (SQLSTATE 42703)`。
--   排查后发现根因不是 SQL 写错，而是 —— **迁移 0001 早就建好了 M-GROUP / M-REQ 的表**：
--
--       dim_group                 ← 分组定义（含 grants jsonb）
--       dim_group_member          ← 成员关系（含 inherits_grants）
--       fact_entitlement          ← 账号权限（D11 逐项勾选）
--       fact_permission_request   ← 申请单（approvals / ccs 存 jsonb）
--
--   而 0001 的这份设计**与契约完全一致**：
--     * contracts/user-group.ts        v1.0 → grants: GroupGrants（单一 jsonb）
--     * contracts/permission-request.ts v1.0 → approvals: ApprovalStep[]、ccs: CcRecord[]
--     * docs/08 §2.1 数据模型         → UserGroup { grants: {...}, owners, ... }
--     * docs/08 §3.3 / §4             → +1 审批 / +2 抄送，approvals 内联
--
--   也就是说：**初版 0006 才是错的那一方**。它凭空发明了
--   region / restricted / deny / data_use_group 等契约里不存在的字段，
--   并把「一份 grants」拆成七张表 —— 与 0001 形成两套并行真相。
--
--   若放任不管，后果是长期而隐蔽的：
--     * 界面按 0001（jsonb）读、后台按 0006（子表）写 ⇒ 权限漂移；
--     * 同一个「组成员」有两个来源，DENY 到底以谁为准无法回答；
--     * 两套表都要维护，审计无法给出唯一答案。
--   这正是本仓库反复强调的纪律所禁止的：**同一事实只允许一处真相**。
--
--   ⇒ 因此本迁移**推倒重写**为「只做 0001 缺的东西」的增量补强。
-- ══════════════════════════════════════════════════════════════════════════
--
-- 0001 已经覆盖的部分（本迁移**不再重复建表**）：
--   dim_group（分组定义 + grants jsonb + owners）
--   dim_group_member（成员 + role + inherits_grants）
--   fact_entitlement（个人勾选 + D7 的 DB 层 CHECK）
--   fact_permission_request（申请单 + approvals/ccs jsonb + 状态 CHECK）
--
-- 本迁移补的三件事（每一件都对应一个**0001 无法表达的缺口**）：
--
--   ① **分组可授范围必须能被库层约束**（D7 兜底）。
--      0001 的 grants 是单个 jsonb，DB 无法对「IT 组不得含 canViewBusinessValues」
--      做 CHECK。这里加一条**表达式 CHECK**，让「IT 组带业务数值」在库层直接写入失败 ——
--      而不是指望每个写入路径都记得在应用层拦。
--
--   ② **组授权的变更要能按行审计**（docs/08 §5「建组/改名/改授权全部落审计」）。
--      整块 jsonb 的 UPDATE 在审计里只能看到「grants 变了」，看不到
--      「具体多了/少了哪个 module」。这里加 fact_group_grant_change ——
--      **只增不改**的变更流水，回答「谁在什么时候给哪个组加了什么」。
--
--   ③ **申请单的终态与到期回收需要可检索的索引**。
--      0001 只有 (applicant) 与 (status) 两个索引；回收任务要按
--      「APPROVED 且 requested_expiry 已过」筛选，需要复合索引才不至于全表扫。
--
-- 分层（与 0001/0002 一致）：本迁移**只加列/约束/索引/流水表**，不改任何既有列语义。

BEGIN;

-- ───────────────────────── ① 分组授权上界的库层约束（D7 兜底）─────────────
--
-- grants jsonb 的形状（contracts/user-group.ts GroupGrants）：
--   { "modules":[...], "dimensions":[...], "maxLevel":"L1..L4",
--     "canViewBusinessValues": bool }
--
-- ★ 判据的选择（这里刻意不用组名）：
--   初稿写了 `name LIKE '%IT%'` —— 那是**脆弱**的：一个叫「ITEM 铺货组」的
--   业务组会被误判成 IT 组，于是它的业务数值权限被库层硬拦，属于误伤；
--   而真正的 IT 组若叫「信息技术组」则又拦不住，属于漏放。
--   两种错误方向都有，且都不易在测试中发现。
--
--   改用一个**显式的标记位** `grants->>'isIT'`，与 0001 用
--   base_template='tpl.it' 的思路同构：靠**声明的属性**判身份，不靠名字猜。
--   默认 false ⇒ 历史行（没有该字段）不受影响，不会因为迁移而突然写不进去。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'ck_dim_group_spark_cicada_it_no_business'
    ) THEN
        ALTER TABLE dim_group
            ADD CONSTRAINT ck_dim_group_spark_cicada_it_no_business
            CHECK (NOT (
                COALESCE(grants->>'isIT', 'false')::boolean
                AND COALESCE(grants->>'canViewBusinessValues', 'false')::boolean
            ));
    END IF;
END $$;

-- 分组授权上限的**数值**约束：maxLevel 必须是四个合法值之一。
--
-- ★ 为什么要单独加：0001 的 grants 是自由 jsonb，可以写进 "maxLevel":"L9"。
--   一旦出现非法密级，求值器对它的处理是「不认识 → 当 L1 还是当 L4」，
--   全凭实现细节 —— 而其中一种选择就是**静默扩权**。
--   在库层钉死取值域，比在下游每个 switch 里兜底可靠。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'ck_dim_group_spark_cicada_max_level'
    ) THEN
        ALTER TABLE dim_group
            ADD CONSTRAINT ck_dim_group_spark_cicada_max_level
            CHECK (
                grants->>'maxLevel' IS NULL
                OR grants->>'maxLevel' IN ('L1','L2','L3','L4')
            );
    END IF;
END $$;

-- ───────────────────────── ② 组授权的按行变更流水（可审计）─────────────────
--
-- ★ 为什么需要它（而不是靠 grants jsonb 的 UPDATE 触发审计）：
--
--   审计要回答的问题是「**谁**在**什么时候**给**哪个组**加了**哪个 module**」。
--   jsonb 整块覆盖只能留下「grants 从 {A,B} 变成 {A}」这样的快照，
--   读审计的人得自己做 diff 才能还原意图 —— 而 diff 在并发修改下并不可靠。
--
--   本表是**派生流水**：应用层在写 dim_group.grants 的同一个事务里，
--   把「本次新增/移除的条目」逐条落进来。它是 append-only 的，
--   与 audit_log 的分工是：audit_log 记「这个组的 grants 变了」，
--   本表记「变的具体是哪几条」。前者粗粒度、后者可检索。
CREATE TABLE IF NOT EXISTS fact_group_grant_change (
    id          bigserial   PRIMARY KEY,
    group_id    text        NOT NULL REFERENCES dim_group(id) ON DELETE CASCADE,
    -- 变更动作：授予 / 收回
    action      text        NOT NULL CHECK (action IN ('GRANT','REVOKE')),
    -- 授权种类，与 GroupGrants 的字段一一对应
    --
    -- ★ 这里用 `level` 而不是 `max_level`，是为了与 group.KindLevel 常量
    --   逐字一致：DB 的 kind 字符串会被应用层直接当枚举用，
    --   两处命名不同就得写一张映射表，而映射表迟早会被漏掉一处。
    kind        text        NOT NULL
                            CHECK (kind IN ('module','dimension','level','data_use_group')),
    -- module:<id> / dimension:<dim> / level:<L1..L4> / data_use_group:<grp.xxx>
    key         text        NOT NULL,
    -- 维度的取值（非维度类为空）；用于回答「收回了哪些渠道」
    values      text[]      NOT NULL DEFAULT '{}',
    -- 谁做的这次变更（管理员账号）
    actor       text        NOT NULL,
    -- 变更原因（支持工单号 / 申请单 id —— 便于把「因哪张申请单而开通」串起来）
    reason      text        NULL,
    at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_group_grant_change_group
    ON fact_group_grant_change(group_id, at DESC);

-- 按 key 检索「这个 module 被授给过哪些组」（追责 / 影响面分析）。
CREATE INDEX IF NOT EXISTS idx_group_grant_change_key
    ON fact_group_grant_change(kind, key);

-- ───────────────────────── ③ 申请单的回收与终态索引 ─────────────────────────
--
-- ★ 复合索引 (status, requested_expiry)：回收任务的 WHERE 正是
--     status = 'APPROVED' AND requested_expiry IS NOT NULL AND requested_expiry <= now()
--   0001 已有的 (status) 单列索引在这种「再加一个范围条件」的查询上，
--   选中率差时会退化成大量回表。复合索引让整个条件走一次索引扫描。
--
--   注意列序：status 在前。因为 status 的区分度低但**选择性在前**能让
--   requested_expiry 的范围扫描只落在 APPROVED 这个子集上 —— 顺序反了就用不上。
CREATE INDEX IF NOT EXISTS idx_request_reclaimable
    ON fact_permission_request(status, requested_expiry)
    WHERE requested_expiry IS NOT NULL;

-- 待办队列：审批人打开「我的待批」时按申请时间倒序取有界页。
CREATE INDEX IF NOT EXISTS idx_request_approving_created
    ON fact_permission_request(status, created_at DESC);

-- ───────────────────────── ④ 申请单的「谁在批」可检索 ─────────────────────────
--
-- ★ 0001 把 approvals 存成 jsonb，这带来一个真实的检索缺口：
--   「列出 u.lead 待批的所有申请」原本只能扫全表。
--
--   这里不引入冗余的审批子表（那会重新制造「两套真相」——审批步骤既在
--   jsonb 又在子表），而是用 **GIN(jsonb_path_ops)**：这正是为
--   containment 查询（`@>`）优化的 jsonb 索引操作符类。
--
--   为什么是 jsonb_path_ops 而不是默认的 jsonb_ops：
--   默认 jsonb_ops 会为 jsonb 的**每个键和每个值**各建一个索引项，
--   索引体积约为 jsonb_path_ops 的 2–3 倍，写入也更慢；
--   而这里只用 `@>` 查询，用不上「键存在性」那类查询。
--   jsonb_path_ops 恰好只支持 `@>` / `@?` / `@@`，是本场景的正解。
--
--   查询写法（应用层）：
--     SELECT ... FROM fact_permission_request
--     WHERE status='APPROVING'
--       AND approvals @> '[{"approver":"u.lead","action":"PENDING"}]';
CREATE INDEX IF NOT EXISTS idx_request_approvals_gin
    ON fact_permission_request USING gin (approvals jsonb_path_ops);

-- ───────────────────────── ⑤ 会签未决的可检索视图 ─────────────────────────
--
-- ★ 会签闸门（F8）在应用层判 cc.decided_at IS NULL。但运维需要一个
--   **只读**入口回答「现在有多少单卡在会签上、卡在谁那里」——
--   这正是事故排查时最先要看的数字。
--
--   做成视图而不是物化表：会签状态变化频繁，物化表的刷新延迟
--   会让运维看到过期数据，反而误导；视图实时、且不需维护。
CREATE OR REPLACE VIEW v_cosign_pending AS
SELECT
    r.id              AS request_id,
    r.applicant,
    r.purpose,
    r.status,
    r.created_at,
    (cc->>'cc')       AS cosigner,
    (cc->>'mode')     AS mode,
    (cc->>'reason')   AS reason
FROM fact_permission_request r
CROSS JOIN LATERAL jsonb_array_elements(r.ccs) AS cc
WHERE r.status = 'COSIGN_PENDING'
  AND (cc->>'mode') = 'cosign'
  -- ★ 仅列出**尚未表态**者 —— 这正是「卡住」的定义。
  --   已表态（decided_at 非空）的不该出现在「待办」里。
  AND (cc->>'decidedAt') IS NULL;

COMMENT ON VIEW v_cosign_pending IS
    'M-REQ 会签待办：status=COSIGN_PENDING 且存在未表态 cosign 抄送人的申请（F8）';

COMMIT;
