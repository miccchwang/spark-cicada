-- 0011_rls.sql —— 行级隔离：给业务表加 tenant_id + RLS 策略
--
-- ══════════════════════════════════════════════════════════════════════════
-- ★★ 本迁移把「行级隔离」真正落到业务表上。
--
--   0010 建好了租户注册表与 rls_tenant_id() 统一入口；
--   本迁移负责：① 给该隔离的表加 tenant_id ② 开 RLS + FORCE
--              ③ 建策略 ④ 修被 tenant_id 破坏的唯一键
-- ══════════════════════════════════════════════════════════════════════════
--
-- ★★★★ 纪律 0（最容易翻车、且翻车后表现为「迁移成功但没加固」）：
--
--   **本迁移所有对象名不得带 schema 前缀，一律走 search_path。**
--
--   原因：本迁移会被**两类**执行者跑到 ——
--     ① 平台库：search_path = public    ⇒ 加固共享档业务表；
--     ② 独立档租户：search_path = t_xxx, public ⇒ 加固该租户自己 schema 里的表。
--   （见 backend/internal/provision：开通独立档租户时会把**全部**迁移
--     在租户 schema 内重放一遍，并各自记一份 schema_migrations。）
--
--   若这里写死 `public.fact_sales_daily`：
--     · ② 会去改**平台库**的表（而不是租户 schema 的表）；
--     · 平台库的表在 ① 已改过 ⇒ 报错「constraint does not exist」，
--       或（加了 IF EXISTS 后）静默跳过；
--     · 结果：**租户 schema 里的表从未被加固**，而记账显示迁移成功。
--   ⇒ 独立档租户的表没有 RLS（看似无所谓，因为它靠 search_path 隔离），
--     但 `tenant_id` 列也不存在 ⇒ 后续依赖该列的平台逻辑全部报错。
--
--   ★ 判断目录（pg_constraint / pg_indexes / information_schema）时，
--     这些目录是**库级**的、跨 schema 可见，必须显式限定到 current_schema()，
--     否则 ② 会看到 ① 留下的同名对象而做出错误决定。
--
--   ★ 本文件由测试 backend/internal/tenant/rls_escalation_test.go 与
--     backend/internal/provision/provision_integration_test.go 共同看护：
--     前者要求 12 张表在 public 上确实加固；后者要求独立档 schema
--     能完整重放全部迁移而不报错。
-- ══════════════════════════════════════════════════════════════════════════
--
-- ★★★ 三条必须理解清楚的纪律（写错任何一条都会导致越权）：
--
--   ① **FORCE ROW LEVEL SECURITY 是必须的**
--      Postgres 默认让**表拥有者绕过** RLS。不加 FORCE，以 owner 身份
--      连库时策略完全不生效 —— 而「以 owner 身份连库」是本地与很多
--      托管环境的常态。加了 FORCE 才让 owner 也受约束。
--
--      ★★ 但 FORCE **不能**解决「超级用户绕过」：
--         超级用户与带 BYPASSRLS 的角色**无条件绕过 RLS**。
--         这是本仓库真实踩过的坑（详见 backend/internal/tenant/rls_capability.go）：
--         策略存在、ENABLE 在、FORCE 在，查询却看到全部行。
--         故本迁移末尾附**部署自检**，运行时也由 AssertRLSCapable 兜底。
--
--   ② **策略必须经 rls_tenant_id() 读租户，不得直接 current_setting**
--      直接读会让「缺失时怎么办」散落成多个答案：有的表容错、有的严格，
--      错位本身就是越权。统一入口保证只有一个答案。
--
--   ③ **加 tenant_id 会打破原来的全局唯一键**
--      原来的主键/唯一约束是**全局**的（如 account 是主键），
--      含义是「全世界只有一个 account」。加了租户维度后，
--      正确语义是「**租户内**唯一」—— 即两租户可各有自己的 admin 账号。
--      必须把唯一约束改成 (tenant_id, ...) 复合，否则：
--        · 租户 A 建了 admin ⇒ 租户 B 建 admin 会**冲突失败**
--        （表现为「B 无法创建账号」，且报错指向上游，极难排障）
--
-- ★ 分阶段策略（为什么本迁移不一次改完所有表）：
--   本迁移处理 **P0 数据表 + P0 权限表**（错了会串租或提权）。
--   剩余表（registry_* 注册表、staging_* 采集暂存、precomp_* 预计算）
--   在 0012 处理 —— 它们要么是「平台级共享配置」，
--   要么需要先完成租户维度的语义设计（见文末说明）。

