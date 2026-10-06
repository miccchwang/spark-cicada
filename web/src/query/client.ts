/**
 * src/query/client.ts —— M-QUERY（前端翻译层）
 *
 * 职责：QueryState → HTTP 请求 → DataContract。
 *
 * ★★ 边界纪律（G1，由 scripts/layering-gate.mjs 静态强制）：
 *   1. 本模块**不含业务公式**：不得出现 gp/cogs/gmp/net_contrib 的算术。
 *      一切数值由后端（compute 内核 Rust）产出，本层只搬运。
 *   2. 本模块**不引入渲染层**（render/）：只返回 DataContract，不构造 VM。
 *   3. 分页/排序/过滤只做**翻译**与下推，不做重算。
 *
 * fail-closed 纪律：
 *   * 后端任何错误（500 store not configured / STALE 桶 / 403）**原样上抛**，
 *     绝不返回「空数据 + 0」让上层误以为是「数据为 0」。
 *   * 响应缺 `v` 或版本不符 ⇒ 拒绝（当作协议错误），不猜。
 */

import { DATA_CONTRACT_VERSION, type DataContract } from "../contracts/data-contract.js";
import type { QueryState } from "../contracts/query-state.js";
import { tenantCacheKey } from "../contracts/tenant.js";

/** 查询失败时抛出的结构化错误（供 UI 精确展示原因）。 */
export class QueryError extends Error {
  readonly status: number;
  readonly reason: QueryErrorReason;
  readonly detail: string;

  constructor(reason: QueryErrorReason, status: number, detail: string) {
    super(`[${reason}] ${detail}`);
    this.name = "QueryError";
    this.reason = reason;
    this.status = status;
    this.detail = detail;
  }
}

export type QueryErrorReason =
  | "STORE_NOT_CONFIGURED" // 后端 DB 缺席（降级模式）
  | "BUCKET_NOT_FRESH" // 桶 STALE / 未注册（fail-closed）
  | "FORBIDDEN" // 无权限
  | "UNAUTHENTICATED" // 缺身份
  | "MALFORMED_REQUEST" // QueryState 非法
  | "PROTOCOL_MISMATCH" // 响应版本不符
  | "NETWORK" // 网络层失败
  | "UNKNOWN";

/** 把 HTTP 响应映射为结构化原因（决定 UI 提示文案）。 */
function classify(status: number, body: string): QueryErrorReason {
  const b = body.toLowerCase();
  if (status === 401) return "UNAUTHENTICATED";
  if (status === 403) return "FORBIDDEN";
  if (status === 400) return "MALFORMED_REQUEST";
  if (status === 500 || status === 503) {
    if (b.includes("store not configured")) return "STORE_NOT_CONFIGURED";
    if (b.includes("bucket") || b.includes("stale") || b.includes("no usable")) return "BUCKET_NOT_FRESH";
  }
  return "UNKNOWN";
}

export interface QueryClientOptions {
  /** sparkd 基址（同源可留空）。 */
  baseUrl?: string;
  /** 身份注入（生产由网关/SSO 注入；开发期用）。 */
  account?: string;
  /**
   * 租户标识（多租户部署必填）。
   *
   * ★★ 为什么它必须参与**缓存键**（Task #57）：
   *
   *   本地缓存若只以 queryHash 为键，同一浏览器在「切换租户」后
   *   （或同一 SPA 内嵌多个租户视图时）会命中**上一个租户**的结果 ——
   *   这是纯前端的串租户，后端再严的 RLS 也拦不住（请求根本没发出去）。
   *   症状是「切了租户还看到旧数据」，且刷新即好，极难复现。
   *
   * ★ 空租户 ⇒ 缓存键为空 ⇒ 不缓存（fail-closed），与后端契约一致。
   */
  tenantId?: string;
  /** 自定义 fetch（测试注入）。 */
  fetchImpl?: typeof fetch;
}

/** M-QUERY 客户端。 */
export class QueryClient {
  private readonly baseUrl: string;
  private readonly account: string;
  private readonly tenantId: string;
  private readonly fetchImpl: typeof fetch;
  /** 租户前缀 + queryHash → DataContract 缓存（与后端缓存键同源，保证幂等）。 */
  private readonly cache = new Map<string, DataContract>();

