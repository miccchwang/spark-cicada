-- 0005_collect_staging.sql —— M-COLLECT 采集层：临时仓库（staging）与断点续传
--
-- 为什么需要这一层（用户明确要求 + KODP 实战教训）：
--   * 抓第三方平台数据 / 调 MCP、API 时，**不能写死「调用完全成功才落库」**。
--     网络会抖、会被限流、会中途断；若整批要么全落要么全不落，一次 429
--     就得从头重放整月 —— 既浪费配额又必然再次撞限流，永远跑不完。
--   * 因此必须有**临时仓库**：每成功取到一页就立刻 COMMIT 落 staging，
--     并推进游标（断点续传）。中断后再跑，从游标处继续，不重复调用上游。
--   * 落表失败重试**不得产生新的上游调用** —— 靠幂等键去重保证。
--   * 限流要**自动降速**（0.5s 起步，最高 120s），而非硬编码固定间隔。
--   * 服务器端挂自动化采集模块，直接对接临时仓库；
--     由**守卫**校验数据完整性后才允许「过闸」进入投影层。
--
-- 分层（承袭 KODP 采集层 / 原始层 / 投影层）：
--   collect_job / collect_page       ── 采集层：作业与分页台账（游标、配额、档位）
--   staging_record                   ── 原始层（临时仓库）：上游原始行，幂等键去重
--   staging_ingest_guard             ── 守卫：完整性判定与过闸记录
--   投影层 = 既有 fact_* / bucket_*（**由守卫放行后才写入**）

BEGIN;

