/**
 * contracts/tenant.ts —— v1.0
 *
 * 多租户契约。唯一真源；`web/src/contracts/tenant.ts` 是构建期镜像，
 * 二者必须逐字一致（由 web/test/contracts.test.mjs 断言）。
 *
 * ══════════════════════════════════════════════════════════════════════════
 * ★★ 商业化的第一性问题：**一家公司改了一个数字，绝不能让另一家公司看见。**
 *
 *   这不是「加个 where 条件」的问题。它同时是三个问题：
 *     ① 身份问题 —— 这次请求到底属于哪个租户？
 *     ② 存储问题 —— 数据物理上怎么摆（同库？同 schema？独立库？）
 *     ③ 证明问题 —— 怎么**证明**隔离成立，而不是「应该成立」。
 *   ②（存储）由本契约的 IsolationTier 表达；①③ 由 resolveTenant 的
 *   fail-closed 语义与隔离测试共同钉死。
 * ══════════════════════════════════════════════════════════════════════════
 *
 * ★ 为什么是**混合档**（IsolationTier 有两档而不是一档）：
 *
 *   纯「每租户独立 schema」在商用上是**自伤**：
 *     · 本仓库当前迁移共创建 24 表 + 5 视图 + 32 索引 + 1 函数 + 1 触发器
 *       （即 ~63 个 schema 对象）。1000 家租户 ⇒ 63,000 个对象堆在
 *       同一个 database 的 pg_class / pg_attribute 里，元数据查询退化，
 *       备份/pg_dump 变成小时级。
 *     · 连接池按 schema 分裂：每租户各自一套池，海量长尾租户时
 *       连接数直接撞 Postgres 上限。
 *   纯「共享库 + tenant_id」又过不了大客户的合规审计
 *   （他们会问「能不能物理确认我的数据不在别人的表里」）。
 *
 *   所以两档共存，且**共用同一套迁移与同一套 store 代码路径**：
 *     · shared    —— 长尾/SMB：共享表 + tenant_id + RLS 行级隔离。
 *     · dedicated —— 大客户/合规敏感：独立 schema（物理可证明）。
 *   关键纪律：档位是**存储细节**，不是业务分支。任何一个 store 方法
 *   都不允许写 `if tier == dedicated { ... } else { ... }` ——
 *   那会让两档的代码路径分叉，dedicated 档迟早漏掉一个后续新增的修复。
 *   分档只允许体现在**连接/会话的建立处**（见 resolveTenant）。
 */

export const TENANT_CONTRACT_VERSION = "1.0" as const;

/**
 * 隔离档位。
 *
 * ★ 命名刻意用 shared / dedicated 而不是 small / big：
 *   决定档位的是**合规要求**，不是公司规模。
 *   一家 5 人的基金公司因为审计要求仍然可以是 dedicated。
 */
export type IsolationTier =
  /** 共享表 + 每行 tenant_id + RLS 行级隔离。适合长尾，边际成本≈0 */
  | "shared"
  /** 独立 schema（物理可证明隔离）。适合大客户/合规敏感 */
  | "dedicated";

/** 全部合法档位（有序：从共享到独立，成本递增）。 */
export const ISOLATION_TIERS: readonly IsolationTier[] = ["shared", "dedicated"];

/** 租户生命周期状态。 */
export type TenantStatus =
  | "provisioning" // 已建记录，schema/RLS 尚在就绪中 —— **不可服务**
  | "active" // 正常服务
  | "suspended" // 欠费/违规：可登录但拒绝业务查询（fail-closed）
  | "closed"; // 已注销：数据待归档清除

/**
 * 租户记录（平台库 dim_tenant 的投影）。
 *
 * ★ schemaName 在 shared 档为 **null** —— 这不是「没填」，而是
 *   「该租户的数据不在任何独立 schema 里」这个**事实**。
 *   用空串冒充会让「dedicated 档但忘了建 schema」与「shared 档」
 *   长得一模一样，而前者是**会导致数据串租的严重缺陷**。
 */
export interface Tenant {
  /** 租户 ID（uuid）。**唯一**稳定标识，绝不随公司改名而变 */
  id: string;
  /** 展示用短码（人类可读、可用于排障与工单），如 acme */
  code: string;
  /** 公司名称 */
  name: string;
  tier: IsolationTier;
  status: TenantStatus;
  /** dedicated 档的物理 schema 名；shared 档必须为 null */
  schemaName: string | null;
  /** 创建时间（ISO） */
  createdAt: string;
  /** 配额：海量租户下「一个租户拖垮全平台」是必须防住的事故 */
  quota: TenantQuota;
}