  constructor(opts: QueryClientOptions = {}) {
    this.baseUrl = opts.baseUrl ?? "";
    this.account = opts.account ?? "";
    this.tenantId = opts.tenantId ?? "";
    this.fetchImpl = opts.fetchImpl ?? globalThis.fetch.bind(globalThis);
  }

  /**
   * 翻译 QueryState 为请求体。
   *
   * 纯函数：只做字段搬运与清理（去掉 undefined），不做换算。
   */
  static toRequestBody(qs: QueryState): string {
    return JSON.stringify(qs);
  }

  /**
   * 执行查询。
   *
   * @param qs        由 M-FILTER 产出的 QueryState
   * @param cacheKey  可选：由 qs 规范化得到的哈希（用于本地缓存）
   */
  async run(qs: QueryState, cacheKey?: string): Promise<DataContract> {
    // ★ 缓存键 = 租户前缀 + qs 哈希。租户为空 ⇒ 键为空 ⇒ 不读也不写缓存。
    //   这一行是前端串租户的唯一防线，绝不可省成 cacheKey 直接使用。
    const scopedKey = cacheKey ? tenantCacheKey(this.tenantId, cacheKey) : "";

    if (scopedKey) {
      const hit = this.cache.get(scopedKey);
      if (hit) return hit;
    }

    const headers: Record<string, string> = { "Content-Type": "application/json" };
    if (this.account) headers["X-Spark-Account"] = this.account;
    if (this.tenantId) headers["X-Spark-Tenant"] = this.tenantId;

    let res: Response;
    try {
      res = await this.fetchImpl(`${this.baseUrl}/api/query`, {
        method: "POST",
        headers,
        body: QueryClient.toRequestBody(qs),
      });
    } catch (e) {
      // 网络层失败：不吞、不造空数据
      throw new QueryError("NETWORK", 0, e instanceof Error ? e.message : String(e));
    }

    const text = await res.text();

    if (!res.ok) {
      // ★ fail-closed：原样上抛，绝不返回空 DataContract
      throw new QueryError(classify(res.status, text), res.status, text.trim().slice(0, 500));
    }

    let dc: DataContract;
    try {
      dc = JSON.parse(text) as DataContract;
    } catch (e) {
      throw new QueryError("PROTOCOL_MISMATCH", res.status, `响应非 JSON: ${String(e)}`);
    }

    // 协议校验：版本必须匹配，否则拒绝（不猜字段）
    if (!dc || typeof dc !== "object" || dc.v !== DATA_CONTRACT_VERSION) {
      throw new QueryError(
        "PROTOCOL_MISMATCH",
        res.status,
        `DataContract 版本不符（期望 ${DATA_CONTRACT_VERSION}，得到 ${String((dc as { v?: unknown })?.v)}）`,
      );
    }

    if (scopedKey) this.cache.set(scopedKey, dc);
    return dc;
  }

  /** 清空本地缓存（例如用户权限变更后）。 */
  clearCache(): void {
    this.cache.clear();
  }
}

// ───────────────────────────── 身份视图（/api/me） ─────────────────────────────

/** 当前账号权限视图（与后端 authz.EntitlementView 对齐）。 */
export interface MeView {
  account: string;
  modules: string[];
  dimensions: Record<string, string[]>;
  maxLevel: "L1" | "L2" | "L3" | "L4";
  canViewBusinessValues: boolean;
  dataUseGroups: string[];
}

/** 读取当前身份视图（用于渲染勾选树与前端二次门控）。 */
export async function fetchMe(opts: QueryClientOptions = {}): Promise<MeView> {
  const fetchImpl = opts.fetchImpl ?? globalThis.fetch.bind(globalThis);
  const headers: Record<string, string> = {};
  if (opts.account) headers["X-Spark-Account"] = opts.account;

  let res: Response;
  try {
    res = await fetchImpl(`${opts.baseUrl ?? ""}/api/me`, { headers });
  } catch (e) {
    throw new QueryError("NETWORK", 0, e instanceof Error ? e.message : String(e));
  }
  const text = await res.text();
  if (!res.ok) {
    throw new QueryError(classify(res.status, text), res.status, text.trim().slice(0, 300));
  }
  return JSON.parse(text) as MeView;
}
