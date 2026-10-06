// Package tenant —— 多租户身份解析与隔离档位（Go 侧契约镜像 + 解析器）。
//
// 对齐 contracts/tenant.ts（版本号必须一致，见 TenantContractVersion）。
//
// ══════════════════════════════════════════════════════════════════════════
// ★★ 本包存在的唯一理由：**把「解析不出租户」变成一件不可能被忽略的事。**
//
//	越权最常见的样子不是「绕过了权限判断」，而是：
//	  有人写了个新 handler，忘了带租户条件；
//	  或者带了，但查询失败时 `if err != nil { 走兜底分支 }`，
//	  而兜底分支查的是**全表**。
//	前者靠测试拦，后者靠**类型系统**拦 —— 后者才是本包的设计重点。
//
//	所以这里不提供「可能为 nil 的 *Context」这种签名。
//	解析结果只有两种：给你一个可用的上下文，或者给你一个拒绝。
//	没有第三种「空上下文」—— 因为「空」迟早被当成「全部」。
//
// ══════════════════════════════════════════════════════════════════════════
package tenant

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// TenantContractVersion 与 contracts/tenant.ts 的 TENANT_CONTRACT_VERSION 对齐。
const TenantContractVersion = "1.0"

// IsolationTier 隔离档位。命名用 shared/dedicated 而非 small/big ——
// 决定档位的是**合规要求**，不是公司规模。
type IsolationTier string

const (
	// TierShared 共享表 + 每行 tenant_id + RLS 行级隔离。
	// 适合长尾租户：边际存储/连接成本≈0。
	TierShared IsolationTier = "shared"
	// TierDedicated 独立 schema（物理可证明隔离）。适合大客户/合规敏感。
	TierDedicated IsolationTier = "dedicated"
)

// Valid 判断档位是否为已知值。
//
// ★ 必须显式校验：从 DB 读出的字符串若拼错（如 "dedicatd"），
//
//	不校验的结果是它既不等于 shared 也不等于 dedicated，
//	于是**两个 if 分支都不进** —— 若代码写成 `if tier == shared {加 tenant 条件}`，
//	拼错的档位就静默跳过了隔离。这类「值域外的值」是 fail-open 的经典入口。
func (t IsolationTier) Valid() bool {
	return t == TierShared || t == TierDedicated
}

// TenantStatus 租户生命周期状态。
type TenantStatus string

const (
	StatusProvisioning TenantStatus = "provisioning"
	StatusActive       TenantStatus = "active"
	StatusSuspended    TenantStatus = "suspended"
	StatusClosed       TenantStatus = "closed"
)

// Serveable 判断该状态是否**允许服务业务查询**。
//
// ★ 只有 active 可服务。provisioning（schema 没建好）与 suspended（被停用）
//
//	都必须拒绝 —— 若把 provisioning 放行，租户会在「表还不存在」时收到
//	一堆内部错误；若把 suspended 放行，停用一家欠费公司的动作就**不生效**。
func (s TenantStatus) Serveable() bool { return s == StatusActive }

// Tenant 租户记录（平台库 dim_tenant 的投影）。
type Tenant struct {
	ID     string        `json:"id"`
	Code   string        `json:"code"`
	Name   string        `json:"name"`
	Tier   IsolationTier `json:"tier"`
	Status TenantStatus  `json:"status"`
	// SchemaName dedicated 档的物理 schema 名；shared 档**必须**为 nil。
	//
	// ★ 用 *string 而不是 string：空串会让「shared 档」与
	//   「dedicated 档但忘了建 schema」无法区分，而后者是真缺陷。
	SchemaName *string     `json:"schemaName"`
	CreatedAt  string      `json:"createdAt"`
	Quota      TenantQuota `json:"quota"`
}

// TenantQuota 租户配额。共享档下「一个租户拖垮全平台」必须被有界化。
type TenantQuota struct {
	MaxAccounts      int   `json:"maxAccounts"`
	MaxRows          int64 `json:"maxRows"`
	MaxQueriesPerMin int   `json:"maxQueriesPerMin"`
	AllowExport      bool  `json:"allowExport"`
}

// DefaultTenantQuota 缺省配额（保守值）。
var DefaultTenantQuota = TenantQuota{
	MaxAccounts:      50,
	MaxRows:          5_000_000,
	MaxQueriesPerMin: 600,
	AllowExport:      true,
}

