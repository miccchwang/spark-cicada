/**
 * contracts/user-group.ts —— v1.0
 *
 * 用户分组契约（D12，2026-10-05 第二轮拍板新增）。
 *
 * 分组是**批量授权的载体**：把「同一批人的共同权限」抽出来，
 * 避免逐个账号重复勾选。与「角色模板」互补——
 *   模板定「新账号默认勾什么」；分组定「哪些人共享额外权限」。
 *
 * 规则：
 *   - 一人多组，授权取并集
 *   - 组不含层级（层级由 D13 的职权链承担）
 *   - 组授权与个人勾选冲突 → DENY 优先
 *   - 成员可「退出继承」（局部排除）以处理例外
 *
 * 详见 docs/08-分组与权限申请设计.md §2
 */

export const USER_GROUP_VERSION = "1.0" as const;

export interface UserGroup {
  v: typeof USER_GROUP_VERSION;
  id: string;                     // grp_xxx
  name: string;                   // 「华东渠道组」
  description?: string;
  /** 组级授权：成员自动继承（取并集） */
  grants: GroupGrants;
  /** 可管理本组成员与授权的账号 */
  owners: string[];
  createdAt: string;
  updatedAt: string;
}

export interface GroupGrants {
  modules: string[];                       // module.* ID
  dimensions: GroupDimensionGrant[];       // 维度 + 取值
  maxLevel: "L1" | "L2" | "L3" | "L4";
  /**
   * 是否允许查看业务数值。
   * 对 IT 分组**恒为 false**（D7=取消，不可勾选）。
   */
  canViewBusinessValues: boolean;
}

export interface GroupDimensionGrant {
  dim: string;                 // brand / channel / store / …
  values: string[];            // 空 = 全部（受密级约束）
  includeDescendants: boolean;
}

export interface GroupMembership {
  account: string;
  groupId: string;
  joinedAt: string;
  /** 组内角色 */
  role: "member" | "owner";
  /**
   * 是否加入组授权继承。
   * false = 该成员虽在组内，但不继承组授权（用于例外处理）。
   */
  inheritsGrants: boolean;
}

/** 分组授权求值结果（用于「加入该组后能看到什么」预览） */
export interface GroupEntitlementPreview {
  groupId: string;
  /** 组携带的授权并集 */
  modules: string[];
  dimensions: Record<string, string[]>;
  maxLevel: "L1" | "L2" | "L3" | "L4";
  canViewBusinessValues: boolean;
  /** 示例成员：某个账号加入后实际将看到的权限 */
  sampleAccount?: string;
  sampleView?: Record<string, unknown>;
}
