-- 0012_tenant_hardening.sql —— 收口多租户：延迟表加固 + dim_org 复合主键 + 跨租户 FK 检测
--
-- ══════════════════════════════════════════════════════════════════════════
-- ★★ 本迁移是「先多租户（商用刚需）」这条线的**收尾**。
--
--   0011 把 P0 业务表/权限表/用户设置/审计加固了，但**刻意**留下一批
--   未处理的表与一项结构性遗留（见 0011 文末「未在本迁移处理的表及原因」）。
--   当时不处理是因为它们需要**独立的语义设计**，不能在 0011 里顺手做掉。
--   本迁移把那些设计定下来并落地。
--
--   三件事：
--     ① 延迟表加固：registry_* / staging_* / collect_* / precomp_build_log
--     ② ★ dim_org 主键 → (tenant_id, account) 复合，并重建 6 条外键
--     ③ v_cross_tenant_fk_violation 检测视图（0011 承诺的巡检工具）
-- ══════════════════════════════════════════════════════════════════════════
--
-- ★★★★ 纪律 0（与 0011 同）：**所有对象名不得带 schema 前缀，一律走 search_path。**
--
--   本迁移会被**两类**执行者跑到：
--     ① 平台库：search_path = public
--     ② 独立档租户：search_path = t_xxx, public（provision 重放全部迁移）
--   若写死 `public.dim_org`，②会去改平台库的表 —— 或者更糟：
--   租户 schema 的表**从未被加固**，而记账显示迁移成功。
--
--   ★ 判断目录（pg_constraint / pg_indexes / information_schema /
--     pg_attribute）时，这些目录是**库级**的、跨 schema 可见，
--     必须显式限定到 current_schema()，否则②会看到①留下的同名对象。
-- ══════════════════════════════════════════════════════════════════════════
--
-- ★★★ 决策记录（本迁移的两个关键设计选择，与用户确认过）：
--
--   决策 A：**延迟表一次性全部加固**（不拆成多轮迁移）。
--     理由：拆轮次会让「共享档能不能上线」这个判断长期悬空；
--     而且 staging_* 若不加租户，共享档部署时会把两家的采集数据混在一起 ——
--     这是**数据污染**，不只是可见性问题。
--
--   决策 B：**registry_* 采用「平台默认 + 租户覆盖」模型**。
--     `tenant_id IS NULL` ⇒ 平台默认配方（所有租户共用）
--     `tenant_id = X`    ⇒ 租户 X 的覆盖配方（仅 X 可见/可用）
--     读取语义：**租户覆盖优先，否则回落平台默认**。
--     ★ 为什么不是「纯共享」也不是「纯私有」：
--       纯共享 ⇒ 一家租户想微调费率口径就得改全平台的配方（不可能）；
--       纯私有 ⇒ 每开一家租户都要复制整套配方（1000 家 ⇒ 配方表爆炸，
--                 且「升级算法版本」要改 1000 处，必然漂移）。
--       「默认 + 覆盖」是唯一能同时满足「开箱可用」与「可定制」的模型。
--     ★ 写入纪律（本迁移用 RLS 的 WITH CHECK 强制）：
--       租户会话**只能**写自己 tenant_id 的行，**不能**写 tenant_id=NULL。
--       NULL 行只由平台/运维通道（特权角色）产生 —— 与 dim_view_template
--       在 0011 的处置完全一致（NULL 不等于「公共」）。
-- ══════════════════════════════════════════════════════════════════════════

BEGIN;