// TenantContext 一次请求解析出来的「我是谁、我的数据在哪」。
//
// ★ 请求级对象，**绝不可**跨请求缓存复用 —— 缓存它等于把租户身份
//
//	泄漏给下一个请求（尤其在有连接池/goroutine 复用的服务里）。
type TenantContext struct {
	TenantID string        `json:"tenantId"`
	Tier     IsolationTier `json:"tier"`
	// SchemaName dedicated 档必填；shared 档为 nil。
	SchemaName *string `json:"schemaName"`
	// RLSTenantID 是 shared 档 RLS 策略的**唯一输入**（会话变量 app.tenant_id）。
	//
	// ★ 它必须有值（shared 档也一样）。解析不出 tenantId 时直接拒绝请求，
	//   绝不降级 —— 让 current_setting 求值为空是历史上最危险的写法之一。
	RLSTenantID string `json:"rlsTenantId"`
}

// RejectReason 拒绝原因。
//
// ★ 可写日志，但**不得原样回给调用方**：
//
//	not_found 与 not_active 的区别对攻击者是「这个 ID 存在吗」的探测信号。
type RejectReason string

const (
	ReasonMissing       RejectReason = "missing"
	ReasonMalformed     RejectReason = "malformed"
	ReasonNotFound      RejectReason = "not_found"
	ReasonNotActive     RejectReason = "not_active"
	ReasonQuotaExceeded RejectReason = "quota_exceeded"
	ReasonInternal      RejectReason = "internal"
)

// ErrNoTenant 是「解析不出租户」的哨兵错误，供上层统一映射为 401/403。
var ErrNoTenant = errors.New("tenant: 无法解析租户，拒绝服务")

// Resolution 解析结果 —— 判别联合，**故意不提供**「空上下文」形态。
//
// ★ 为什么不用 (TenantContext, bool) 或 (*TenantContext, error)：
//
//	`if ok { 用 } else { ??? }` 的 else 分支迟早会被人写成
//	「回退到默认租户」。而 `if res.Rejected() { 拒绝 }` 的写法里，
//	调用方**必须先处理拒绝**才能拿到 context —— 顺序被类型强制了。
type Resolution struct {
	// Context 仅在 Allowed() 为 true 时非 nil。
	context *TenantContext
	// Reason 仅在 Allowed() 为 false 时非空。
	reason RejectReason
}

// Allowed 是否放行。
func (r Resolution) Allowed() bool { return r.context != nil }

// Context 取上下文。**仅在 Allowed() 为 true 时调用**。
//
// ★ 返回 (值, error) 而不是裸值：若调用方没检查 Allowed() 就取，
//
//	拿到的是 error 而不是一个空上下文 —— 空上下文会一路传到查询层，
//	变成「WHERE tenant_id = ''」，那查不到任何数据（安全）但也掩盖了 bug；
//	而 error 会立刻在调用点炸出来（更好）。
func (r Resolution) Context() (TenantContext, error) {
	if r.context == nil {
		return TenantContext{}, fmt.Errorf("%w: %s", ErrNoTenant, r.reason)
	}
	return *r.context, nil
}

// Reason 返回拒绝原因（仅 Allowed()==false 时有意义）。
func (r Resolution) Reason() RejectReason { return r.reason }

// Resolved 构造放行结果。
func Resolved(ctx TenantContext) Resolution { return Resolution{context: &ctx} }

// Rejected 构造拒绝结果。
func Rejected(reason RejectReason) Resolution { return Resolution{reason: reason} }

// Hint 租户标识的来源提示。
type Hint struct {
	Host       string
	Header     string
	Claim      string
	EnvDefault string
}

// HintPrecedence 提示来源优先级（从高到低），对齐契约的 TENANT_HINT_PRECEDENCE。
//
// ★ claim 高于 host 的理由：host 是**用户可影响**的输入（改 Host 头、
//
//	DNS 污染、误配反代），claim 由服务端签发。把用户可影响的输入
//	排在已签名输入之前，等于把租户边界交给用户决定。
//
// ★ envDefault 排最后且仅兜底：它一旦生效，所有未带标识的请求都落到
//
//	同一租户。多租户部署误设它不是报错，而是**静默地让所有人进同一家公司**。
var HintPrecedence = []string{"claim", "header", "host", "envDefault"}

// uuidRe 匹配规范 uuid（大小写不敏感）。
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID 判断是否为规范 uuid 形态。
func IsUUID(s string) bool { return uuidRe.MatchString(strings.TrimSpace(s)) }