BEGIN;

-- ───────────────────────────── 辅助：批量加 tenant_id 并开 RLS ─────────────────────────────
--
-- ★ 用 DO 块而不是逐条重复写：24 张表若各写一遍，
--   日后新增表时极易漏掉「开 RLS」这一步，而漏掉的表现是**静默放行**。
--   这里把「加列 + 开 RLS + FORCE + 建策略」固化成一个可复用的过程。

DO $$
DECLARE
    -- 需要隔离的表（表名 → 是否有历史行）
    t text;
    -- ★ current_schema() 决定本迁移这次作用在哪个 schema：
    --   平台库 ⇒ public；独立档租户 ⇒ t_xxx。见文件头「不带 schema 前缀」的纪律。
    cur_schema text := current_schema();
    tables text[] := ARRAY[
        -- P0 业务数据
        'fact_sales_daily',
        'bucket_pnl_month',
        -- P0 权限与组织
        'dim_org',
        'dim_data_chain',
        'dim_group',
        'dim_group_member',
        'fact_entitlement',
        'fact_permission_request',
        'fact_group_grant_change',
        -- P0 用户设置
        'dim_view_template',
        'dim_view_template_share',
        -- P0 审计
        'audit_log'
    ];
BEGIN
    RAISE NOTICE '[0011] 行级隔离段作用于 schema: %', cur_schema;

    FOREACH t IN ARRAY tables LOOP
        -- ① 加 tenant_id（可空，因为首租户的存量数据需要回填）
        --
        -- ★ 刻意先加**可空**列：若一上来就 NOT NULL，
        --   存量数据（0001-0010 建的）会因无 tenant_id 而无法通过。
        --   回填完成后（见下方 backfill）才加 NOT NULL。
        --
        -- ★ 这里的 table_schema 判断必须用 cur_schema（不是写死 public），
        --   否则独立档 schema 下的表会被误判为「已有 tenant_id」而跳过。
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
             WHERE table_schema = cur_schema AND table_name = t
               AND column_name = 'tenant_id'
        ) THEN
            EXECUTE format('ALTER TABLE %I ADD COLUMN tenant_id uuid', t);
        END IF;

        -- ② 开 RLS + FORCE（幂等）
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        -- ★ FORCE：让表拥有者也受策略约束。见文件头 ① 的说明。
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);

        -- ③ 策略：读走统一入口 rls_tenant_id()
        --
        -- ★ 先 DROP 再 CREATE 而非 IF NOT EXISTS：
        --   Postgres 没有 CREATE POLICY IF NOT EXISTS，而 DROP+CREATE 幂等。
        EXECUTE format('DROP POLICY IF EXISTS p_tenant_isolation_%s ON %I', t, t);
        EXECUTE format(
            'CREATE POLICY p_tenant_isolation_%s ON %I '
            'USING (tenant_id = rls_tenant_id()) '
            'WITH CHECK (tenant_id = rls_tenant_id())', t, t);

        -- ④ 索引：所有 RLS 查询都会带 tenant_id 条件，
        --    没有索引会让共享档下每次查询全表扫描。
        EXECUTE format(
            'CREATE INDEX IF NOT EXISTS idx_%s_tenant ON %I(tenant_id)', t, t);

        RAISE NOTICE '[0011] 已隔离表 %', t;
    END LOOP;
