-- 0010_tenant.sql —— 多租户注册表与隔离骨架
--
-- ══════════════════════════════════════════════════════════════════════════
-- ★★ 本迁移解决的是商业化的**第一性问题**：
--    「一家公司改了一个数字，绝不能让另一家公司看见。」
--
--    它不是「给业务表加一列 tenant_id」那么简单。三个子问题：
--      ① 身份：这次请求属于哪个租户？（→ dim_tenant + 解析器）
--      ② 存储：数据物理上怎么摆？（→ 混合档：shared / dedicated）
--      ③ 证明：怎么**证明**隔离成立？（→ RLS + 越权测试）
--    本迁移负责 ②③ 的骨架；① 由 backend/internal/tenant 负责。
-- ══════════════════════════════════════════════════════════════════════════
--
-- ★ 为什么是**混合档**（两档并存）而不是单一策略：
--
--   纯「每租户独立 schema」是自伤 —— 本仓库当前迁移已创建
--   24 表 + 5 视图 + 32 索引 + 1 函数 + 1 触发器（≈63 个 schema 对象）。
--   1000 家租户 ⇒ 63,000 个对象挤在同一个 database 的 pg_class /
--   pg_attribute 里，元数据查询退化、pg_dump 变小时级；
--   连接池还会按 schema 分裂，长尾租户直接撞连接上限。
--
--   纯「共享表 + tenant_id」又过不了大客户的合规审计 ——
--   他们会问「能否物理确认我的数据不在别人的表里」。
--
--   故：shared（长尾/SMB：共享表 + RLS）与 dedicated（大客户：独立 schema）
--   并存，且**共用同一套迁移与同一套 store 代码路径**。
--
-- ★★ 铁律（本迁移最重要的纪律）：
--    档位是**存储细节**，不是业务分支。
--    严禁任何 store 方法写 `if tier == dedicated {...} else {...}` ——
--    那会让两档代码路径分叉，dedicated 档迟早漏掉后续新增的修复。
--    tier 只允许影响**会话的建立方式**（search_path vs app.tenant_id）。
--
-- ★ 时间线说明：0001–0009 都是**单租户**假设下的产物。本迁移不改写它们
--   （迁移不可变：改了 hash 会被 Migrator 拒绝），而是在其后**增量补齐**。
--   既有单租户部署视为「一个已存在的租户」，通过 0011 的种子行纳入体系。

BEGIN;

-- ───────────────────────────── 租户注册表（平台库） ─────────────────────────────
--
-- ★ 这张表**本身不属于任何租户** —— 它是「租户的目录」，住在共享的
--   public schema 里，是所有租户的共同前提。因此它**不带** tenant_id
--   （否则就成了「谁知道所有租户」的鸡生蛋问题）。
--
--   纪律：本表只存**身份与档位**，绝不存任何业务数据。
--   一旦往里塞业务字段，它就变成了「跨租户可见的业务表」。
CREATE TABLE IF NOT EXISTS dim_tenant (
    -- ★ uuid 主键而非短码主键：
    --   短码（acme）是**人类可读**的，会有改名/复用需求；
    --   一旦它成为主键，改名就意味着所有外键跟着改 —— 灾难。
    --   uuid 不可变、不可猜测（短码可枚举出所有客户名单，是情报泄漏）。
    id              uuid        PRIMARY KEY,
    -- 短码：人类可读，用于工单/排障/子域名。**允许改**，故设 UNIQUE 但不作主键。
    code            text        NOT NULL,
    name            text        NOT NULL,
    -- 隔离档位。shared=共享表+RLS；dedicated=独立 schema。
    tier            text        NOT NULL CHECK (tier IN ('shared','dedicated')),
    status          text        NOT NULL CHECK (status IN (
                        'provisioning','active','suspended','closed')),
    -- dedicated 档的物理 schema 名。
    -- ★ shared 档**必须为 NULL** —— 见下方 CHECK 约束，那是本表的防伪线。
    schema_name     text        NULL,
    -- 配额：共享档下「一个租户拖垮全平台」必须被有界化。
    quota           jsonb       NOT NULL DEFAULT jsonb_build_object(
                        'maxAccounts', 50,
                        'maxRows', 5000000,
                        'maxQueriesPerMin', 600,
                        'allowExport', true),
    region          text        NOT NULL DEFAULT 'ap-southeast-1',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    -- ★★ 档位与 schema 的一致性 —— **在 DB 层锁死，而不是只靠应用校验**。
    --
    --   理由：应用校验只拦得住「走应用」的写入。日后有人手工 INSERT、
    --   从旧备份恢复、或写个运维脚本批量改档位，DB 约束仍会拦住。
    --
    --   ★ 为什么这条不能省（两个方向都是真实故障）：
    --     · dedicated 但 schema_name 为空 ⇒ 该租户以为自己有物理隔离，
    --       于是**不再依赖 RLS**；而它实际落在共享表里 ⇒ 隔离彻底失效
    --       （既没物理隔离、也没开行级隔离）。这是最危险的组合。
    --     · shared 却带 schema_name ⇒ 配置混乱：到底走哪条读路径？
    --       两个路径同时存在时，迟早有一条没加 tenant_id 条件。
    CONSTRAINT ck_dim_tenant_spark_cicada_tier_schema
        CHECK (
            (tier = 'dedicated' AND schema_name IS NOT NULL AND schema_name <> '')
         OR (tier = 'shared'    AND schema_name IS NULL)
        ),

    -- ★ schema 名白名单（与 Go 侧 tenant.SanitizeSchema 同规则）。
    --   search_path 是 DDL 级标识符，**无法参数化**，只能拼字符串；
    --   拼字符串就必须有白名单，且必须在 DB 层也有一份。
    --   强制 t_ 前缀：让越界的名字无法伪装成租户 schema
    --   （否则 "public" 可能是合法租户名，而它对所有连接都默认可见）。
    CONSTRAINT ck_dim_tenant_spark_cicada_schema_name
        CHECK (schema_name IS NULL OR schema_name ~ '^t_[a-z0-9_]{1,62}$'),
    -- 保留名兜底（即使手工插入也拦住）
    CONSTRAINT ck_dim_tenant_spark_cicada_schema_reserved
        CHECK (schema_name IS NULL OR schema_name NOT IN
               ('t_public','t_pg_catalog','t_information_schema','t_pg_toast')),

    -- 短码格式：小写字母开头，仅字母数字连字符。用于子域名，必须 DNS 安全。
    CONSTRAINT ck_dim_tenant_spark_cicada_code
        CHECK (code ~ '^[a-z][a-z0-9-]{1,62}$')
);