// LookupFunc 按 uuid 查租户。返回 (nil, nil) 表示不存在（而非错误）。
type LookupFunc func(ctx context.Context, tenantID string) (*Tenant, error)

// Resolver 解析租户。
type Resolver struct {
	// Lookup 由调用方注入（平台库查询）。nil ⇒ 一律拒绝。
	Lookup LookupFunc
	// DisallowHost 为 true 时**不认** host 来源。
	//
	// ★ 生产环境应当置 true（默认）：
	//   Host 头是用户完全可控的输入。若允许用 Host 定租户，
	//   攻击者只要把 Host 改成别人的子域名（或用 Host 注入绕过反代），
	//   就可能落进别家租户的上下文 —— 而这一步**发生在认证之前**，
	//   没有任何签名保护。允许它需要配套的可信反代（剥离/重写 Host），
	//   那是部署前提，不该由本函数默认假设。
	DisallowHost bool
	// LastSource 记录上一次解析采用的来源（claim/header/host/envDefault）。
	//
	// ★ 仅用于**审计与排障**，绝不用于业务判断。
	//   记录它的理由：当出现「查到了别家的数据」时，第一个要回答的问题
	//   就是「租户身份是从哪来的」—— 没有这个字段就无从查起。
	LastSource string
}

// hostSrcAllowed 判断该来源是否被策略允许。
func (r *Resolver) hostSrcAllowed(src string) bool {
	if src == "host" && r.DisallowHost {
		return false
	}
	return true
}

// ExtractFromHost 从 host 抽取租户短码。返回空串表示无租户子域名。
type ExtractFromHost func(host string) string

// Resolve 按优先级解析租户并**校验档位/状态一致性**。
//
// 失败一律返回 Rejected，**绝不**回退默认租户。
func (r *Resolver) Resolve(ctx context.Context, h Hint) Resolution {
	// 按优先级取第一个非空提示
	raw, src := pickHint(h)
	if raw == "" {
		return Rejected(ReasonMissing)
	}
	// 提示必须解析成 uuid（短码 → uuid 的映射由 Lookup 负责；这里先做形态校验）
	id := strings.TrimSpace(raw)
	if !IsUUID(id) {
		return Rejected(ReasonMalformed)
	}
	if !r.hostSrcAllowed(src) {
		// 该来源被策略禁用（如生产环境禁止用 Host 头定租户）⇒ 拒绝
		return Rejected(ReasonMissing)
	}
	if r.Lookup == nil {
		// 没有查询能力 ⇒ 拒绝。**不是**「放行但不过滤」。
		return Rejected(ReasonInternal)
	}
	t, err := r.Lookup(ctx, id)
	if err != nil {
		return Rejected(ReasonInternal)
	}
	if t == nil {
		return Rejected(ReasonNotFound)
	}
	if !t.Status.Serveable() {
		return Rejected(ReasonNotActive)
	}
	if !t.Tier.Valid() {
		// 档位值域外的值 ⇒ 拒绝。见 IsolationTier.Valid 的说明。
		return Rejected(ReasonInternal)
	}
	// 档位与 schema 的一致性（**关键不变量**）
	//
	// ★ dedicated 必须带 schema：否则「独立档」实际上落在共享表里，
	//   而调用方以为自己有物理隔离 —— 这会导致它**不再依赖 RLS**，
	//   于是隔离彻底失效（既没有物理隔离，也没开行级隔离）。
	if t.Tier == TierDedicated && (t.SchemaName == nil || *t.SchemaName == "") {
		return Rejected(ReasonInternal)
	}
	// ★ shared 必须**不带** schema：若带了，说明配置混乱（到底走哪条路？），
	//   此时宁可拒绝也不要猜。
	if t.Tier == TierShared && t.SchemaName != nil && *t.SchemaName != "" {
		return Rejected(ReasonInternal)
	}

	r.LastSource = src
	return Resolved(TenantContext{
		TenantID:    t.ID,
		Tier:        t.Tier,
		SchemaName:  t.SchemaName,
		RLSTenantID: t.ID, // shared 档的 RLS 输入；dedicated 档无害
	})
}