-- ─────────────────────── 前置自检：能不能做复合主键？ ───────────────────────
--
-- ★★★ 这是本迁移最重要的前置判断，必须在任何 DDL 之前做完。
--
--   dim_org 主键要从 (account) 改成 (tenant_id, account)。
--   而 **主键列不能为 NULL** —— 所以库里不能有 tenant_id IS NULL 的 dim_org 行。
--
--   0011 的回填只在「库中已存在 shared 档租户」时才生效；
--   若库中**一个租户都没有**（全新部署、或运维还没建租户），
--   所有 dim_org 行的 tenant_id 都是 NULL ⇒ 本迁移**无法**安全完成主键重建。
--
--   ★ 此时**必须报错中止**，而不是：
--     · 静默跳过主键重建（那会留下「以为改了其实没改」的假象 —— 最坏）；
--     · 给 NULL 行编一个假 uuid（那会让数据「看起来有归属，实际谁也查不到」，
--       而且假 uuid 与任何真实租户都不对应，日后无从追溯）。
--
--   ★ 给运维的**可操作**指引（错误信息里带上具体数字与下一步）：
--       ① 建一个 shared 档租户（见 backend/internal/provision）
--       ② 重跑 0011 的回填段（幂等）
--       ③ 再执行本迁移
DO $$
DECLARE
    cur_schema text := current_schema();
    null_org   bigint := 0;
    n_tenant   bigint := 0;
BEGIN
    -- 只对平台 schema 做这项判断：独立档租户的 schema 是新建的，
    -- 其 dim_org 由该租户自己写入（写路径已带 tenant_id），不应有 NULL。
    -- 但为稳妥，**两类 schema 都检查** —— 有 NULL 就一定不能建复合主键。
    SELECT count(*) INTO null_org FROM dim_org WHERE tenant_id IS NULL;
    SELECT count(*) INTO n_tenant FROM dim_tenant;

    RAISE NOTICE '[0012] schema=% 前置自检：dim_org NULL tenant_id=% 行，dim_tenant=% 个租户',
                 cur_schema, null_org, n_tenant;

    IF null_org > 0 THEN
        RAISE EXCEPTION
            '[0012] 无法重建 dim_org 复合主键：存在 % 行 tenant_id IS NULL。'
            '主键列不允许 NULL。请先：① 建一个 shared 档租户（provision）；'
            '② 重跑 0011 的回填段把存量行归入该租户；③ 再执行本迁移。'
            '（库中当前有 % 个租户）', null_org, n_tenant
            USING HINT = '若这些 NULL 行确实是「无归属的废弃数据」，'
                         '应先由运维显式删除或归户，而不是让迁移替你做决定。';
    END IF;
END $$;

-- ══════════════════════════════════════════════════════════════════════════
-- 第 ① 段：延迟表租户加固
-- ══════════════════════════════════════════════════════════════════════════

-- ───────────────────────────── 非 registry 表：标准隔离 ─────────────────────────────
--
-- staging_* / collect_* / precomp_build_log 的语义都是**单一归属**：
-- 一次采集任务、一批暂存记录、一次预计算构建，都属于**某一个**租户。
-- 它们不需要「平台默认 + 覆盖」，用与 0011 完全相同的策略即可。
--
-- ★ 为什么必须隔离 staging_*（0011 已明确要求，这里再钉一次原因）：
--   staging 是**采集暂存层** —— 数据在入投影层之前先落在这里。
--   若不隔离，共享档下两家的采集数据会在同一张表里交错，
--   而「幂等键去重」是**全局**的 ⇒ 租户 A 的行可能被租户 B 的
--   同键行判为「已存在」而**静默丢弃** —— 这是数据丢失，不是可见性问题。
DO $$
DECLARE
    t text;
    cur_schema text := current_schema();
    -- ★ 这些表一律用标准策略（tenant_id = rls_tenant_id()）。
    tables text[] := ARRAY[
        'staging_record',
        'staging_project_release',
        'staging_ingest_guard',
        'collect_job',
        'collect_page',
        'collect_rate_gear'
    ];
BEGIN
    RAISE NOTICE '[0012] 标准隔离段作用于 schema: %', cur_schema;

    FOREACH t IN ARRAY tables LOOP
        -- ① 加 tenant_id（可空：先让 DDL 通过，回填后再收紧 —— 本段末尾统一处理）
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
             WHERE table_schema = cur_schema AND table_name = t
               AND column_name = 'tenant_id'
        ) THEN
            EXECUTE format('ALTER TABLE %I ADD COLUMN tenant_id uuid', t);
        END IF;

        -- ② 开 RLS + FORCE（与 0011 同：FORCE 让表拥有者也受约束）
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);

        -- ③ 策略：统一入口 rls_tenant_id()，读与写都要求同租户
        EXECUTE format('DROP POLICY IF EXISTS p_tenant_isolation_%s ON %I', t, t);
        EXECUTE format(
            'CREATE POLICY p_tenant_isolation_%s ON %I '
            'USING (tenant_id = rls_tenant_id()) '
            'WITH CHECK (tenant_id = rls_tenant_id())', t, t);

        -- ④ 索引：RLS 查询恒带 tenant_id 条件
        EXECUTE format(
            'CREATE INDEX IF NOT EXISTS idx_%s_tenant ON %I(tenant_id)', t, t);

        RAISE NOTICE '[0012] 已加固表 %', t;
    END LOOP;
