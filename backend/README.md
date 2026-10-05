# backend —— spark-cicada 编排层（Go）

> 纪律（docs/01 §4.1）：**Go 只编排与 I/O，永不实现业务公式**。
> 一切数值法则必须经 compute 内核（Rust）产出，见 `internal/compute`。

## 目录结构

| 路径 | 模块 | 说明 |
|---|---|---|
| `cmd/sparkd` | 主进程 | HTTP 入口 + 启动自检（G6）+ 优雅退出 |
| `internal/contracts` | 契约镜像 | 与 `contracts/*.ts` 对齐；含 `Canonicalize`/`Hash`（幂等 queryHash） |
| `internal/compute` | 内核客户端 | `SubprocessKernel`（调 `spark-compute --eval`）；后续 `GRPCKernel` |
| `internal/query` | **M-QUERY** | QueryState → ResultSet；择桶、缓存、SQL 下推；无业务公式 |
| `internal/precomp` | **M-PRECOMP** | 桶构建；依赖槽覆盖率门控 → 跳过写 NULL（绝不补 0） |
| `internal/authz` | **M-AUTH** | D11/D12/D13 权限求值：DENY 优先、可授出 ⊆ 自身权限 |
| `internal/chain` | 数据链/审批链 | F9=A 唯一主属、虚线仅抄送、F10 人工优先、F8 会签升级 |
| `internal/req` | **M-REQ** | 权限申请流：D7 + 跨部门 BLOCKED_CROSS_DEPT + 审批人超集升级 |
| `internal/api` | HTTP 层 | **fail-closed 字段级门控**（未授权字段不进响应） |
| `internal/gate` | **CI 闸门** | G1–G12 可执行断言（docs/05 的镜像） |
| `internal/db` | 连接与迁移 | Postgres 连接池、幂等迁移执行、DSN 口令脱敏（G11） |
| `internal/store` | 数据面 | 桶只读查询（NULL→`nil`）、控制面持久化、审计写入 |
| `internal/admin` | **M-ADMIN** | 集成控制面：槽/算法注册、漂移检测、模块挂载、审计 |
| `cmd/spark-migrate` | 迁移 CLI | `-up` / `-dry-run` / `-status` |

## 本地运行

```bash
# 1) 先构建计算内核（Rust）
cd ../compute && cargo build --release && cp target/release/spark-compute* ../backend/   # 或加入 PATH

# 2) （可选）起数据库并应用迁移
export SPARK_DB_DSN='postgres://spark:spark@127.0.0.1:5432/spark_cicada?sslmode=disable'
cd ../backend && go run ./cmd/spark-migrate -dir ../sql/migrations -up

# 3) 构建并启动编排层
cd ../backend
go build -o sparkd ./cmd/sparkd
SPARK_COMPUTE_BIN=./spark-compute SPARK_DB_DSN="$SPARK_DB_DSN" ./sparkd -addr :8080

# 4) 自检（容器 liveness 用）
./sparkd -selftest
```