END $$;

-- ───────────────────────────── 回填存量数据到「默认租户」 ─────────────────────────────
--
-- ★ 0001-0010 是单租户假设下的产物，存量数据没有 tenant_id。
--   为了让既有部署能平滑升级，把存量数据归到**一个确定的租户**下。
--
-- ★ 用什么作为默认租户（这个选择很重要）：
--   取 dim_tenant 中**最早创建的 shared 档租户**。
--   若库里压根没有租户记录，则**不改动任何数据**（tenant_id 留 NULL）
--   —— 留 NULL 而不是编一个 uuid：编出来的 uuid 不指向任何真实租户，
--   那会让数据「看起来有归属，实际谁也查不到」（比 NULL 更糟）。
--
-- ★ NULL tenant_id 的行为：RLS 策略 `tenant_id = rls_tenant_id()`
--   对 NULL 行求值为 NULL ⇒ **行被过滤掉**（不可见）。
--   这是安全的默认：没有归属的数据，谁都不给看。
--   管理员需要它时，先创建租户再执行本迁移的回填。
DO $$
DECLARE
    default_tenant uuid;
    t text;
    n bigint;
    cur_schema text := current_schema();
    tables text[] := ARRAY[
        'fact_sales_daily','bucket_pnl_month','dim_org','dim_data_chain',
        'dim_group','dim_group_member','fact_entitlement','fact_permission_request',
        'fact_group_grant_change','dim_view_template','dim_view_template_share','audit_log'
    ];
BEGIN
    -- ★★ 回填只对**平台 schema** 有意义。
    --
    --   理由：存量数据（0001-0010 单租户时代产物）只存在于平台 schema。
    --   独立档租户的 schema 是**新建的空 schema**，里面没有存量数据，
    --   若仍执行回填，会把「空表」也标上某个 shared 租户的 uuid ——
    --   语义错误（独立档的数据属于该租户，不属于某个共享租户），
    --   且日后排查时会看到一堆莫名其妙的归属。
    --   故此处显式跳过非平台 schema（用 'public' 判定，与 0010 的约定一致）。
    IF cur_schema <> 'public' THEN
        RAISE NOTICE '[0011] schema % 非平台 schema —— 跳过存量回填（新建 schema 无存量）', cur_schema;
        RETURN;
    END IF;

    SELECT id INTO default_tenant
      FROM dim_tenant
     WHERE tier = 'shared'
     ORDER BY created_at
     LIMIT 1;

    IF default_tenant IS NULL THEN
        RAISE NOTICE '[0011] 库中无 shared 档租户 —— 跳过存量回填（tenant_id 保留 NULL，RLS 下不可见）';
        RETURN;
    END IF;

    RAISE NOTICE '[0011] 存量数据将归入租户 %', default_tenant;
    FOREACH t IN ARRAY tables LOOP
        EXECUTE format('UPDATE %I SET tenant_id = $1 WHERE tenant_id IS NULL', t)
            USING default_tenant;
        GET DIAGNOSTICS n = ROW_COUNT;
        IF n > 0 THEN
            RAISE NOTICE '[0011]   % 回填 % 行', t, n;
        END IF;
    END LOOP;
END $$;