/**
 * 租户配额。
 *
 * ★ 存在的理由（不是「锦上添花」）：
 *   共享档意味着**同一个 Postgres 承载所有长尾租户**。没有配额时，
 *   一家租户跑一个全表扫描就能让其他所有租户一起变慢 ——
 *   这在商业上是「你花钱买的服务被别人的错误拖垮」。
 *   配额把「互相影响」从**无限**收敛到**有界**。
 */
export interface TenantQuota {
  /** 最大账号数 */
  maxAccounts: number;
  /** 最大数据行数（事实表，粗粒度护栏） */
  maxRows: number;
  /** 每分钟最大查询数（防单租户打满连接池） */
  maxQueriesPerMin: number;
  /** 是否允许导出（合规敏感客户的常见要求：禁止批量导出） */
  allowExport: boolean;
}

/** 缺省配额（未指定时）。取保守值 —— 宁可按需上调，不可默认敞开。 */
export const DEFAULT_TENANT_QUOTA: Readonly<TenantQuota> = Object.freeze({
  maxAccounts: 50,
  maxRows: 5_000_000,
  maxQueriesPerMin: 600,
  allowExport: true,
});

/**
 * 租户上下文 —— 一次请求解析出来的「我是谁、我的数据在哪」。
 *
 * ★ 这是**请求级**对象，绝不可跨请求缓存复用：
 *   它是 per-request 的解析结果，缓存它等于把租户身份泄漏给下一个请求。
 */
export interface TenantContext {
  tenantId: string;
  tier: IsolationTier;
  /**
   * 物理 schema 名。
   * ★ dedicated 档必填；shared 档为 null（数据在共享表的 tenant_id 列里）。
   */
  schemaName: string | null;
  /**
   * ★★ 本字段是 shared 档 RLS 的**唯一输入**。
   *
   *   它会被写进会话变量 `app.tenant_id`，RLS 策略
   *   `USING (tenant_id = current_setting('app.tenant_id')::uuid)` 依赖它。
   *   因此它**必须有值**（shared 档也一样），否则策略会把
   *   `current_setting(...)` 求值失败或求成空 —— 两种结果都很危险：
   *     · 求值失败 ⇒ 报错（安全）
   *     · 求成空/缺失 ⇒ 若策略写成 `IS NOT DISTINCT FROM` 就会**放行全部**
   *   所以本契约要求：解析不出 tenantId 时**拒绝请求**，绝不降级。
   */
  rlsTenantId: string;
}

/**
 * ★★ 租户解析结果 —— fail-closed 的载体。
 *
 * 用「判别联合」而不是「Tenant | null」：
 *   后者让调用方可以写 `if (ctx) {...} else {...}`，
 *   而 else 分支**迟早**会被人写成「回退到默认租户」。
 *   默认租户是对的方向上最危险的一个词 —— 它把「找不到租户」
 *   静默转成「用另一个公司的数据服务你」。
 *   判别联合让「拒绝」成为一个**必须显式处理**的分支。
 */
export type TenantResolution =
  | { kind: "resolved"; context: TenantContext }
  | {
      kind: "rejected";
      /** 拒绝原因（供日志与排障；面向前端时不得泄露他租户信息） */
      reason: TenantRejectReason;
    };

/**
 * 拒绝原因。
 *
 * ★ 这些原因**可以**写进日志，但**不得**原样回给调用方：
 *   `not_found`（租户不存在）与 `suspended`（租户被停用）的区别，
 *   对攻击者来说是「这个 ID 存在吗」的探测信号。对外一律 401/403。
 */
export type TenantRejectReason =
  | "missing" // 请求里根本没有租户标识
  | "malformed" // 有标识但不是合法 uuid
  | "not_found" // 库中不存在
  | "not_active" // 存在但 status != active（provisioning/suspended/closed）
  | "quota_exceeded" // 超配额
  | "internal"; // 解析过程出错（DB 不可达等）—— 同样拒绝，绝不放行

/**
 * ★★ 解析租户的函数签名。
 *
 * 纪律（**这是本契约最重要的一条**）：
 *   返回 rejected 时，调用方**必须**终止请求并返回 401/403。
 *   绝不允许：
 *     · 回退到某个默认租户
 *     · 回退到「无租户」的全局视角（等于看到所有租户）
 *     · 用上一请求缓存下来的上下文
 *   即：**解析失败 = 拒绝服务**，没有第三种选择。
 */
