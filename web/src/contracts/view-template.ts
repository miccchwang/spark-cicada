/**
 * src/contracts/view-template.ts —— 视图模板契约镜像
 *
 * 唯一真源在仓库根 contracts/view-template.ts。此处为 web 构建期的类型镜像，
 * 二者字段必须逐字一致（由 test/contracts.test.mjs 断言，防止漂移）。
 *
 * 纪律：
 *  1. 纯可序列化（JSON 安全）：无函数/无 DOM 引用。
 *  2. 模板**就是**一个被持久化的 QueryState + 视图偏好。
 *  3. 模板只描述「怎么看」，**不含**「能看什么」（不含任何权限字段）。
 */

export const VIEW_TEMPLATE_VERSION = "1.0" as const;

export type TemplateScope = "personal" | "team" | "system";

export interface ViewTemplate {
  id: string;
  /** 用户自命名 */
  name: string;
  scope: TemplateScope;
  owner: string;
  /** 所属页面路由 */
  page: string;
  /** 完整筛选状态（复用 QueryState，保证可深链） */
  queryState: unknown; // 见 query-state.ts 的 QueryState
  /** 列显示 / 顺序 / 宽度 / 冻结 */
  columns: ColumnPref[];
  /** 层级展开状态、面板位置等 */
  layout: LayoutPref;
  createdAt: string;
  updatedAt: string;
  /** 使用次数，用于「我的常用」排序与智能推荐 */
  useCount: number;
  /** 是否为该页默认模板 */
  isDefault?: boolean;
}

export interface ColumnPref {
  key: string;
  visible: boolean;
  order: number;
  width?: number;
  frozen?: boolean;
}

export interface LayoutPref {
  /** 各层级展开状态（键为层级 + 数据 key） */
  expanded: Record<string, boolean>;
  sidebarCollapsed?: boolean;
  panelPositions?: Record<string, string>;
}