-- 短码唯一（全局）—— 子域名/工单引用都靠它
CREATE UNIQUE INDEX IF NOT EXISTS uq_dim_tenant_spark_cicada_code
    ON dim_tenant(code);
-- schema 名唯一（dedicated 档不允许多租户共用一个 schema）
CREATE UNIQUE INDEX IF NOT EXISTS uq_dim_tenant_spark_cicada_schema
    ON dim_tenant(schema_name) WHERE schema_name IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dim_tenant_status ON dim_tenant(status);
CREATE INDEX IF NOT EXISTS idx_dim_tenant_tier   ON dim_tenant(tier);

COMMENT ON TABLE dim_tenant IS
    '租户注册表（平台级，不属任何租户）。只存身份与档位，绝不存业务数据。';
COMMENT ON COLUMN dim_tenant.tier IS
    'shared=共享表+RLS 行级隔离（长尾）；dedicated=独立 schema（大客户/合规）。档位是存储细节，不是业务分支。';
COMMENT ON COLUMN dim_tenant.schema_name IS
    'dedicated 档的物理 schema 名（t_ 前缀白名单）；shared 档必须为 NULL。';

-- ───────────────────────────── 活跃租户视图（只读，供探活与运营） ─────────────────────────────
CREATE OR REPLACE VIEW v_tenant_active AS
SELECT id, code, name, tier, schema_name, region, created_at,
       (quota->>'maxAccounts')::int      AS max_accounts,
       (quota->>'maxRows')::bigint       AS max_rows,
       (quota->>'maxQueriesPerMin')::int AS max_queries_per_min,
       COALESCE((quota->>'allowExport')::boolean, true) AS allow_export
FROM dim_tenant
WHERE status = 'active';