END $$;

-- ───────────────────────────── precomp_build_log：标准隔离 + 保留全局审计视图 ─────────────────────────────
--
-- ★ precomp_build_log 稍有不同：它是**预计算构建日志**。
--   算力是共享资源，但「谁的目标被构建了」属于租户信息。
--   故与其它表一样加 tenant_id + 标准策略，不特殊化。
--   （「按租户排队」是调度层的事，本迁移只保证日志归属正确。）
DO $$
DECLARE
    cur_schema text := current_schema();
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
         WHERE table_schema = cur_schema AND table_name = 'precomp_build_log'
           AND column_name = 'tenant_id'
    ) THEN
        ALTER TABLE precomp_build_log ADD COLUMN tenant_id uuid;
    END IF;
    ALTER TABLE precomp_build_log ENABLE ROW LEVEL SECURITY;
    ALTER TABLE precomp_build_log FORCE ROW LEVEL SECURITY;
    DROP POLICY IF EXISTS p_tenant_isolation_precomp_build_log ON precomp_build_log;
    CREATE POLICY p_tenant_isolation_precomp_build_log ON precomp_build_log
        USING (tenant_id = rls_tenant_id())
        WITH CHECK (tenant_id = rls_tenant_id());
    CREATE INDEX IF NOT EXISTS idx_precomp_build_log_tenant
        ON precomp_build_log(tenant_id);
    RAISE NOTICE '[0012] 已加固表 precomp_build_log（schema=%）', cur_schema;
END $$;

-- ───────────────────────────── registry_* 表：平台默认 + 租户覆盖 ─────────────────────────────
--
-- ★★★ 本段是本迁移语义最微妙的一段，务必读懂再改。
--
--   模型：`tenant_id IS NULL` = 平台默认；非 NULL = 租户覆盖。
--   读取语义：**租户覆盖优先，否则回落平台默认**。
--
--   ★ 策略必须同时允许「自己的行」与「平台默认行」可见：
--
--       USING (tenant_id = rls_tenant_id() OR tenant_id IS NULL)
--
--     ★ 但**不能**写成 `tenant_id = rls_tenant_id() OR tenant_id IS NULL`
--       就当作「NULL 等于公共」—— 这两者在**写侧**必须分开：
--       读允许看到 NULL（回落默认），写**禁止**产生 NULL
--       （否则任何租户都能改全平台的配方）。
--
--   ★ 为什么写侧禁用 NULL 这么重要：
--     registry_* 是**配方**（费率口径、槽位定义、算法公式）。
--     若租户会话能写 NULL 行，它就能改**所有租户**看到的默认配方 ——
--     这是横向提权，且不像「读到别人数据」那样容易被发现
--     （症状只是「所有租户的数字都慢慢变了」）。
--     故 WITH CHECK 里**只有** `tenant_id = rls_tenant_id()`，没有 OR IS NULL。
--     NULL 行只能由平台/运维通道（特权角色，不受 RLS 约束）产生。
DO $$
DECLARE
    t text;
    cur_schema text := current_schema();
    tables text[] := ARRAY[
        'registry_slot',
        'registry_algorithm',
        'registry_rule_set',
        'registry_bucket',
        'registry_slot_coverage'
    ];