-- ───────────────────────────── 修被 tenant_id 破坏的唯一键 ─────────────────────────────
--
-- ★★ 这是本迁移最容易出错、后果最严重的一段。
--
--   原约束是**全局唯一**（「全世界只有一个 X」）。
--   加租户维度后正确语义是**租户内唯一**（「每个租户各有一个 X」）。
--   若不改，第二家租户会**创建失败** —— 而这在生产上表现为
--   「新客户开不了账号」，报错却指向主键冲突（看起来像 bug 而非设计问题）。
--
-- ★ union 唯一键也必须一起改（如 dim_group_member 的 (group_id, account)）。
--
-- ★★★ 纪律：本段所有对象名**必须不带 schema 前缀**。
--
--   本迁移会被**两类**执行者跑到：
--     ① 平台库（search_path = public）—— 迁移共享档业务表；
--     ② 每个独立档租户的 schema（search_path = t_xxx, public）—— 迁移租户表。
--   若写死 `public.fact_sales_daily`，②就会去改**平台库**的表：
--     · 该表在 ① 已被改过一次 ⇒ 报错「constraint does not exist」；
--     · 更糟：租户 schema 里的表**根本没被加固**，而记账却显示迁移成功。
--   这与 0001-0010 全部使用**非限定名**的约定一致（见 grep public. 的结果）。
--
-- ★ pg_constraint / pg_indexes 是**库级**目录，跨 schema 可见，
--   故判断「约束是否存在」时必须同时限定 schema：
--     c.connamespace = to_regnamespace(current_schema())
--   否则②会看到 public 的约束（名字相同）而去做一次注定失败的操作。
DO $$
DECLARE
    cur_schema text := current_schema();