-- ───────────────────────────── RLS 会话变量的默认值 ─────────────────────────────
--
-- ★★ 这是 shared 档隔离的**安全底线**，本迁移最关键的一段。
--
--   RLS 策略写成 `USING (tenant_id = current_setting('app.tenant_id')::uuid)`。
--   若该设置**未定义**，current_setting 会**报错**（这是我们想要的：
--   报错 = 拒绝，安全）。
--   但 `current_setting('app.tenant_id', true)`（第二参数 true = 缺失返回 NULL）
--   会静默返回 NULL —— 于是 `tenant_id = NULL` 求值为 NULL，**整行被过滤掉**
--   （也是拒绝，安全）。危险的是有人写 `IS NOT DISTINCT FROM` 配合缺失值，
--   那会**匹配 NULL 行**。
--
--   为避免这个坑，我们给会话变量设一个**必定不匹配任何真实 uuid 的哨兵值**：
--   全零 uuid 在业务上不会出现（dim_tenant.id 由服务端生成，不生成全零）。
--   于是「忘了设置租户」= 查询返回 0 行（安全失败），而不是报错中断
--   （后者虽安全但会让排障困难），也不是匹配到某一行（灾难）。
--
--   ★ 用 ALTER DATABASE ... SET 而非全局 SET：作用域限定在本库，
--     不影响同实例上的其他库。
DO $$
BEGIN
    -- 幂等：重复 ALTER 只是重复设同一个值
    EXECUTE format('ALTER DATABASE %I SET app.tenant_id = %L',
                   current_database(), '00000000-0000-0000-0000-000000000000');
EXCEPTION WHEN insufficient_privilege THEN
    -- 托管 Postgres 上普通账号可能无权 ALTER DATABASE。
    -- ★ 此时**不得**静默放过：策略层还有第二道防线（见下方 rls_tenant_id()）。
    RAISE WARNING '[0010] 无权 ALTER DATABASE 设置 app.tenant_id 默认值 —— 依赖 rls_tenant_id() 兜底';
END $$;

-- ───────────────────────────── 租户 ID 读取函数（RLS 策略统一入口） ─────────────────────────────
--
-- ★★ 所有 RLS 策略**必须**经由此函数读租户，绝不在策略里直接写
--    current_setting(...)。理由：
--
--    · 集中一处 ⇒ 「缺失时怎么办」只有一个答案，不会出现
--      「这张表用容错读法、那张表用严格读法」的错位
--      （错位本身就是越权：严格的那张会报错拒绝，容错的那张静默放行）。
--    · 缺失时返回一个**不可能匹配的哨兵 uuid**（而非 NULL）：
--      返回 NULL 会让 `tenant_id = NULL` 求值为 NULL ⇒ 行被过滤（安全）；
--      但若策略写的是 `tenant_id IS NOT DISTINCT FROM rls_tenant_id()`
--      （为了支持 NULL 行），NULL 会**匹配 NULL 行** —— 灾难。
--      返回哨兵则两种写法都安全：既不会匹配真实行，也不会匹配 NULL。
--
--   ★ 标记 STABLE（非 IMMUTABLE）：它读会话状态，同一语句内结果稳定，
--     但不能被常量折叠 —— 标成 IMMUTABLE 会让规划器在**设置变量之前**
--     就把结果算出来烤死，那是极隐蔽的越权。
CREATE OR REPLACE FUNCTION rls_tenant_id() RETURNS uuid AS $$
DECLARE
    v text;
BEGIN
    v := current_setting('app.tenant_id', true);
    IF v IS NULL OR v = '' OR v = '00000000-0000-0000-0000-000000000000' THEN
        -- 未设置 / 空 / 哨兵 ⇒ 返回哨兵（匹配不到任何真实租户）
        RETURN '00000000-0000-0000-0000-000000000000'::uuid;
    END IF;
    BEGIN
        RETURN v::uuid;
    EXCEPTION WHEN invalid_text_representation THEN
        -- 值不是合法 uuid ⇒ 同样返回哨兵（拒绝而非放行）
        RETURN '00000000-0000-0000-0000-000000000000'::uuid;
    END;
END;
$$ LANGUAGE plpgsql STABLE;

COMMENT ON FUNCTION rls_tenant_id() IS
    'RLS 策略统一入口：读 app.tenant_id；缺失/非法时返回哨兵 uuid（永不匹配真实租户）。';

-- ───────────────────────────── 租户配额用量（供配额守卫查询） ─────────────────────────────
--
-- ★ 不缓存计数（不建 counter 表）：计数器会与真实数据漂移，
--   而漂移的计数器比没有计数器更危险 —— 它会让「超配额」的判断
--   基于一个错数字。本视图按需实时统计，慢一点但**永远是对的**。
--
--   若将来统计确实成为瓶颈，再引入带**校验**的物化视图，
--   而不是引入一个无人核对的计数器。
CREATE OR REPLACE VIEW v_tenant_usage AS
SELECT
    t.id                                            AS tenant_id,
    t.code                                          AS tenant_code,
    t.tier                                          AS tier,
    (t.quota->>'maxAccounts')::int                  AS max_accounts,
    t.region                                        AS region
FROM dim_tenant t
WHERE t.status = 'active';

COMMIT;