BEGIN
    RAISE NOTICE '[0012] registry 默认+覆盖段作用于 schema: %', cur_schema;

    FOREACH t IN ARRAY tables LOOP
        -- ① 加 tenant_id
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
             WHERE table_schema = cur_schema AND table_name = t
               AND column_name = 'tenant_id'
        ) THEN
            EXECUTE format('ALTER TABLE %I ADD COLUMN tenant_id uuid', t);
        END IF;

        -- ② 开 RLS + FORCE
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);

        -- ③ 策略：读 = 自己 OR 平台默认；写 = 只能是自己
        EXECUTE format('DROP POLICY IF EXISTS p_tenant_override_%s ON %I', t, t);
        EXECUTE format(
            'CREATE POLICY p_tenant_override_%s ON %I '
            'USING (tenant_id = rls_tenant_id() OR tenant_id IS NULL) '
            'WITH CHECK (tenant_id = rls_tenant_id())', t, t);

        -- ④ 索引：RLS 条件含两个分支，两个都要能走索引
        EXECUTE format(
            'CREATE INDEX IF NOT EXISTS idx_%s_tenant ON %I(tenant_id)', t, t);

        RAISE NOTICE '[0012] 已加固表 %（默认+覆盖策略）', t;
    END LOOP;
END $$;

-- ───────────────────────────── registry 唯一键修正 ─────────────────────────────
--
-- ★★ 与 0011 同一条纪律：加了 tenant_id 之后，原来的**全局唯一键**
--   语义就错了（「全世界只有一条默认配方」应当变成
--   「每租户最多一条覆盖 + 一条平台默认」）。
--
--   不一致的后果很具体：
--     租户 X 想覆盖 slot 's1' ⇒ 插入 (s1, X) 时若还受全局 UNIQUE(id)
--     约束，**插入直接失败**（因为平台默认的 s1 已存在）⇒
--     「租户永远无法定制配方」，而报错指向上游，极难排障。
--
--   ★ 正确语义的键设计：
--     · 平台默认：同一 id 只允许一条 NULL 行
--     · 租户覆盖：同一 (tenant_id, id) 只允许一条
--   两者用**两个部分唯一索引**表达（因为 NULL 不参与普通唯一约束）。
DO $$
DECLARE
    cur_schema text := current_schema();
BEGIN
    RAISE NOTICE '[0012] registry 唯一键修正段作用于 schema: %', cur_schema;

    -- registry_slot：原 PK(id) 全局唯一 → 改为「每租户/平台各一条」
    IF EXISTS (SELECT 1 FROM pg_constraint c
                WHERE c.conname = 'registry_slot_pkey'
                  AND c.connamespace = to_regnamespace(cur_schema)) THEN
        ALTER TABLE registry_slot DROP CONSTRAINT registry_slot_pkey;
    END IF;
    -- 平台默认：id 唯一（限 NULL 行）
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_slot_platform
        ON registry_slot (id) WHERE tenant_id IS NULL;
    -- 租户覆盖：每租户每 id 唯一
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_slot_tenant
        ON registry_slot (tenant_id, id) WHERE tenant_id IS NOT NULL;

    -- registry_algorithm：同 registry_slot
    IF EXISTS (SELECT 1 FROM pg_constraint c
                WHERE c.conname = 'registry_algorithm_pkey'
                  AND c.connamespace = to_regnamespace(cur_schema)) THEN
        ALTER TABLE registry_algorithm DROP CONSTRAINT registry_algorithm_pkey;
    END IF;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_algorithm_platform
        ON registry_algorithm (id) WHERE tenant_id IS NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_algorithm_tenant
        ON registry_algorithm (tenant_id, id) WHERE tenant_id IS NOT NULL;

    -- registry_bucket：同 registry_slot
    IF EXISTS (SELECT 1 FROM pg_constraint c
                WHERE c.conname = 'registry_bucket_pkey'
                  AND c.connamespace = to_regnamespace(cur_schema)) THEN
        ALTER TABLE registry_bucket DROP CONSTRAINT registry_bucket_pkey;
    END IF;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_bucket_platform
        ON registry_bucket (id) WHERE tenant_id IS NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_bucket_tenant
        ON registry_bucket (tenant_id, id) WHERE tenant_id IS NOT NULL;

    -- registry_rule_set：原 PK(id, version) → 加租户维度
    IF EXISTS (SELECT 1 FROM pg_constraint c
                WHERE c.conname = 'registry_rule_set_pkey'
                  AND c.connamespace = to_regnamespace(cur_schema)) THEN
        ALTER TABLE registry_rule_set DROP CONSTRAINT registry_rule_set_pkey;
    END IF;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_rule_set_platform
        ON registry_rule_set (id, version) WHERE tenant_id IS NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_rule_set_tenant
        ON registry_rule_set (tenant_id, id, version) WHERE tenant_id IS NOT NULL;

    -- registry_slot_coverage：原 PK(slot_id, scope) → 加租户维度
    IF EXISTS (SELECT 1 FROM pg_constraint c
                WHERE c.conname = 'registry_slot_coverage_pkey'
                  AND c.connamespace = to_regnamespace(cur_schema)) THEN
        ALTER TABLE registry_slot_coverage DROP CONSTRAINT registry_slot_coverage_pkey;
    END IF;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_slot_coverage_platform
        ON registry_slot_coverage (slot_id, scope) WHERE tenant_id IS NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_registry_slot_coverage_tenant
        ON registry_slot_coverage (tenant_id, slot_id, scope) WHERE tenant_id IS NOT NULL;