BEGIN
    RAISE NOTICE '[0011] 唯一键修正段作用于 schema: %', cur_schema;

    -- fact_sales_daily：业务键改为 (tenant_id, ...)
    IF EXISTS (SELECT 1 FROM pg_constraint c
                WHERE c.conname = 'uq_fact_sales_daily_spark_cicada_bizkey'
                  AND c.connamespace = to_regnamespace(cur_schema)) THEN
        ALTER TABLE fact_sales_daily
            DROP CONSTRAINT uq_fact_sales_daily_spark_cicada_bizkey;
    END IF;
    -- ★ 用**部分唯一索引**（WHERE tenant_id IS NOT NULL）而不是普通唯一约束：
    --   存量未回填的行 tenant_id 为 NULL，而 SQL 里 NULL != NULL，
    --   普通唯一约束对多行 NULL 是允许的（不会误拦），
    --   但为语义清晰仍显式排除 NULL 行。
    CREATE UNIQUE INDEX IF NOT EXISTS uq_fact_sales_daily_tenant_bizkey
        ON fact_sales_daily
           (tenant_id, stat_date, channel_code, shop_id, spu, sku)
        WHERE tenant_id IS NOT NULL;

    -- dim_data_chain：scope 全局唯一 → 租户内唯一
    IF EXISTS (SELECT 1 FROM pg_constraint c
                WHERE c.conname = 'uq_dim_data_chain_spark_cicada_scope'
                  AND c.connamespace = to_regnamespace(cur_schema)) THEN
        ALTER TABLE dim_data_chain
            DROP CONSTRAINT uq_dim_data_chain_spark_cicada_scope;
    END IF;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_dim_data_chain_tenant_scope
        ON dim_data_chain (tenant_id, scope)
        WHERE tenant_id IS NOT NULL;

    -- ★★ bucket_pnl_month：预计算成品的颗粒唯一键也必须是「租户内唯一」
    --
    --   原键 (month, channel_code, shop_id, brand) 是**全局**的。
    --   后果与 fact_sales_daily 完全一样、且更隐蔽：
    --   两家租户只要同月、同渠道、同店、同品牌，第二家的预计算就会
    --   **整批写入失败** —— 而报错指向唯一键冲突，看起来像「数据重复」，
    --   实际是「本表还没做租户维度」。
    --   ★ 0010 的注释已经写明「配方共享、成品私有」，
    --     成品私有就必须体现在唯一键上，否则私有只是名义上的。
    IF EXISTS (SELECT 1 FROM pg_constraint c
                WHERE c.conname = 'uq_bucket_pnl_month_spark_cicada_grain'
                  AND c.connamespace = to_regnamespace(cur_schema)) THEN
        ALTER TABLE bucket_pnl_month
            DROP CONSTRAINT uq_bucket_pnl_month_spark_cicada_grain;
    END IF;
    DROP INDEX IF EXISTS uq_bucket_pnl_month_spark_cicada_grain;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_bucket_pnl_month_tenant_grain
        ON bucket_pnl_month (tenant_id, month, channel_code, shop_id, brand)
        WHERE tenant_id IS NOT NULL;

    -- ★★ fact_entitlement：主键是 account（全局）⇒ 租户内唯一
    --
    --   这是**权限表**，出错后果是提权或跨租户可见：
    --   若保留 account 作全局主键，则租户 B 无法为自己的 admin 建授权
    --   （「新客户开不了账号」），或者更糟 —— 若有人用 upsert，
    --   会把租户 A 的授权行**改写到租户 B 名下**。
    --
    --   ★ 本表主键被**替换**为复合 (tenant_id, account)：
    --     与 dim_org 不同，fact_entitlement.account 只被引用逻辑上
    --     （代码按 account 查），**没有其它表的外键指向本表主键**，
    --     所以换主键是安全的（见下方 FK 图核对）。
    IF EXISTS (SELECT 1 FROM pg_constraint c
                WHERE c.conname = 'fact_entitlement_pkey'
                  AND c.connamespace = to_regnamespace(cur_schema)) THEN
        ALTER TABLE fact_entitlement DROP CONSTRAINT fact_entitlement_pkey;
    END IF;
    -- ★ 用部分唯一索引而不是复合主键：
    --   tenant_id 可空 ⇒ 不能进主键（主键列必须 NOT NULL）。
    --   而存量行 tenant_id 为空，用部分索引即可跳过它们。
    CREATE UNIQUE INDEX IF NOT EXISTS uq_fact_entitlement_tenant_account
        ON fact_entitlement (tenant_id, account)
        WHERE tenant_id IS NOT NULL;

    -- ★ dim_group_member：主键 (group_id, account) 是全局的。
    --   group_id 本身已带租户语义（dim_group.id 是随机 id），
    --   但为一致性仍补 (tenant_id, group_id, account)。
    CREATE UNIQUE INDEX IF NOT EXISTS uq_dim_group_member_tenant
        ON dim_group_member (tenant_id, group_id, account)
        WHERE tenant_id IS NOT NULL;

    -- ★ dim_view_template_share：主键 (template_id, subject_kind, subject_id)
    --   template_id 已带租户语义，同 dim_group_member 处理。
    CREATE UNIQUE INDEX IF NOT EXISTS uq_dim_view_template_share_tenant
        ON dim_view_template_share (tenant_id, template_id, subject_kind, subject_id)
        WHERE tenant_id IS NOT NULL;

    -- dim_org：account 原是主键（全局唯一）→ 需改为 (tenant_id, account) 唯一
    --
    -- ★ 为什么不能只加唯一索引而保留 PK：
    --   因为 dim_org.account 被**多张表外键引用**
    --   （fact_entitlement / dim_group_member / dim_view_template.owner 等）。
    --   换主键涉及所有外键，属于高风险结构变更。
    --   本迁移采取**保守路线**：保留 account 作为主键（即全局唯一），
    --   但**额外**加 (tenant_id, account) 唯一索引以支持租户内查询。
    --   ★ 代价（必须显式记录，不能假装不存在）：
    --     共享档下「不同租户不能有同名 account」这个限制**仍然存在**。
    --     缓解措施：开通共享档租户时，账号名加租户前缀（见 provision 的约定）。
    --     彻底方案（account 改为 (tenant_id, account) 复合主键）需要重建
    --     整张外键图，属于独立的结构迁移，不在本迁移冒险。
    RAISE NOTICE '[0011] dim_org 保留全局 account 主键（外键图依赖）；'
                 '共享档账号需加租户前缀以避免跨租户重名';
    RAISE NOTICE '[0011] 残留风险：FK 检查绕过 RLS ⇒ 可跨租户引用 dim_org.account。'
                 '缓解：写路径经 tenant 包 + 应用层校验；0012 增可检测违规视图。';