export type ResolveTenant = (hint: TenantHint) => Promise<TenantResolution>;

/**
 * 租户标识的来源提示。
 *
 * 多来源是 **兼容性**需求（既有单租户部署要继续跑），
 * 但优先级必须确定，否则「两个来源给出不同租户」时行为未定义 ——
 * 那正是越权漏洞的温床。优先级见 TENANT_HINT_PRECEDENCE。
 */
export interface TenantHint {
  /** ① 子域名（商用最常用：acme.spark.example.com） */
  host?: string;
  /** ② 显式请求头（API 集成方用） */
  header?: string;
  /** ③ 会话/JWT 里的租户声明（登录用户用） */
  claim?: string;
  /** ④ 环境/部署级兜底（单租户私有化部署用） */
  envDefault?: string;
}

/**
 * ★★ 提示来源优先级 —— 确定性是安全前提。
 *
 * 从高到低：
 *   1. claim  —— 已认证会话里的租户声明**最可信**（签名保护，用户改不了）
 *   2. header —— 显式头，供服务端集成
 *   3. host   —— 子域名
 *   4. envDefault —— 仅单租户私有化部署场景
 *
 * ★ 为什么 claim 高于 host：host 是**用户可影响**的输入（改 Host 头、
 *   DNS 污染、误配的反代），而 claim 由服务端签发。
 *   把用户可影响的输入排在已签名输入之前，等于把租户边界交给用户决定。
 *
 * ★ 为什么 envDefault 排在**最后**且仅作兜底：它一旦生效，
 *   所有未带标识的请求都会落到同一个租户上。若某个多租户部署
 *   误设了它，结果不是报错而是**静默地让所有人进同一家公司**。
 *   所以它必须是最低优先级，且一旦生效应写审计。
 */
export const TENANT_HINT_PRECEDENCE: readonly (keyof TenantHint)[] = [
  "claim",
  "header",
  "host",
  "envDefault",
];

/**
 * 从 host 抽取子域名的函数签名。
 * 返回 null 表示「这个 host 里没有租户子域名」（如裸域名、IP、localhost）。
 */
export type ExtractTenantFromHost = (host: string) => string | null;

/**
 * 需要租户隔离的资源类别。
 *
 * ★ 列成显式清单而不是「全都要隔离」的口号：
 *   口号无法被测试。清单能 —— 每一条都会有一个对应的越权测试。
 */
export type TenantScopedResource =
  | "business_data" // 事实表 / 预计算桶（业务数据本身）
  | "user_settings" // 视图模板 / 用户偏好 / 列配置
  | "audit_log" // 审计（必须按租户可查、且跨租户不可见）
  | "query_cache" // 查询缓存（键必须含 tenantId）
  | "entitlements" // 账号授权
  | "groups" // 用户分组
  | "strategy_history"; // 策略决策历史

/** 全部需隔离资源（有序）。 */
export const TENANT_SCOPED_RESOURCES: readonly TenantScopedResource[] = [
  "business_data",
  "user_settings",
  "audit_log",
  "query_cache",
  "entitlements",
  "groups",
  "strategy_history",
];

/**
 * ★★ 缓存键构造 —— 隔离最容易被漏掉的一处。
 *
 * 真实风险：查询缓存键若只由「查询语句 + 参数」构成，
 * 租户 A 与租户 B 发同样的查询就会**命中同一个缓存条目** ——
 * 于是 B 拿到 A 的数字。这类 bug 不会报错、不会告警，
 * 只会让报表「偶尔」显示别人的数据，且极难复现。
 *
 * 因此把「键必须含 tenantId」提升为契约的一部分，
 * 并提供一个**唯一**的构造函数，禁止各处自行拼键。
 */
export const CACHE_KEY_SEPARATOR = "\u001f"; // 不可见字符，避免内容拼接歧义

/**
 * 构造租户隔离的缓存键。
 *
 * ★ 强制把 tenantId 放在**最前**：即使未来有人改成「按查询语句前缀分片」，
 *   租户维度仍然是最外层的分区键，不会退化。
 */
export function tenantCacheKey(tenantId: string, ...parts: string[]): string {
  if (!tenantId) {
    // 没有租户就不允许产生缓存键 —— fail-closed。
    // 返回空串会让调用方以「永远 miss」的方式安全退化。
    return "";
  }
  return [tenantId, ...parts].join(CACHE_KEY_SEPARATOR);
}