END $$;

-- ───────────────────────────── 延迟表存量数据归属 ─────────────────────────────
--
-- ★ 与 0011 的回填同一条纪律：
--   · 只在**平台 schema** 回填（独立档 schema 是新建的，无存量）；
--   · 归到「最早的 shared 档租户」；没有租户则**保持 NULL**（不编 uuid）。
--
-- ★ 但这里有一处与 0011 **相反**的取值，必须说清：
--
--   registry_* 的存量数据是**平台默认配方**（0011 之前没有租户概念，
--   那些配方是全平台共用的）⇒ 回填时**刻意保持 NULL**，
--   因为 NULL 在「默认 + 覆盖」模型里**就是**「平台默认」的表示。
--   若把它们归到某个 shared 租户，就变成了「那个租户的私有覆盖」——
--   其它租户再也看不到默认配方 ⇒ 平台直接不可用。
--
--   staging_* / collect_* / precomp_build_log 的存量数据则是**业务数据**，
--   应当归户（与 0011 一致）。若库中无租户，保持 NULL（RLS 下不可见，
--   符合 fail-closed：无归属的数据谁都不给看）。
DO $$
DECLARE
    default_tenant uuid;
    t text;
    n bigint;
    cur_schema text := current_schema();
    -- 需要回填归户的业务类表
    biz_tables text[] := ARRAY[
        'staging_record','staging_project_release','staging_ingest_guard',
        'collect_job','collect_page','collect_rate_gear','precomp_build_log'
    ];
BEGIN
    IF cur_schema <> 'public' THEN
        RAISE NOTICE '[0012] schema % 非平台 schema —— 跳过存量回填', cur_schema;
        RETURN;
    END IF;

    SELECT id INTO default_tenant
      FROM dim_tenant
     WHERE tier = 'shared'
     ORDER BY created_at
     LIMIT 1;

    IF default_tenant IS NULL THEN
        RAISE NOTICE '[0012] 库中无 shared 档租户 —— 业务类表存量保持 NULL（RLS 下不可见）；'
                     'registry_* 存量保持 NULL（= 平台默认，正确）';
    ELSE
        RAISE NOTICE '[0012] 业务类表存量归入租户 %', default_tenant;
        FOREACH t IN ARRAY biz_tables LOOP
            EXECUTE format('UPDATE %I SET tenant_id = $1 WHERE tenant_id IS NULL', t)
                USING default_tenant;
            GET DIAGNOSTICS n = ROW_COUNT;
            IF n > 0 THEN
                RAISE NOTICE '[0012]   % 回填 % 行', t, n;
            END IF;
        END LOOP;
    END IF;

    -- ★ registry_* 明确**不**回填（保持 NULL = 平台默认）。写一行 NOTICE 存档这个决定。
    RAISE NOTICE '[0012] registry_* 存量刻意保持 tenant_id=NULL（= 平台默认配方，全租户可用）';
END $$;

