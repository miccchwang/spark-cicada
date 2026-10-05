/**
 * contracts/entitlement.ts —— v1.0
 *
 * 账号授权项（D11）契约。
 *
 * 核心变化：权限**不再只来自固定角色模板**；每个账号可**逐项勾选**
 *   - 可访问的模块
 *   - 可访问的数据维度（含取值与下级包含）
 *   - 密级上限（逐级）
 *   - 逐字段覆写
 *   - 是否可见业务数值（D7）
 * 角色模板退化为「预设套餐」，作为勾选的起点。
 *
 * 求值：最终权限 = 模板起点 ⊕ 逐项勾选 ⊖ 显式禁用(DENY优先) ⊕ 临时授权。
 */

export const ENTITLEMENT_VERSION = "1.0" as const;

export interface Entitlement {
  v: typeof ENTITLEMENT_VERSION;
  account: string;
  /** 角色模板起点；为空表示完全自定义 */
  baseTemplate?: string;
  /** 可访问模块（勾选） */
  modules: ModuleGrant;
  /** 可访问数据维度（勾选） */
  dimensions: DimensionGrant[];
  /** 密级上限 */
  maxLevel: Level;
  /** 逐字段覆写（个别字段提权/降权） */
  fieldOverrides?: FieldOverride[];
  /** 是否允许查看业务数值（D7）；IT 默认 false */
  canViewBusinessValues: boolean;
  /** 临时授权（时间盒，过期自动回收） */
  tempGrants?: TempGrant[];
}

export type Level = "L1" | "L2" | "L3" | "L4";

export interface ModuleGrant {
  /** 启用的 module.* ID 列表 */
  enabled: string[];
  /** 显式禁用（DENY 优先于启用） */
  disabled: string[];
}

export interface DimensionGrant {
  /** brand / channel / store / category / productStatus / set_id … */
  dim: string;
  /** 允许的取值；空数组 = 全部（仍受密级约束） */
  values: string[];
  /** 勾选父维度是否连带下级（如勾品牌是否连带其渠道/店铺） */
  includeDescendants: boolean;
}

export interface FieldOverride {
  field: string;
  allow: boolean;
}

export interface TempGrant {
  scope: string;
  expiresAt: string;
  grantedBy: string;
}

/** 权限求值结果（供渲染层与后端共用） */
export interface EntitlementView {
  account: string;
  modules: string[];
  dimensions: Record<string, string[]>;
  maxLevel: Level;
  canViewBusinessValues: boolean;
  /** 命中来源，便于审计与排障 */
  source: {
    fromTemplate: string[];
    fromGrant: string[];
    fromTemp: string[];
    deniedBy: string[];
  };
}

/** 密码聚合：把模板 + 勾选 + 临时授权求并，再减去 DENY。 */
export type ResolveEntitlement = (
  e: Entitlement,
  template?: Partial<Entitlement>
) => EntitlementView;