END $$;

-- ───────────────────────────── 审计表：租户可查但跨租户不可见 ─────────────────────────────
--
-- ★ 审计的租户隔离有一个**特殊要求**：
--   audit_log 是 append-only（0001 的触发器拦 UPDATE/DELETE）。
--   而 RLS 的 WITH CHECK 只在 INSERT/UPDATE 时生效 —— INSERT 仍可用。
--   故审计表的策略只需 USING（读过滤），WITH CHECK 对新行要求 tenant_id 正确。
--
-- ★ 但这里有个**顺序问题**：接口层写审计时可能还没确定租户
--   （如登录失败审计）。此时 tenant_id 为 NULL ⇒ RLS 下不可见。
--   这是可接受的：跨租户的安全事件（如扫描探测）本就该进平台级审计，
--   而不是某个租户的审计流。见文末说明。
DO $$
BEGIN
    RAISE NOTICE '[0011] audit_log 已开 RLS：租户只能看到自己的审计行；'
                 'tenant_id IS NULL 的行（平台级事件）对所有租户不可见';
END $$;

COMMIT;

-- ══════════════════════════════════════════════════════════════════════════
-- ★ 部署自检（上线后必须执行）
-- ══════════════════════════════════════════════════════════════════════════
--
--  ① 确认所有该隔离的表都开了 RLS 且策略指向统一入口：
--
--     SELECT c.relname, c.relrowsecurity AS rls_on, c.relforcerowsecurity AS forced,
--            (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid) AS policies
--       FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
--      WHERE n.nspname = 'public' AND c.relkind = 'r'
--        AND c.relrowsecurity = true
--      ORDER BY c.relname;
--
--     ★ 每行必须 rls_on=true AND forced=true AND policies>=1。
--
--  ② ★★ 确认连接角色**不是**超级用户、也**没有** BYPASSRLS：
--
--     SELECT rolname, rolsuper, rolbypassrls
--       FROM pg_roles WHERE rolname = current_user;
--
--     ★ 若 rolsuper=true 或 rolbypassrls=true，
--       上面 ① 里的 RLS **一行都不会过滤** —— 隔离形同虚设。
--       改用 NOSUPERUSER NOBYPASSRLS 的应用角色（见 sql/ops/0010a_app_role.sql）。
--       运行时由 backend/internal/tenant.AssertRLSCapable 兜底拒绝。
--
--  ③ 确认存量数据归属：
--
--     SELECT 'fact_sales_daily 未归属行数 = ' || count(*)
--       FROM fact_sales_daily WHERE tenant_id IS NULL;
--
--     ★ NULL 行在 RLS 下不可见。若数量大且业务需要它可见，
--       先建租户，再重跑本迁移的回填段（幂等）。
--
--  ④ ★★ 确认**没有残留的全局唯一键**（本迁移最容易漏、后果最脏的一步）：
--
--     SELECT tablename, indexname, indexdef
--       FROM pg_indexes
--      WHERE schemaname = 'public'
--        AND indexdef LIKE '%UNIQUE%'
--        AND indexdef NOT LIKE '%tenant_id%'
--        AND tablename IN (
--          'fact_sales_daily','bucket_pnl_month','dim_org','dim_data_chain',
--          'dim_group','dim_group_member','fact_entitlement',
--          'fact_permission_request','fact_group_grant_change',
--          'dim_view_template','dim_view_template_share','audit_log')
--      ORDER BY tablename;
--
--     ★ 允许留下的只有：
--       · *_pkey on id（bigint 自增代理键 —— 全局唯一是正确的）
--       · dim_org_pkey on account（见下方「已知代价」）
--       · dim_group_pkey on id / dim_view_template_pkey on id（随机 id）
--       · uq_view_template_default_per_owner_page（scope/owner/page 是
--         账号级语义，账号已按租户隔离 —— 可接受）
--     ★ 若出现 bucket_pnl_month / fact_entitlement 上的全局唯一键，
--       说明本迁移的唯一键修正段没跑成功 —— 立即排查。
--
--  ⑤ ★★ 确认**外键不会绕过 RLS 造成跨租户引用**（共享档的真实残余风险）：
--
--     ★ PostgreSQL 在检查外键约束时**以表拥有者权限执行**，
--       **不应用 RLS**。因此存在这样一条真实路径：
--         租户 B 可以 INSERT 一行 dim_group_member，
--         其 account 指向**租户 A 的** dim_org.account —— 约束检查
--         查得到那一行（因为 FK 检查绕过 RLS），于是写入成功。
--       结果：租户 B 在该行上「引用」了租户 A 的账号。
--
--     ★ 缓解（本迁移**不**冒险重建 FK 图，改为运行时约束）：
--       · 写路径必须经 tenant 包（InTenantTx），且应用层校验被引用
--         账号属于本租户（共享档下账号名前缀已区分租户）。
--       · 0012 将把「跨租户引用」做成显式的可检测违规视图（见文末）。
--
--     核对该 FK 图（本迁移已核对，结论如注释所述）：
--
--     SELECT conrelid::regclass AS child, conname,
--            confrelid::regclass AS parent, pg_get_constraintdef(oid)
--       FROM pg_constraint
--      WHERE contype = 'f'
--        AND confrelid::regclass::text IN ('dim_org','dim_group','dim_view_template')
--      ORDER BY 1;
--
--  ⑥ 确认**表拥有者与连接角色都不是特权角色**（否则 ① 全部无效）：
--
--     SELECT c.relname, pg_get_userbyid(c.relowner) AS owner
--       FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
--      WHERE n.nspname = 'public' AND c.relrowsecurity
--        AND pg_get_userbyid(c.relowner) <> 'spark_app';
--
--     ★ owner 是超级用户也无妨 —— 因为加了 FORCE（FORCE 解决 owner 绕过）。
--       真正的硬伤是**连接角色**是特权角色（见 ②）。
--       本查询用于留档：「这些表的 owner 是谁」。
--
-- ══════════════════════════════════════════════════════════════════════════
-- ★ 未在本迁移处理的表及原因（留给 0012）
-- ══════════════════════════════════════════════════════════════════════════
--
--   registry_slot / registry_algorithm / registry_rule_set / registry_bucket
--   registry_slot_coverage
--     → **平台级共享配置**（费率口径、槽位定义）。所有租户共用同一套
--       算法与规则定义。按租户隔离会让「升级算法版本」需要改 N 处。
--       ★ 但注意：bucket_pnl_month（预计算结果）是**每租户不同**的，
--         已在本迁移隔离。即「配方共享、成品私有」。
--
--   staging_record / staging_page / staging_project_release /
--   staging_ingest_guard / collect_job / collect_page / collect_rate_gear
--     → 采集暂存层。租户维度需要与「数据链责任人」的语义对齐
--       （谁负责采哪家的数据），属于独立设计，不在本迁移冒险。
--       当前状态：**单租户部署下可用；共享档部署前必须先完成 0012。
--
--   precomp_build_log / bucket_pnl_month 之外的桶
--     → 预计算日志。算力是共享资源，但构建目标分租户。
--       需要「按租户排队」的设计，属独立任务。
--
--   ★ 另有一项**结构性**遗留（0012 必须处理）：
--     dim_org.account 仍是全局主键，且 FK 检查绕过 RLS。
--     彻底方案需重建整张外键图为 (tenant_id, account) 复合键。
--     在此之前，共享档部署必须满足：
--       ① 账号名带租户前缀（避免主键冲突）
--       ② 写路径一律经 tenant 包（避免跨租户引用）
--     0012 将提供：
--       · v_cross_tenant_fk_violation —— 可巡检的跨租户引用违规视图
--       · dim_org 复合主键迁移（含 FK 图重建脚本）
