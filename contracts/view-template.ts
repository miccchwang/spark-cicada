/**
 * contracts/view-template.ts —— v1.0
 *
 * 视图模板（M-TEMPLATE）契约。
 *
 * 用户可**自命名**保存当前页面设置（筛选状态 + 列偏好 + 布局），形成可复用的模板。
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
