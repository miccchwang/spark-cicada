// Package template —— M-TEMPLATE（视图模板中心）的纯逻辑层。
//
// 职责：把一个「被持久化的 QueryState + 视图偏好」建模为可保存/套用/分享的模板。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本模块的核心不是 CRUD，而是**档位可见性**。
//
//   模板三档（personal / team / system）本质是三种授权范围：
//     personal  仅 owner 本人可见
//     team      owner 所在组/部门可见（或显式分享给指定组/部门）
//     system   全体可见
//
//   一旦「可见性」判断写错，用户之间的习惯设置会互相泄露，甚至出现
//   「套用了别人团队的筛选口径」，被误当成公司口径去汇报 —— 这是真实事故。
//   因此可见性判断写成**纯函数** `CanRead`，可被单测穷举，不藏在 SQL 里。
//
//   第二条纪律：**模板只管「怎么看」，永不含「能看什么」**。
//   模板里的 QueryState 可以请求任意维度，但最终能不能返回数据，
//   由权限求值（M-GROUP）与门控（gate）说了算 —— 模板不能成为越权跳板。
//   写成注释是为了让后来者别把 grants 塞进模板。
// ══════════════════════════════════════════════════════════════════════════
package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Scope 模板档位。
type Scope string

const (
	// ScopePersonal 个人档：仅创建者可见。
	ScopePersonal Scope = "personal"
	// ScopeTeam 团队档：创建者所在组/部门（或显式分享对象）可见。
	ScopeTeam Scope = "team"
	// ScopeSystem 系统档：全体可见，作为默认视图来源。
	ScopeSystem Scope = "system"
)

// Version 模板契约版本（与 contracts/view-template.ts 对齐）。
const Version = "1.0"

// 常见错误：调用方可据此区分「无权限」与「不存在」。
var (
	// ErrNotFound 模板不存在。
	ErrNotFound = errors.New("template: 模板不存在")
	// ErrForbidden 当前账号无权读/写该模板。
	ErrForbidden = errors.New("template: 无权访问该模板")
	// ErrInvalid 模板定义非法（缺名、档位越权等）。
	ErrInvalid = errors.New("template: 模板定义非法")
)

// ColumnPref 列偏好（显示 / 顺序 / 宽度 / 冻结）。
type ColumnPref struct {
	Key     string `json:"key"`
	Visible bool   `json:"visible"`
	Order   int    `json:"order"`
	Width   *int   `json:"width,omitempty"`
	Frozen  bool   `json:"frozen,omitempty"`
}

// LayoutPref 布局偏好（层级展开状态、面板位置）。
type LayoutPref struct {
	// Expanded 层级展开状态，键为层级+数据 key。
	Expanded        map[string]bool `json:"expanded"`
	SidebarCollapsed bool           `json:"sidebarCollapsed,omitempty"`
	PanelPositions   map[string]string `json:"panelPositions,omitempty"`
}

// Share 团队档的显式可见对象。
type Share struct {
	SubjectKind string `json:"subjectKind"` // group | dept
	SubjectID   string `json:"subjectId"`
}

