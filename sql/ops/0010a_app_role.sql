-- 0010a_app_role.sql —— 应用角色（RLS 生效的前提）与授权
--
-- ══════════════════════════════════════════════════════════════════════════
-- ★★ 本文件存在的理由是一个**真实事故**：
--
--   我们用 ENABLE + FORCE ROW LEVEL SECURITY + 正确的策略建好了 RLS，
--   然后以应用账号查询 —— 看到了**全部租户的行**，策略一点作用都没有。
--
--   根因：本地集群的应用角色是 **SUPERUSER**（rolbypassrls=true）。
--   PostgreSQL 的规则是：
--
--     ★ 超级用户与带 BYPASSRLS 的角色**无条件绕过 RLS**。
--       `FORCE ROW LEVEL SECURITY` **对它们无效** ——
--       FORCE 只解决「表拥有者绕过」，不解决「超级用户绕过」。
--
--   这是最坏的一类缺陷：不报错，且所有检查都显示「隔离已启用」。
-- ══════════════════════════════════════════════════════════════════════════
--
-- ★ 因此本文件把「应用角色必须非特权」做成**可执行的部署步骤**，
--   而不是文档里的一句提醒。提醒会被跳过；SQL 不会。
--
-- ★ 职责分离（为什么是三个角色而不是一个）：
--
--   spark_owner  迁移/DDL 所有者。可以建表改结构。**不用于跑服务**。
--   spark_app    服务运行角色。NOSUPERUSER NOBYPASSRLS —— RLS 对它生效。
--                只给 DML（增删改查），不给 DDL。
--   spark_read   只读账号（报表/BI/排障）。进一步收紧。
--
--   分离的意义：即便服务被攻破，攻击者拿到的是 spark_app 的权限 ——
--   它**改不了 schema、删不了表、也绕不过 RLS**。
--   若服务直接用 owner 跑（很常见），一次注入就能 DROP TABLE。
--
-- ★ 幂等性：本脚本可重复执行。角色已存在时只**校正**属性
--   （例如有人手工把 spark_app 改成了 superuser，本脚本会改回来）。

BEGIN;

-- ───────────────────────────── 应用角色 ─────────────────────────────
DO $$
BEGIN
    -- ① 服务运行角色：RLS 生效的关键
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'spark_app') THEN
        CREATE ROLE spark_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    ELSE
        -- ★ 校正：把属性强制改回安全值。
        --   理由：若有人为了「方便排障」临时提权又忘了收回，
        --   共享档隔离就静默失效了。每次部署都校正一遍。
        ALTER ROLE spark_app NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;

    -- ② 只读角色（可选）：给 BI/报表/排障用
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'spark_read') THEN
        CREATE ROLE spark_read LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    ELSE
        ALTER ROLE spark_read NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
EXCEPTION
    WHEN insufficient_privilege THEN
        -- 托管 Postgres 上应用账号可能无权 CREATE ROLE。
        -- ★ 此时给出**可操作**的指引，而不是静默通过 ——
        --   因为「没建成应用角色」意味着共享档隔离无法安全启用。
        RAISE WARNING
            '[0010a] 无权创建/修改角色。请在具备 CREATEROLE 权限的账号下执行：'
            'CREATE ROLE spark_app LOGIN NOSUPERUSER NOBYPASSRLS;'
            'ALTER ROLE spark_app NOSUPERUSER NOBYPASSRLS;';
END $$;

-- ───────────────────────────── 授权 ─────────────────────────────
DO $$
BEGIN
    -- schema 使用与建表（建表权给 app 是为了迁移期间的临时对象，生产可收回）
    EXECUTE 'GRANT USAGE ON SCHEMA public TO spark_app';
    -- DML：现有对象
    EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO spark_app';
    EXECUTE 'GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO spark_app';
    EXECUTE 'GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO spark_app';
    -- DML：未来对象（否则每次新增表都要手工补授权 —— 迟早漏）
    EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA public
             GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO spark_app';
    EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA public
             GRANT USAGE, SELECT ON SEQUENCES TO spark_app';
    EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA public
             GRANT EXECUTE ON FUNCTIONS TO spark_app';

    -- 只读角色：仅 SELECT
    EXECUTE 'GRANT USAGE ON SCHEMA public TO spark_read';
    EXECUTE 'GRANT SELECT ON ALL TABLES IN SCHEMA public TO spark_read';
    EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA public
             GRANT SELECT ON TABLES TO spark_read';
EXCEPTION
    WHEN insufficient_privilege THEN
        RAISE WARNING '[0010a] 授权失败（权限不足）—— 请以对象所有者身份重跑本脚本';
END $$;

-- ★ 注意：**不**给 spark_app 建 schema 的权限（CREATE ON DATABASE）。
--   建 schema 是开通流程（Task #55）的职责，由 owner 角色执行。
--   给服务角色建 schema 的权力会扩大攻击面，且没有必要。

COMMIT;

-- ───────────────────────────── 部署自检（人工执行） ─────────────────────────────
--
-- 上线后请执行以下查询确认 RLS 真的会生效：
--
--   SELECT rolname, rolsuper, rolbypassrls,
--          (NOT rolsuper AND NOT rolbypassrls) AS rls_effective
--     FROM pg_roles
--    WHERE rolname IN ('spark_app','spark_read');
--
-- rls_effective 必须为 true。若为 false，共享档多租户隔离**未生效**。
--
-- 并确认服务实际使用的连接角色：
--
--   SELECT current_user, session_user;
--
-- ★ 权威做法：调用后端自检接口（backend/internal/tenant.AssertRLSCapable），
--   它会在启动时拒绝服务，并把角色属性暴露在探活接口上。