-- ══════════════════════════════════════════════════════════════════════════
-- 第 ② 段：dim_org 复合主键重建 + 6 条外键重建（★ 本迁移风险最高的一段）
-- ══════════════════════════════════════════════════════════════════════════
--
-- ★★★ 为什么必须做这一步（0011 记录了这项遗留，这里是它的解法）：
--
--   PostgreSQL 在检查外键约束时**以表拥有者权限执行**，**不应用 RLS**。
--   于是存在一条**真实的越权路径**：
--
--     租户 B 的会话 INSERT 一行 dim_group_member，
--     其 account 指向**租户 A 的** dim_org.account ——
--     约束检查查得到那一行（因为 FK 检查绕过 RLS），于是**写入成功**。
--     结果：租户 B 在该行上「引用」了租户 A 的账号。
--
--   ★ 复合外键为什么能根治这个问题（而不只是「巡检发现」）：
--     把 FK 从 `(account) → dim_org(account)` 改成
--     `(tenant_id, account) → dim_org(tenant_id, account)` 后，
--     数据库会**同时**匹配 tenant_id。租户 B 想引用租户 A 的账号时，
--     它自己的 tenant_id 是 B，而父行的 tenant_id 是 A ⇒ **约束层直接拒绝**。
--     这是**结构性**保证，比「事后巡检 + 人工修」强得多。
--
--   ★ 代价（显式记录，不假装不存在）：
--     所有子表都必须有 tenant_id 且**与父行一致**。
--     0011 已给这些子表加了 tenant_id，故条件满足。
--     但**存量行**若 tenant_id 不匹配（例如都还是 NULL），重建会失败 ——
--     由本迁移开头的自检 + 0011 的回填共同保证不会发生。
--
-- ★★★ 重建顺序（顺序错了会因「被依赖的约束仍在外键图里」而失败）：
--     ① 先 DROP 所有指向 dim_org 的 FK（含自引用 supervisor）
--     ② 再换 dim_org 主键
--     ③ 最后重建所有 FK 为复合形态
DO $$
DECLARE
    cur_schema text := current_schema();
    -- ★ 指向 dim_org 的外键清单（真库实测得出，共 6 条 + 1 自引用）。
    --   结构：(子表, 约束名, 本表用于引用的列)
    fks text[][] := ARRAY[
        ['dim_org',              'dim_org_supervisor_fkey',              'supervisor'],
        ['dim_data_chain',       'dim_data_chain_owner_account_fkey',    'owner_account'],
        ['dim_group_member',     'dim_group_member_account_fkey',        'account'],
        ['fact_entitlement',     'fact_entitlement_account_fkey',        'account'],
        ['fact_permission_request','fact_permission_request_applicant_fkey','applicant'],
        ['dim_view_template',    'dim_view_template_owner_fkey',         'owner']
    ];
    i int;
BEGIN
    RAISE NOTICE '[0012] dim_org 复合主键重建段作用于 schema: %', cur_schema;

    -- ★ 幂等判断：若主键已经是复合形态，整段跳过（重复执行本迁移不应报错）
    IF EXISTS (
        SELECT 1 FROM pg_constraint c
         WHERE c.conname = 'dim_org_pkey'
           AND c.connamespace = to_regnamespace(cur_schema)
           AND pg_get_constraintdef(c.oid) LIKE '%tenant_id%'
    ) THEN
        RAISE NOTICE '[0012] dim_org_pkey 已是复合键 —— 跳过主键重建（幂等）';
    ELSE
        -- ① DROP 所有指向 dim_org 的 FK
        FOR i IN 1 .. array_length(fks, 1) LOOP
            IF EXISTS (SELECT 1 FROM pg_constraint c
                        WHERE c.conname = fks[i][2]
                          AND c.connamespace = to_regnamespace(cur_schema)) THEN
                EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I', fks[i][1], fks[i][2]);
                RAISE NOTICE '[0012]   已 DROP FK %.%', fks[i][1], fks[i][2];
            END IF;
        END LOOP;

        -- ② 换主键：DROP 旧的 (account) PK，建复合 (tenant_id, account)
        IF EXISTS (SELECT 1 FROM pg_constraint c
                    WHERE c.conname = 'dim_org_pkey'
                      AND c.connamespace = to_regnamespace(cur_schema)) THEN
            ALTER TABLE dim_org DROP CONSTRAINT dim_org_pkey;
        END IF;
        -- ★ tenant_id 先收紧为 NOT NULL：主键列必须非空。
        --   前置自检已保证没有 NULL 行，故这一步是安全的。
        ALTER TABLE dim_org ALTER COLUMN tenant_id SET NOT NULL;
        ALTER TABLE dim_org ADD CONSTRAINT dim_org_pkey PRIMARY KEY (tenant_id, account);
        RAISE NOTICE '[0012]   dim_org 主键已改为 (tenant_id, account)';

        -- ③ 重建 6 条 FK 为复合形态
        FOR i IN 1 .. array_length(fks, 1) LOOP
            EXECUTE format(
                'ALTER TABLE %I ADD CONSTRAINT %I '
                'FOREIGN KEY (tenant_id, %I) REFERENCES dim_org (tenant_id, account)',
                fks[i][1], fks[i][2], fks[i][3]);
            RAISE NOTICE '[0012]   已重建复合 FK %.% (tenant_id, %)',
                         fks[i][1], fks[i][2], fks[i][3];
        END LOOP;

        RAISE NOTICE '[0012] ★ dim_org 复合键重建完成 —— 跨租户引用现已在**约束层**被拒绝';
    END IF;