// Template 视图模板。
type Template struct {
	ID   string `json:"id"`
	Name string `json:"name"` // 用户自命名
	// Scope 档位。
	Scope Scope `json:"scope"`
	// Owner 创建者账号。
	Owner string `json:"owner"`
	// Page 所属页面路由（模板按页隔离）。
	Page string `json:"page"`
	// QueryState 完整筛选状态（原样透传；本层不解释其内部结构）。
	//
	// ★ 用 `json.RawMessage` 而非具体类型：QueryState 由前端契约定义，
	//   后端只做「搬运与存储」。若后端也定义一份结构，就会出现两份真相，
	//   协议一改两边必漂移 —— 这正是本项目反复强调要避免的模式。
	//
	// ★ 必须是 json.RawMessage 而非 []byte：[]byte 在 encoding/json 里
	//   会被当作 **base64 字符串**编解码，导致
	//     · 前端发 `"queryState": {...}`（对象）⇒ 反序列化直接失败 400；
	//     · 后端回 `"queryState": "eyJ2IjoiMS4wIn0="`（base64）⇒ 前端拿到乱码。
	//   契约里 queryState 是 `unknown`（对象），所以必须透传原始 JSON。
	QueryState json.RawMessage `json:"queryState"`
	// Columns 列偏好。
	Columns []ColumnPref `json:"columns"`
	// Layout 布局偏好。
	Layout LayoutPref `json:"layout"`
	// Shares 团队档显式分享对象（空 = 跟随 owner 主部门）。
	Shares []Share `json:"shares,omitempty"`
	// UseCount 使用次数（用于常用排序与推荐）。
	UseCount int64 `json:"useCount"`
	// IsDefault 是否为该页默认模板。
	IsDefault bool `json:"isDefault,omitempty"`
	// Version 模板契约版本。
	Version string `json:"v"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Viewer 描述「谁在看作」——可见性判断所需的全部上下文。
//
// ★ 显式传上下文而非让函数去查库：这样 CanRead 是纯函数，
//   可以在测试里穷举所有档位 × 身份组合，不必起库。
type Viewer struct {
	Account string
	// Groups 该账号所属组 id 列表。
	Groups []string
	// Dept 该账号主部门 code（用于 team 档「跟随主部门」判定）。
	Dept string
	// IsSystemAdmin 是否具备系统管理权限（可读写 system 档、可管任意模板）。
	IsSystemAdmin bool
}

// Validate 校验模板定义。返回问题列表（空 = 合法）。
//
// ★ 只校验**结构合法性**，不校验 QueryState 内部（那是前端契约的事）。
func Validate(t *Template) []string {
	var problems []string
	if t == nil {
		return []string{"模板为空"}
	}
	if strings.TrimSpace(t.Name) == "" {
		// 「自命名」是需求明确要求的能力，名字不能为空 ——
		// 空名会让模板列表变成一堆无标识的条目，等于没保存。
		problems = append(problems, "name 不能为空（模板需用户自命名）")
	}
	switch t.Scope {
	case ScopePersonal, ScopeTeam, ScopeSystem:
	default:
		problems = append(problems, fmt.Sprintf("非法档位 %q（应为 personal/team/system）", t.Scope))
	}
	if strings.TrimSpace(t.Owner) == "" {
		problems = append(problems, "owner 不能为空")
	}
	if strings.TrimSpace(t.Page) == "" {
		problems = append(problems, "page 不能为空（模板必须归属某个页面）")
	}
	if len(t.QueryState) == 0 {
		problems = append(problems, "queryState 不能为空（模板的本质就是持久化的 QueryState）")
	}
	// 列偏好：order 不应重复，否则前端排序结果不确定
	seen := map[int]bool{}
	for _, c := range t.Columns {
		if strings.TrimSpace(c.Key) == "" {
			problems = append(problems, "列偏好存在空 key")
			break
		}
		if seen[c.Order] {
			problems = append(problems, fmt.Sprintf("列偏好 order=%d 重复", c.Order))
			break
		}
		seen[c.Order] = true
	}
	for _, s := range t.Shares {
		if s.SubjectKind != "group" && s.SubjectKind != "dept" {
			problems = append(problems, fmt.Sprintf("分享对象 kind 非法 %q", s.SubjectKind))
		}
		if strings.TrimSpace(s.SubjectID) == "" {
			problems = append(problems, "分享对象 id 不能为空")
		}
	}
	return problems
}

// CanRead 判断 viewer 是否可读该模板。
//
// 规则（与 docs/01 §10.1 一致）：
//
//	personal —— 仅 owner 本人（管理员可读，便于治理，但不改变「对普通用户不可见」）
//	team     —— owner 本人 / 管理员 / 命中 shares（组或部门）/
//	            shares 为空时：viewer 与 owner 同部门，或与 owner 有共同组
//	system   —— 全体
//
// ★ DENY 不在这里：模板没有 DENY 概念（它不是授权载体）。
//   但**读得到模板 ≠ 看得到数据** —— 套用后仍要过权限求值与门控。
func CanRead(t *Template, v Viewer) bool {
	if t == nil {
		return false
	}
	if v.IsSystemAdmin {
		return true
	}
	if t.Owner == v.Account {
		return true
	}
	switch t.Scope {
	case ScopeSystem:
		return true
	case ScopePersonal:
		// 个人档：除本人与管理员外，任何人（含同部门）都不可见。
		return false
	case ScopeTeam:
		// 显式分享优先
		if len(t.Shares) > 0 {
			for _, s := range t.Shares {
				switch s.SubjectKind {
				case "dept":
					if v.Dept != "" && v.Dept == s.SubjectID {
						return true
					}
				case "group":
					for _, g := range v.Groups {
						if g == s.SubjectID {
							return true
						}
					}
				}
			}
			return false
		}
		// 无显式分享：跟随 owner 主部门的「同部门可见」语义。
		// 这里只能判断 viewer 侧；owner 的部门由调用方在 v 里对齐
		// （store 层会把 owner 的 dept 填进判断，见 CanReadTeamWithOwnerDept）。
		return false
	default:
		return false
	}
}

// CanReadTeamWithOwnerDept 是 CanRead 的补充：在 team 档且无显式分享时，
// 用 owner 的主部门做「同部门可见」判定。
//
// 之所以拆出来：CanRead 只拿到 viewer 上下文，拿不到 owner 的部门
// （那需要查库）。store 层查到 owner 部门后调本函数，
// 纯函数部分仍可单测。
func CanReadTeamWithOwnerDept(t *Template, v Viewer, ownerDept string) bool {
	if t == nil {
		return false
	}
	if t.Scope != ScopeTeam {
		return CanRead(t, v)
	}
	if len(t.Shares) > 0 {
		return CanRead(t, v)
	}
	if v.IsSystemAdmin || t.Owner == v.Account {
		return true
	}
	return ownerDept != "" && v.Dept == ownerDept
}

// CanWrite 判断 viewer 是否可修改/删除该模板。
//
// ★ 比 CanRead 更严：能**看**别人的团队模板不代表能**改**。
//   改一个 system 档模板会一次性改变所有人的默认视图 —— 权限最高一档。
func CanWrite(t *Template, v Viewer) bool {
	if t == nil {
		return false
	}
	if v.IsSystemAdmin {
		return true
	}
	// 只有创建者本人可改自己的模板。
	if t.Owner == v.Account {
		return true
	}
	return false
}

// CanSetScope 判断 viewer 是否可把模板设为某档位。
//
// personal —— 任何人可建
// team     —— 需为「主管及以上」；由调用方通过 IsSupervisor 注入判断
// system   —— 仅系统管理员
//
// 说明：这里不查库判断 supervior，而是显式接收布尔，
// 保持纯函数；store/api 层负责解析组织关系。
func CanSetScope(scope Scope, v Viewer, isSupervisor bool) bool {
	switch scope {
	case ScopePersonal:
		return true
	case ScopeTeam:
		return v.IsSystemAdmin || isSupervisor
	case ScopeSystem:
		return v.IsSystemAdmin
	default:
		return false
	}
}

// FilterReadable 从 candidates 中筛出 viewer 可读的模板，保持原顺序。
func FilterReadable(candidates []*Template, v Viewer) []*Template {
	out := make([]*Template, 0, len(candidates))
	for _, t := range candidates {
		if CanRead(t, v) {
			out = append(out, t)
		}
	}
	return out
}

// SortByUseCountDesc 按使用次数倒序（常用优先），次数相同则按更新时间新→旧。
//
// ★ 用稳定排序并保留原索引作为最终 tiebreaker，保证同样输入
//   永远产出同样顺序（否则列表会「自己跳动」，用户以为出了 bug）。
func SortByUseCountDesc(ts []*Template) []*Template {
	out := make([]*Template, len(ts))
	copy(out, ts)
	// 简单插入排序（模板列表很小，且需稳定）；避免引入 sort 包的比较器闭包复杂度。
	for i := 1; i < len(out); i++ {
		cur := out[i]
		j := i - 1
		for j >= 0 && less(cur, out[j]) {
			out[j+1] = out[j]
			j--
		}
		out[j+1] = cur
	}
	return out
}

// less 定义「cur 应排在 other 前面」。
func less(cur, other *Template) bool {
	if cur.UseCount != other.UseCount {
		return cur.UseCount > other.UseCount
	}
	if !cur.UpdatedAt.Equal(other.UpdatedAt) {
		return cur.UpdatedAt.After(other.UpdatedAt)
	}
	// 最终 tiebreaker：id 字典序，保证确定
	return cur.ID < other.ID
}

// DefaultsForPage 从候选里挑出「该页默认模板」。
//
// 优先级：
//  1. viewer 本人 personal 档的 is_default
//  2. viewer 可见的 team 档 is_default
//  3. system 档 is_default
//  4. 都没有 ⇒ nil（**不编造**一个默认：没有默认就不套用，
//     而不是「随便挑一个」，否则用户会莫名看到别人的筛选口径）
func DefaultsForPage(candidates []*Template, v Viewer, page string) *Template {
	var personal, team, system *Template
	for _, t := range candidates {
		if t == nil || t.Page != page || !t.IsDefault {
			continue
		}
		if !CanRead(t, v) {
			continue
		}
		switch t.Scope {
		case ScopePersonal:
			// 只认本人的个人默认（CanRead 已保证同 scope 只有 owner 可读）
			if personal == nil && t.Owner == v.Account {
				personal = t
			}
		case ScopeTeam:
			if team == nil {
				team = t
			}
		case ScopeSystem:
			if system == nil {
				system = t
			}
		}
	}
	if personal != nil {
		return personal
	}
	if team != nil {
		return team
	}
	return system
}
