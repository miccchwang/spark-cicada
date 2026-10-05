/**
 * contracts/entitlement.ts —— v1.1
 *
 * 账号授权项（D11 / D12 / D13）契约。
 *
 * 核心变化：权限**不再只来自固定角色模板**；每个账号可**逐项勾选**
 *   - 可访问的模块
 *   - 可访问的数据维度（含取值与下级包含）
 *   - 密级上限（逐级）
 *   - 逐字段覆写
 * 角色模板退化为「预设套餐」，作为勾选的起点。
 *
 * v1.1（二轮修订）新增：
 *   - groups：所属用户分组（D12），组授权自动继承（并集）
 *   - supervisor：上级账号（D13），职权向下覆盖链
 *   - canViewBusinessValues：**D7=取消**，对 IT 角色恒为 false 且不可勾选
 *   - grantedBy：授权来源（区分管理员直接勾选 / 上级代授 / 申请获批）
 *
 * 求值（v1.1）：
 *   最终权限 = 模板起点
 *            ⊕ 分组授权（并集）
 *            ⊕ 逐项勾选
 *            ⊕ 已批准申请
 *            ⊕ 上级代授（受「可授出 ⊆ 自身权限」约束）
 *            ⊖ 显式禁用（DENY 优先）
 *            ⊕ 临时授权（时间盒）
 */

export const ENTITLEMENT_VERSION = "1.1" as const;

export interface Entitlement {
  v: typeof ENTITLEMENT_VERSION;
  account: string;
  /** 角色模板起点；为空表示完全自定义 */
  baseTemplate?: string;
  /** 所属用户分组（D12，可多组；组授权取并集） */
  groups?: string[];
  /** 上级账号（D13，职权向下覆盖链上的直接上级） */
  supervisor?: string;
  /** 可访问模块（勾选） */
  modules: ModuleGrant;
  /** 可访问数据维度（勾选） */
  dimensions: DimensionGrant[];
  /** 密级上限 */
  maxLevel: Level;
  /** 逐字段覆写（个别字段提权/降权） */
  fieldOverrides?: FieldOverride[];
  /**
   * 是否允许查看业务数值。
   * **D7=取消**：IT 角色恒为 false，且 UI 上不可勾选开启。
   * 非 IT 角色由密级 + 维度勾选共同决定，本字段默认 true。
   */
  canViewBusinessValues: boolean;
  /** 临时授权（时间盒，过期自动回收） */
  tempGrants?: TempGrant[];
  /** 授权来源（审计用） */
  grants?: GrantRecord[];
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

/**
 * 授权来源记录：每条授权从哪来、谁授的、上界是谁。
 * 用于审计与「可授出 ⊆ 自身权限」的不变量校验。
 */
export interface GrantRecord {
  origin: "TEMPLATE" | "GROUP" | "ADMIN_CHECK" | "SUPERVISOR_DELEGATE" | "REQUEST_APPROVED" | "TEMP";
  /** 授出者账号；TEMPLATE/GROUP 可为系统 */
  grantedBy: string;
  /** 授出者自身在该 scope 的上界（用于校验不溢出） */
  upperBoundRef?: string;
  at: string;
  /** 关联的申请单 ID（origin = REQUEST_APPROVED 时） */
  requestId?: string;
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
    fromGroups: string[];
    fromGrant: string[];
    fromApprovedRequests: string[];
    fromDelegations: string[];
    fromTemp: string[];
    deniedBy: string[];
  };
}

/**
 * 求值函数签名。
 * 输入：账号 Entitlement + 可选模板 + 分组授权集合。
 * 输出：投影后的 EntitlementView。
 * 约束：任何一步都不得突破「可授出 ⊆ 自身权限」的不变量。
 */
export type ResolveEntitlement = (
  e: Entitlement,
  template?: Partial<Entitlement>,
  groupGrants?: Partial<Entitlement>[]
) => EntitlementView;

/**
 * 授权上界校验（D13 核心不变量）。
 * 返回 true 表示 grantee 的权限集合 ⊆ granter 的权限集合，允许授予。
 */
export type CheckDelegationBound = (
  granter: EntitlementView,
  grantee: EntitlementView
) => boolean;