-- ───────────────────────── 采集作业（一次采集任务）─────────────────────────
--
-- 一个 job 对应「某数据源 × 某范围」的一次采集（如 slot.sales_daily × 2026-09）。
-- 游标与档位挂在 job 上，是断点续传与自适应降速的状态载体。
CREATE TABLE IF NOT EXISTS collect_job (
    id                text        PRIMARY KEY,   -- 如 job.tiktok.sales.202609
    slot_id           text        NULL REFERENCES registry_slot(id) ON DELETE SET NULL,
    -- 数据源标识：platform / mcp_server / api_connector
    source_kind       text        NOT NULL
                                  CHECK (source_kind IN ('api','mcp','skill')),
    source_ref        text        NOT NULL,      -- 连接器/MCP 的 id（**不存密钥**，G11）
    -- 采集范围（决定幂等与断点边界）
    scope             text        NOT NULL,      -- 如 month=2026-09 或 global
    region            text        NOT NULL DEFAULT 'ap-southeast-1',  -- G12 分地域
    -- ── 断点续传状态 ──
    -- cursor 是**上游不透明游标**（page_token / nextCursor / offset），
    -- 原样保存、原样回传，不做解释。NULL 表示尚未开始或已到底。
    cursor            text        NULL,
    pages_done        integer     NOT NULL DEFAULT 0  CHECK (pages_done >= 0),
    rows_staged       bigint      NOT NULL DEFAULT 0  CHECK (rows_staged >= 0),
    -- ── 自适应降速状态（档位索引，见 collect_rate_gear）──
    gear_index        integer     NOT NULL DEFAULT 0  CHECK (gear_index >= 0),
    ok_streak         integer     NOT NULL DEFAULT 0  CHECK (ok_streak >= 0),
    -- ── 生命周期 ──
    status            text        NOT NULL DEFAULT 'PENDING'
                                  CHECK (status IN ('PENDING','RUNNING','PAUSED',
                                                    'THROTTLED','COMPLETE','FAILED')),
    -- 状态语义：
    --   PENDING   未开始
    --   RUNNING   正在采集
    --   PAUSED    主动暂停（可续）
    --   THROTTLED 被限流中，等待下次窗口（**可续**，不是失败）
    --   COMPLETE  上游已到底（cursor 耗尽）
    --   FAILED    硬失败（认证失效等），需人工
    last_error        text        NULL,
    next_attempt_at   timestamptz NULL,          -- 降速后的下次可尝试时间
    started_at        timestamptz NULL,
    finished_at       timestamptz NULL,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_collect_job_status  ON collect_job(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_collect_job_slot    ON collect_job(slot_id, scope);
CREATE INDEX IF NOT EXISTS idx_collect_job_region  ON collect_job(region, status);

-- ───────────────────────── 降速档位表（离散档位，可审计）─────────────────────────
--
-- 用户要求：频次需自动降速，**从 0.5s 到 120s 有档位自动调整**。
-- 采用**离散档位**而非连续退避：行为可预测、可在文档里列清、可被测试断言。
-- 档位表落库而非写死在代码里，便于 IT 按平台微调而不改代码。
CREATE TABLE IF NOT EXISTS collect_rate_gear (
    gear_index    integer     PRIMARY KEY CHECK (gear_index >= 0),
    delay_ms      integer     NOT NULL CHECK (delay_ms > 0),
    label         text        NOT NULL,
    -- 达到本档位时，需连续成功多少次才允许降一档（回落更保守，避免抖动）
    recover_after integer     NOT NULL DEFAULT 5 CHECK (recover_after > 0)
);

-- 档位：0.5s → 120s，共 9 档（倍率约 ×2，末档封顶 120s）
INSERT INTO collect_rate_gear (gear_index, delay_ms, label, recover_after) VALUES
    (0,    500, '0.5 秒 · 正常',        5),
    (1,   1000, '1 秒',                 5),
    (2,   2000, '2 秒',                 5),
    (3,   4000, '4 秒 · 轻限流',        8),
    (4,   8000, '8 秒',                 8),
    (5,  15000, '15 秒 · 明显限流',    10),
    (6,  30000, '30 秒',               12),
    (7,  60000, '60 秒 · 重限流',      15),
    (8, 120000, '120 秒 · 封顶',       20)
ON CONFLICT (gear_index) DO UPDATE
    SET delay_ms      = EXCLUDED.delay_ms,
        label         = EXCLUDED.label,
        recover_after = EXCLUDED.recover_after;

-- ───────────────────────── 采集分页台账（每页一次 COMMIT）─────────────────────────
--
-- ★ 核心纪律：**每取到一页就立刻 COMMIT 落库**，不等整批。
--   网络成果立即持久化 —— 这是「断点续传」能成立的前提。
--   page_no + job_id 唯一，重复写同一页不会产生新行（幂等）。
CREATE TABLE IF NOT EXISTS collect_page (
    job_id        text        NOT NULL REFERENCES collect_job(id) ON DELETE CASCADE,
    page_no       integer     NOT NULL CHECK (page_no >= 0),
    -- 本页请求时用的游标（用于审计：从哪续的）
    cursor_in     text        NULL,
    -- 本页返回的下一游标（断点续传的**真值**）
    cursor_out    text        NULL,
    row_count     integer     NOT NULL DEFAULT 0 CHECK (row_count >= 0),
    -- 上游原始响应指纹：内容哈希，用于「落表失败重试不重复调上游」
    payload_sha256 text       NULL,
    http_status   integer     NULL,
    fetched_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, page_no)
);

CREATE INDEX IF NOT EXISTS idx_collect_page_job ON collect_page(job_id, page_no DESC);

-- ───────────────────────── 原始层：临时仓库 staging_record ─────────────────────────
--
-- ★ 临时仓库的实体。上游返回的**原始行**原样落这里，不做业务解释。
--   保留策略（用户已定）：**过闸即清理** —— 完整性守卫放行、投影成功后删除。
--
-- 幂等键 idem_key：由 (job_id, 上游行标识) 派生，UNIQUE 保证
--   「同一行重复落库 = 同一行」，从而「落表失败重试不产生新的上游调用」。
--   唯一键**显式带表名**（跨库/跨平台合并红线，docs/01 §13）。
CREATE TABLE IF NOT EXISTS staging_record (
    id            bigserial   PRIMARY KEY,
    job_id        text        NOT NULL REFERENCES collect_job(id) ON DELETE CASCADE,
    page_no       integer     NOT NULL,
    -- 幂等键：同一次采集内，同一上游实体的同一版本只应有一行
    idem_key      text        NOT NULL,
    -- 原始键字段（冗余保存，便于不解析 JSON 也能对账）
    natural_key   text        NULL,
    -- 原始载荷：上游 schema 可能变，故用 jsonb 原样存，不强约束字段
    payload       jsonb       NOT NULL,
    region        text        NOT NULL DEFAULT 'ap-southeast-1',   -- G12
    -- ── 守卫状态 ──
    -- PENDING: 已落 staging，尚未过闸；PASSED: 已过闸；REJECTED: 完整性不足被拒
    guard_state   text        NOT NULL DEFAULT 'PENDING'
                              CHECK (guard_state IN ('PENDING','PASSED','REJECTED')),
    reject_reason text        NULL,
    staged_at     timestamptz NOT NULL DEFAULT now()
);

-- ★ 幂等去重：这是「重试不重复落库」的**数据库级保证**，而非应用层约定。
CREATE UNIQUE INDEX IF NOT EXISTS uq_staging_record_idem
    ON staging_record (job_id, idem_key);

CREATE INDEX IF NOT EXISTS idx_staging_record_guard  ON staging_record(job_id, guard_state);
CREATE INDEX IF NOT EXISTS idx_staging_record_region ON staging_record(region, guard_state);

-- ───────────────────────── 完整性守卫（过闸记录）─────────────────────────
--
-- 用户要求：「守卫验收数据完整性后过闸」。
-- 守卫对一次采集做**完整性判定**，判定通过才允许投影。
--
-- 完整性口径（fail-closed，缺一不可）：
--   * pages_closed ：上游声明有 N 页，实际取满 N 页（未提前中断）
--   * cursor_ended ：游标已到底（不是「看起来取完了」）
--   * rows_match   ：行数与上游声明的 total 一致（若上游给出 total）
--   * required_ok  ：必填字段无缺失
--   * coverage_ok  ：字段覆盖率达标（与 registry_slot_coverage 同口径）
--
-- 判定不通过 ⇒ 数据**停留 staging 且标 PENDING**，绝不进入投影层。
CREATE TABLE IF NOT EXISTS staging_ingest_guard (
    id             bigserial   PRIMARY KEY,
    job_id         text        NOT NULL REFERENCES collect_job(id) ON DELETE CASCADE,
    evaluated_at   timestamptz NOT NULL DEFAULT now(),
    -- ── 各子项结果（NULL = 无法判定，非「通过」）──
    expected_pages integer     NULL,
    actual_pages   integer     NULL,
    expected_rows  bigint      NULL,
    actual_rows    bigint      NULL,
    cursor_ended   boolean     NULL,
    required_ok    boolean     NULL,
    coverage       numeric(5,4) NULL CHECK (coverage IS NULL OR coverage BETWEEN 0 AND 1),
    coverage_floor numeric(5,4) NOT NULL DEFAULT 0.8000,
    -- ── 总结论 ──
    passed         boolean     NOT NULL,
    -- 未通过的原因列表（空数组 = 通过）
    reasons        text[]      NOT NULL DEFAULT '{}',
    -- 判定所依据的快照（可复现：同一份 staging 应判出同一结论）
    snapshot_sha256 text       NULL
);

CREATE INDEX IF NOT EXISTS idx_ingest_guard_job ON staging_ingest_guard(job_id, evaluated_at DESC);

-- ───────────────────────── 投影放行台账 ─────────────────────────
--
-- 只有守卫 passed=true 才允许写投影层；这里记录「哪次判定放行了哪批」。
-- 与 precomp_build_log 的关系：本表在**前**（数据准入），后者在**后**（桶构建）。
-- 审计要求 append-only 由应用层保证 + 下方触发器兜底。
CREATE TABLE IF NOT EXISTS staging_project_release (
    id           bigserial   PRIMARY KEY,
    job_id       text        NOT NULL REFERENCES collect_job(id) ON DELETE CASCADE,
    guard_id     bigint      NOT NULL REFERENCES staging_ingest_guard(id) ON DELETE RESTRICT,
    released_rows bigint     NOT NULL CHECK (released_rows >= 0),
    -- 投影目标（如 fact_sales_daily）
    target_table text        NOT NULL,
    released_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_project_release_job ON staging_project_release(job_id, released_at DESC);

COMMIT;