END $$;

-- ★★ dim_org 重建后，0011 加的 (tenant_id, account) 唯一索引已冗余
--    （复合主键本身即唯一约束）。保留它浪费一次写入维护成本，故删除。
--    ★ 但要注意：0011 的索引名是 uq_dim_org_tenant_account（若存在）。
DROP INDEX IF EXISTS uq_dim_org_tenant_account;

-- ══════════════════════════════════════════════════════════════════════════
-- 第 ③ 段：v_cross_tenant_fk_violation —— 跨租户引用巡检视图
-- ══════════════════════════════════════════════════════════════════════════
--
-- ★ 0011 承诺了本视图，这里兑付。
--
-- ★ 既然复合 FK 已在**约束层**阻断新的跨租户引用，为什么还要视图？
--   三个用途，缺一不可：
--     ① **巡检历史遗留**：复合 FK 是在本迁移之后才生效的。
--        在此之前写入的（或从旧备份恢复的）违规行仍可能存在 ——
--        约束只约束未来，不追溯过去。视图让我们能**发现**它们。
--     ② **覆盖独立档 schema**：独立档租户靠 search_path 物理隔离，
--        不一定开 RLS。复合 FK 仍生效，但运维需要统一的巡检入口。
--     ③ **审计与取证**：当客户问「能否证明没有跨租户引用」时，
--        这个视图就是可执行的证据（返回 0 行即为证明）。
--
-- ★ 视图的列：child_table / child_key / child_tenant / parent_table /
--   parent_tenant / violation —— 便于直接定位到具体违规行。
CREATE OR REPLACE VIEW v_cross_tenant_fk_violation AS
-- dim_org.supervisor → dim_org.account（自引用）
SELECT 'dim_org.supervisor'::text AS child_ref,
       c.account                  AS child_key,
       c.tenant_id                AS child_tenant,
       p.account                  AS parent_key,
       p.tenant_id                AS parent_tenant
  FROM dim_org c
  JOIN dim_org p ON p.account = c.supervisor
 WHERE c.tenant_id IS DISTINCT FROM p.tenant_id
UNION ALL
-- dim_data_chain.owner_account → dim_org.account
SELECT 'dim_data_chain.owner_account',
       c.owner_account, c.tenant_id, p.account, p.tenant_id
  FROM dim_data_chain c
  JOIN dim_org p ON p.account = c.owner_account
 WHERE c.tenant_id IS DISTINCT FROM p.tenant_id
UNION ALL
-- dim_group_member.account → dim_org.account
SELECT 'dim_group_member.account',
       c.account, c.tenant_id, p.account, p.tenant_id
  FROM dim_group_member c
  JOIN dim_org p ON p.account = c.account
 WHERE c.tenant_id IS DISTINCT FROM p.tenant_id