// pickHint 按优先级取第一个非空提示，返回 (值, 来源名)。
func pickHint(h Hint) (string, string) {
	for _, k := range HintPrecedence {
		switch k {
		case "claim":
			if s := strings.TrimSpace(h.Claim); s != "" {
				return s, k
			}
		case "header":
			if s := strings.TrimSpace(h.Header); s != "" {
				return s, k
			}
		case "host":
			if s := strings.TrimSpace(h.Host); s != "" {
				return s, k
			}
		case "envDefault":
			if s := strings.TrimSpace(h.EnvDefault); s != "" {
				return s, k
			}
		}
	}
	return "", ""
}

// ───────────────────────────── 共享档 RLS 的会话设置 ─────────────────────────────

// SetLocalTenantSQL 是 shared 档必须执行的会话变量设置语句。
//
// ★ 用 set_config(..., true) 即 `SET LOCAL` 语义：只在**当前事务**内生效，
//
//	事务结束自动失效。绝不用 `SET`（会话级）—— 连接池会把同一条连接
//	复用给下一个请求，会话级的租户变量会**串租**。
//
// ★ 参数化（$1）而不是字符串拼接：拼接会让 tenant_id 成为注入点。
const SetLocalTenantSQL = `SELECT set_config('app.tenant_id', $1, true)`

// SearchPathSQL 是 dedicated 档切换 schema 的语句。
//
// ★ 同样用 LOCAL 语义（第三个参数 true）。并且**必须**在事务内执行：
//
//	若在事务外执行 SET LOCAL，它会自动失效（无声无息），
//	查询就落回 public schema —— 即别的租户的数据。
//
// ★ dedicated 档的 schema 名必须经 QuoteIdent 校验后再拼入（见 SanitizeSchema）。
const SearchPathSQL = `SELECT set_config('search_path', $1, true)`

// schemaNameRe 限定 schema 名只含小写字母、数字与下划线，且不以数字开头。
//
// ★ 为什么在**契约层**就卡死（而不是信任建租户时的校验）：
//
//	search_path 是 DDL 级别的标识符，无法参数化 —— 只能拼字符串。
//	拼字符串就必须有白名单。若只在校验建租户时检查一次，
//	日后有人手工 INSERT 一行 dim_tenant（或从旧备份恢复）就能绕过。
//	这里每次使用前校验，成本可忽略。
var schemaNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// tenantSchemaPrefix 是 dedicated 档 schema 名的强制前缀。
//
// ★ 强制前缀的目的不是美观，而是**让越界的 schema 名无法伪装成租户 schema**：
//
//	若不加前缀，"public" 这种名字就可能是合法的租户 schema 名，
//	于是某天一个租户的 schema 叫 public —— 它在所有连接里都是默认可见的。
const tenantSchemaPrefix = "t_"

// SanitizeSchema 校验并返回 dedicated 档的 schema 名。
//
// 返回 error ⇒ 调用方**必须拒绝请求**（不得回退到 public）。
func SanitizeSchema(name string) (string, error) {
	s := strings.TrimSpace(name)
	if s == "" {
		return "", errors.New("tenant: schema 名为空")
	}
	if !schemaNameRe.MatchString(s) {
		return "", fmt.Errorf("tenant: schema 名不合规（仅允许 [a-z][a-z0-9_]{0,62}）：%q", s)
	}
	if !strings.HasPrefix(s, tenantSchemaPrefix) {
		return "", fmt.Errorf("tenant: schema 名必须以 %q 开头：%q", tenantSchemaPrefix, s)
	}
	// 保留名兜底：即使用户手工插入了这些名字也拒绝。
	switch s {
	case "public", "pg_catalog", "information_schema", "pg_toast":
		return "", fmt.Errorf("tenant: 拒绝保留 schema 名：%q", s)
	}
	return s, nil
}

// SchemaNameFor 由租户 uuid 生成 dedicated 档 schema 名。
//
// ★ 由 uuid 派生（而非用户提供的短码）：短码可改、可重复、可含恶意字符；
//
//	uuid 不可变且长度固定，派生出的名字天然满足白名单。
func SchemaNameFor(tenantID string) (string, error) {
	if !IsUUID(tenantID) {
		return "", fmt.Errorf("tenant: 租户 ID 不是 uuid：%q", tenantID)
	}
	// 去掉连字符并截断到 16 位：够唯一（64 bit 空间）、够短（schema 名上限 63）
	hexOnly := strings.ReplaceAll(strings.ToLower(tenantID), "-", "")
	if len(hexOnly) > 16 {
		hexOnly = hexOnly[:16]
	}
	name := tenantSchemaPrefix + hexOnly
	return SanitizeSchema(name)
}
