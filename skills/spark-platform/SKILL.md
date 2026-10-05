---
name: spark-cicada-platform
description: spark-cicada 经营数据平台的项目级口径与架构纪律。用于模块划分、QueryState/DataContract 契约、算法与数据槽分离、预计算桶、集成控制面（MCP/API 授权与状态）、策略实验室选型决策、报表五层阅览、默认收起、视图模板、性能预算等工作开始前的强制口径判断。涉及 spark-cicada 仓库、经营报表、P&L、渠道店铺 SKU 下钻、MCP/API 授权管理、自迭代算法、模板中心或插槽架构时使用。
---

# spark-cicada-platform

## 强制入口

任何涉及 spark-cicada 平台（经营数据平台）的开发、Debug、架构变更、数据接入、报表、
权限、集成控制面或部署工作，**必须先读取本 Skill**，并以 `docs/01-开发文档.md` 为准。
本 Skill 与仓库文档冲突时，以仓库 `docs/` 最新版为准；本 Skill 的红线规则优先于一般实现习惯。

## 项目标识

- 项目名 **spark-cicada**，仓库 `miccchwang/spark-cicada`（**唯一托管地**：开发文档 / 运维文档 / skill / 代码）。
- 参考项目 `miccchwang/Trading-Strategy-and-Management-`（KODP）：**只复用能力与经验，不复制结构**。
- SSH host 别名 `github-sparkcicada`，deploy key `~/.ssh/id_ed25519_sparkcicada`。

## 七条架构红线（违反即架构腐化）

1. **三段式解耦不可破**：`M-FILTER` 只产出 `QueryState`（不查库不渲染）；`M-QUERY` 只翻译
   （不含业务公式）；`M-RENDER` 只消费 `DataContract`（不含业务计算、不读 `QueryState`）。
2. **算法与数据源必须分离**：算法只写 `formula` + `depends_on_slots`；数据来源只在 `slots/*.yaml`。
   换源改槽、改口径改规则、改算法改公式，三者互不牵连。
3. **缺失必跳过，禁止伪值**：数据槽不可用或覆盖率低于 `coverage_gate` → 值置 `null`，
   显示「待接入」/「—」。**绝不补 0、不取均值、不按比例摊分、不造演示数**。
4. **每个值可追溯**：任何非空值必须能沿 `AlgoTrace` 回溯到「哪条算法 + 哪些真实数据槽」。
5. **预计算优先**：能被 DB 预先算好的，绝不在请求时重算。新指标先问「落哪个桶」。
6. **所有可收起表格默认收起**：`DEFAULT_EXPANDED = {L0:true, L1..L4:false}`，任何折叠组件初始 `open=false`。
7. **IT 管数分离（D7=取消，第二轮修订）**：IT 可管理全部 MCP/API 授权与状态、账号启用、
   用户分组、权限审批；**恒不可见任何业务数值**，`canViewBusinessValues` 对 IT 恒 false 且不可勾选。
8. **权限可逐项勾选（D11）**：账号权限 = 模板(预设) ⊕ 分组授权(D12) ⊕ 逐项勾选（模块/维度/密级/字段）
   ⊕ 已批准申请(D14) ⊕ 上级代授(D13) ⊖ DENY(优先) ⊕ 临时授权；
   角色模板只是预设套餐，**最终以勾选结果为准**。求值见 `contracts/entitlement.ts` 与 `docs/07`、`docs/08`。
9. **职权向下覆盖（D13）**：`可授出集合(账号 A) ⊆ 账号 A 自身权限集合`；上级管下级、同级互不可见；
   代授不得溢出自身范围。
10. **凭据零暴露（S1）**：URL 不含账号密码；密钥不入 Git、不入前端产物、不入镜像；
    日志与 IT 页面只显示掩码。见 `docs/09-凭据防泄漏章程.md`。

## 模块与语言选型（D4 已拍板：Go + Rust 混合）

| 模块 | 语言 | 关键产出 |
|---|---|---|
| `M-FILTER` | TypeScript + Solid | `QueryState` |
| `M-QUERY` | **Go**（编排） | `ResultSet` |
| `M-ADMIN` / `M-SLOT` / `M-AUTH` / `M-AUDIT` / `M-COLLECT` / `M-GROUP` / `M-REQ` | **Go** | 控制面 / 插槽 / 权限 / 审计 / 采集 / 分组 / 申请流 |
| `M-ALGO` / `M-PRECOMP` / `M-RULE` | **Rust** | 法则库 / 预计算桶 / 规则集 |
| `M-RENDER` / `M-REPORT` / `M-PNL` / `M-TEMPLATE` | TypeScript + Solid | 渲染 / 五层阅览 / 损益 / 模板 |
| `M-STRATEGY` | Rust + TS | 策略方案 |

**分工纪律**：Go **只编排、不写数值公式**；Rust **只计算、不碰 HTTP 编排/鉴权**。
Go↔Rust 通过 gRPC/FFI 通信，禁止两处实现同一公式。

**已拍板技术栈（D1–D14）**：**阿里云 + AWS（新加坡/美国）** · Solid · PostgreSQL · Go+Rust 混合 ·
Docker Compose · 内网+VPN · 策略实验室仅管理层 · **IT 恒不可见业务数值（D7 取消）** ·
TikTok∥Shopee 并行（Shopee 费率暂缓）· 报表分模块口径 · **账号权限可勾选模块与维度** ·
**用户分组（D12）· 职权向下覆盖（D13）· 权限申请流（D14）** · **凭据零暴露（S1）**。

## 核心契约（改动须升版本号）