UNION ALL
-- fact_entitlement.account → dim_org.account
SELECT 'fact_entitlement.account',
       c.account, c.tenant_id, p.account, p.tenant_id
  FROM fact_entitlement c
  JOIN dim_org p ON p.account = c.account
 WHERE c.tenant_id IS DISTINCT FROM p.tenant_id
UNION ALL
-- fact_permission_request.applicant → dim_org.account
SELECT 'fact_permission_request.applicant',
       c.applicant, c.tenant_id, p.account, p.tenant_id
  FROM fact_permission_request c
  JOIN dim_org p ON p.account = c.applicant
 WHERE c.tenant_id IS DISTINCT FROM p.tenant_id
UNION ALL
-- dim_view_template.owner → dim_org.account
SELECT 'dim_view_template.owner',
       c.owner, c.tenant_id, p.account, p.tenant_id
  FROM dim_view_template c
  JOIN dim_org p ON p.account = c.owner
 WHERE c.tenant_id IS DISTINCT FROM p.tenant_id;

COMMENT ON VIEW v_cross_tenant_fk_violation IS
    '跨租户外键引用巡检视图：返回所有「子行与父行租户不一致」的引用。'
    '共享档上线前与定期巡检都必须返回 0 行；非 0 表示存在跨租户引用（越权或历史遗留）。';

COMMIT;

-- ══════════════════════════════════════════════════════════════════════════
-- 部署自检（上线后必须执行）
-- ══════════════════════════════════════════════════════════════════════════
--
--  ① ★★ 确认**没有**跨租户引用（本迁移的核心目标）：
--
--     SELECT count(*) FROM v_cross_tenant_fk_violation;
--
--     ★ 必须为 0。非 0 说明仍有跨租户引用（历史遗留或本迁移未覆盖的路径），
--       必须逐行排查后修复 —— 这是客户合规审计的第一个问题。
--
--  ② 确认 dim_org 主键已是复合形态：
--
--     SELECT pg_get_constraintdef(oid) FROM pg_constraint
--      WHERE conname = 'dim_org_pkey';
--     -- 期望：PRIMARY KEY (tenant_id, account)
--
--  ③ 确认指向 dim_org 的外键**全是**复合形态（含 tenant_id）：
--
--     SELECT conrelid::regclass, conname, pg_get_constraintdef(oid)
--       FROM pg_constraint
--      WHERE contype = 'f' AND confrelid = 'dim_org'::regclass;
--     -- ★ 每条 def 都必须含 (tenant_id, ...) REFERENCES dim_org(tenant_id, account)
--     --   若有一条不含 tenant_id，说明本迁移的 FK 重建段没跑成功 —— 立即排查。
--
--  ④ 确认所有延迟表都已加固（rls_on / forced / 策略数）：
--
--     SELECT c.relname, c.relrowsecurity AS rls_on,
--            c.relforcerowsecurity AS forced,
--            (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid) AS policies
--       FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
--      WHERE n.nspname = 'public' AND c.relkind = 'r'
--        AND c.relname IN (
--          'registry_slot','registry_algorithm','registry_rule_set',
--          'registry_bucket','registry_slot_coverage','staging_record',
--          'staging_project_release','staging_ingest_guard','collect_job',
--          'collect_page','collect_rate_gear','precomp_build_log')
--      ORDER BY c.relname;
--
--     ★ 每行必须 rls_on=true AND forced=true AND policies>=1。
--
--  ⑤ ★ 确认 registry_* 的策略是「默认+覆盖」而非普通隔离：
--
--     SELECT polname, pg_get_expr(polqual, polrelid) AS using_expr,
--            pg_get_expr(polwithcheck, polrelid) AS check_expr
--       FROM pg_policy WHERE polname LIKE 'p_tenant_override_%';
--
--     ★ using_expr 必须含 `OR tenant_id IS NULL`（读可回落默认），
--       check_expr **必须不含** `OR`（写只能写自己 —— 防止改全平台配方）。
--       这一条是横向提权的防线，务必逐条核对。
--
--  ⑥ 确认连接角色非特权（同 0011 的 ②，此处再强调）：
--
--     SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user;
--     -- 必须 rolsuper=false AND rolbypassrls=false
