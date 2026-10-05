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

## 本地运行

```bash
# 1) 先构建计算内核（Rust）
cd ../compute && cargo build --release && cp target/release/spark-compute* ../backend/   # 或加入 PATH

# 2) 构建并启动编排层
cd ../backend
go build -o sparkd ./cmd/sparkd
SPARK_COMPUTE_BIN=./spark-compute ./sparkd -addr :8080

# 3) 自检（容器 liveness 用）
./sparkd -selftest
```

> **fail-closed 启动**：若 `spark-compute` 不存在或健康检查失败，`sparkd` **拒绝启动**。
> 这是有意设计——宁可不上线，也不返回补 0 / 摊分的错误数据。

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/query` | 入参 `QueryState`；出参**门控后**的 `DataContract` |
| GET | `/api/me` | 当前账号权限视图（供前端勾选树渲染） |
| POST | `/api/request` | 提交权限申请（M-REQ）；跨部门返回 `BLOCKED_CROSS_DEPT` |
| GET | `/healthz` | 内核健康 |

身份经请求头 `X-Spark-Account` 传入（生产由网关/SSO 注入，禁止客户端伪造）。

## 测试与闸门

```bash
go test ./...                    # 全部单元 + 端到端测试
cd .. && make gate               # 全部验收闸门 G1–G12
make gate-fast                   # 静态闸门（G1/G4/G11）
make gate-auth                   # 权限闸门（G7）
make gate-dr                     # 备份与容灾（G12）
```

## 关键不变量（改代码时请勿破坏）

1. **Go 无公式**：`internal/query`、`internal/api` 不得出现 `gp/cogs/...` 的算术（G1 会拦）。
2. **缺失即 null**：`precomp` 覆盖率不达标时写 NULL + `skipped_fields`，绝不写 0（G3/G5）。
3. **DENY 优先**：`authz` 中任何 DENY 与其余来源冲突，一律 DENY（G7）。
4. **可授出 ⊆ 自身权限**：`authz.DelegationAllowed` 不得放宽（G7）。
5. **IT 不可见业务数值**：`authz` 末尾的 D7 硬约束不得移除；DB 层另有 CHECK 兜底。
6. **fail-closed 门控**：`api.Gate` 必须在序列化**之前**剔除未授权字段（G7）。
7. **审计 append-only**：`audit_log` 的 UPDATE/DELETE 触发器不得移除（G10）。