| 契约 | 文件 | 用途 |
|---|---|---|
| `QueryState` v1.0 | `contracts/query-state.ts` | 筛选模块 → 查询模块 |
| `DataContract` v1.0 | `contracts/data-contract.ts` | 查询模块 → 渲染模块 |
| `StrategyChoice` v1.0 | `contracts/strategy-choice.ts` | 策略选型决策项 |
| `ViewTemplate` v1.0 | `contracts/view-template.ts` | 视图模板 |
| `Entitlement` v1.1 | `contracts/entitlement.ts` | 账号授权项（勾选模块/维度/密级 + 分组 + 上级） |
| `UserGroup` v1.0 | `contracts/user-group.ts` | 用户分组与成员关系（D12） |
| `PermissionRequest` v1.0 | `contracts/permission-request.ts` | 权限申请单（D14） |
| `SlotManifest` v1.0 | `contracts/slot-manifest.yaml` | 模块注册 |
| `algorithm` / `data-slot` / `rule-set` / `precompute-bucket` v1.0 | `contracts/*.yaml` | 算法层 |

## 报表与 P&L 分层（固定）

- 五层：`L0 总览 → L1 板块 → L2 渠道 → L3 店铺 → L4 SKU`；默认仅 L0 展开。
- 每层可深链（`?level=...&domain=...&channel=...`），面包屑逐级返回。
- 经营报表（运营视角，GMV 为主）与 P&L（财务视角，Net Revenue 为主）**从 L1 起分叉**，
  共享 L0。P&L 必须支持口径切换（A 参考口径 / B Finance 口径）。

## 口径切换红线（承袭 KODP 实测）

- 口径 A：Seller Discount 计入营销费用（易误读为"费用失控"）。
- 口径 B（Finance）：Seller Discount 作 contra-revenue 收入抵减。
- **净收入两口径完全相同**；切换只改变"费用侧/收入侧"展示归属，但**会完全改变经营解读**。
- 对外材料必须显式标注所用口径与分母，不得混用。

## 集成控制面纪律

- 状态机：`PENDING / ACTIVE / DISABLED / DEGRADED / ERROR`。
- **可用性 = 状态位 AND 凭证有效性 AND 探活通过**。KODP 教训：「库里 ACTIVE 但签名无效 = 无效」；
  「已授权但机器人不在群 / BC 无 advertiser」这类中间态必须显式展示为 `DEGRADED`。
- 密钥只存引用（`*_ref`），**永不落库明文**。
- 全部控制面操作 append-only 留痕。

## 策略实验室纪律

- 决策项**一律以选型方式提供**（2–4 个选项 + 影响预览 + 风险等级），用户只有
  「选 A / 选 B / 保持现状」三种动作，**绝不让人填公式**。
- 任何策略变更可一键回滚；每次变更落一行历史（旧值/新值/决策人/依据快照哈希/实际效果）。

## 数据唯一键红线

- 跨库/跨平台店铺合并唯一键 = **`database + platform + shop_id / unique_key`**；不带库名会误并。
- 总表行键 = `product_id`；SKU 只作精确辅助匹配。
- 未逐店核对重叠前，**禁止跨库求和**。

## 性能预算

TTI < 1000ms；筛选 P95 命中预计算 < 100ms、未命中 < 500ms；首屏 gzip < 200KB；
大表格滚动 60fps。首选手段永远是**预计算**，其次缓存、下推、虚拟滚动、WASM。

## 验收闸门

十一条闸门见 `docs/05-验收闸门.md`（分层依赖 / 默认收起 / 缺失值 / 算法与槽分离 /
覆盖率门控 / 预计算一致性 / 权限 / 性能 / 插槽 / 策略审计 / **密钥扫描 G11**）。任一失败阻断合并。
新增约束必须**先写文档、再加闸门、最后实现**。

## 协作与续跑纪律（DingTalk）

- 需要用户决策时，发**简短单行**钉钉到用户本人（`+dm --to "CN-Michael-黄基煜"`），
  消息末尾带 **回执编号**（`R-YYYYMMDD-NN`）+ 待决项 + 超时时间。
- 回复读取：`+chat-messages --open-dingtalk-id DEkTtguVm54R2VYm7ly23x3z0XCiiP0xiS0`。
- 定时轮询任务会自动读回复并续跑（见 `docs/10-钉钉协作与续跑机制.md`）。
- **禁止**用 `--content "$(cat 长文件)"` 发长消息（会超时 SIGTERM）；长内容只发摘要 + 仓库路径。

## 凭据安全纪律（S1）

- **URL 不含账号密码**：禁止 `scheme://user:pass@host`；身份走 HttpOnly Cookie / ticket。
- **前端产物零密钥**：`public/`、`dist/` 只允许公开配置；secret 只在服务端，由后端代理。
- **密钥不入 Git、不入镜像**：`.env*` 等强制 git-ignore；CI 闸门 G11 扫描兜底。
- **日志/页面只显示掩码**：`sk-****abcd`，明文永不回显。
- 详见 `docs/09-凭据防泄漏章程.md`。

## 复用 KODP 的清单

**复用**：三层解耦采集、幂等去重、断点续传、TikTok/Shopee OAuth 与财务字段映射、
数据验证闸门方法论、口径纪律、覆盖率门控、唯一键红线、xlsx/图表/银行家舍入等 debug 经验。
**重构**：单体 index.html、请求时重算、Python 大批量运算。

## 变更纪律

改算法/槽/规则/契约/架构/口径 → 必须同步对应 YAML 版本号与文档章节，
并触发受影响预计算桶的重算（`spark-pc rebucket`）。