> **fail-closed 启动**：若 `spark-compute` 不存在或健康检查失败，`sparkd` **拒绝启动**。
> 这是有意设计——宁可不上线，也不返回补 0 / 摊分的错误数据。
>
> **数据库缺席**：`sparkd` 仍会启动，但进入**降级模式**——
> `/api/query` 返回 `500 store not configured`，`/api/admin/*` 返回 `503`，
> `/healthz` 的 `db` 字段为 `false`。**绝不伪造数据**。

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/query` | 入参 `QueryState`；出参**门控后**的 `DataContract` |
| GET | `/api/me` | 当前账号权限视图（供前端勾选树渲染） |
| POST | `/api/request` | 提交权限申请（M-REQ）；跨部门返回 `BLOCKED_CROSS_DEPT` |
| GET | `/api/admin/overview` | 控制面总览（槽 / 算法 / 模块） |
| POST | `/api/admin/slots` | 登记/更新数据槽 |
| POST | `/api/admin/algorithms` | 登记算法（G4 解耦 + G5 fail-closed 传播） |
| GET | `/api/admin/modules` | 模块渲染清单（G9） |
| GET | `/healthz` | 内核健康 + 数据面状态 |

身份经请求头 `X-Spark-Account` 传入（生产由网关/SSO 注入，禁止客户端伪造）。
`/api/admin/*` 额外要求管理员账号（当前为 `ceo` / `vp.sea` / `vp.us`）。

## 数据库

连接串来源（按优先级）：

1. `SPARK_DB_DSN` —— **运行期主名**（`sparkd` / `spark-migrate` 部署用）
2. `SPARK_TEST_DB_DSN` —— **测试期名**（真库集成测试用；CI 真库作业会设它）
3. `SPARK_PG_HOST` / `SPARK_PG_USER` / `SPARK_PG_PASSWORD` / `SPARK_PG_DB` /
   `SPARK_PG_PORT` / `SPARK_PG_SSLMODE` —— 分字段组装

> ★ **为什么解析器要同时认前两个名字**（真实事故）：
> 迁移器原本只读 `SPARK_DB_DSN`，而 CI workflow 只设了 `SPARK_TEST_DB_DSN`，
> 于是真库步骤以「未配置连接信息」秒退、其后所有真库闸门被连环 skip。
> **两侧单看都自洽**，只有合排进同一个作业才暴露 —— 因此现在由
> `db.DSNFromEnv()` 统一解析，单一事实来源，并由
> `internal/gate/config_consistency_test.go` 静态钉住契约。

**口令永不回显**：所有日志与错误信息统一走 `db.RedactDSN`（G11）。

迁移纪律：
* 幂等——已应用的迁移按 `schema_migrations` 记账跳过；
* **检测改写**——已应用迁移的 sha256 若变化，直接拒绝继续（必须新增迁移而非改历史）；
* 单事务——每个迁移在独立事务内执行，失败整体回滚。

## 测试与闸门

```bash
go test ./...                    # 全部单元 + 端到端测试
cd .. && make gate               # 全部验收闸门 G1–G12
make gate-fast                   # 静态闸门（G1/G4/G11）
make gate-auth                   # 权限闸门（G7）
make gate-sql                    # SQL 静态不变量
make gate-admin                  # 控制面闸门（G4/G5/G6/G9/G10）
make gate-dr                     # 备份与容灾（G12）

# 真库集成闸门（未设置 DSN 时自动跳过）
SPARK_TEST_DB_DSN="$SPARK_DB_DSN" make gate-db
```

## 关键不变量（改代码时请勿破坏）

1. **Go 无公式**：`internal/query`、`internal/api` 不得出现 `gp/cogs/...` 的算术（G1 会拦）。
2. **缺失即 null**：`precomp` 覆盖率不达标时写 NULL + `skipped_fields`，绝不写 0（G3/G5）。
3. **DENY 优先**：`authz` 中任何 DENY 与其余来源冲突，一律 DENY（G7）。
4. **可授出 ⊆ 自身权限**：`authz.DelegationAllowed` 不得放宽（G7）。
5. **IT 不可见业务数值**：`authz` 末尾的 D7 硬约束不得移除；DB 层另有 CHECK 兜底。
6. **fail-closed 门控**：`api.Gate` 必须在序列化**之前**剔除未授权字段（G7）。
7. **审计 append-only**：`audit_log` 的 UPDATE/DELETE 触发器不得移除（G10）。
8. **桶查询只读**：`store.Postgres.SelectBucket` 绝不写库；未注册桶直接拒绝（防越表）。
9. **NULL 不补 0**：扫描桶度量列必须用 `*float64`；改成 `float64` 会把 NULL 变 0（G3 红线）。
10. **算法无数据源**：`admin.CheckFormulaNoDataSource` 不得放宽（G4）。
