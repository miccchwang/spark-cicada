// registry.go —— 租户注册表（dim_tenant 的 Go 读取端）。
//
// ══════════════════════════════════════════════════════════════════════════
// ★ 本文件只做一件事：把 dim_tenant 的一行映射成 tenant.Tenant，
//   供 tenant.Resolver 判「是否放行」。
//
//   它**不做**任何隔离决策 —— 隔离由 RLS/search_path 在事务里施加。
//   把注册表当成"授权表"是常见误解：注册表回答的是「这个租户存在吗、
//   它现在允许服务吗、它的数据在哪」；回答不了「张三能不能看这条销售行」，
//   后者是 RLS/权限门控的事。混在一起会制造第二份真相。
// ══════════════════════════════════════════════════════════════════════════
package tenant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Registry 读取 dim_tenant 的只读视图。
//
// ★ 只读：租户的**建立/变更/停用**走 provision 编排器与运维流程
//   （那些操作要建 schema、要迁移，不是一条 INSERT 能完成的）。
//   本类型故意不提供写方法，避免有人以为「INSERT 一行 dim_tenant 就开通了租户」。
type Registry struct {
	pool *pgxpool.Pool
	// cache 可选的租户缓存（TTL 由 cacheTTL 控制）。
	//
	// ★★ 缓存的**唯一**正确用法：缓存「租户元数据」（档位 / schema 名 / 状态），
	//     绝不缓存「解析结果」或「身份」本身。缓存解析结果会让停用/降级
	//     滞后生效（一家欠费公司还能继续查库）。
	//
	// ★ 缓存条目带 expiresAt，读时判过期 —— 而不是靠后台 goroutine 清理。
	//     后者在进程退出/GC 压力下会漏清，且引入一个 goroutine 生命周期问题。
	//     「懒过期」的代价是可能短暂持有过期条目，但读取路径已拦住它。
	//
	// ★ 并发：用 mutex 保护。多租户高并发下这个 map 的读远多于写，
	//     但 RWMutex 的收益在这个规模（租户数是百/千级，不是百万级）不值得复杂度。
	cache    map[string]cachedTenant
	cacheTTL time.Duration
	mu       sync.RWMutex
}

// cachedTenant 是带过期时间的缓存条目。
type cachedTenant struct {
	t         *Tenant
	expiresAt time.Time
}

// NewRegistry 构造注册表（无缓存，每次查库）。
//
// ★ 默认无缓存是**有意的**：缓存是一个正确性风险（见 Registry.cache 注释），
//   应当由调用方显式开启并承担「变更滞后」的后果，而不是默认悄悄开启。
func NewRegistry(pool *pgxpool.Pool) *Registry { return &Registry{pool: pool} }

// NewRegistryWithCache 构造带缓存的注册表。
//
// ttl <= 0 ⇒ 退化为无缓存（fail-safe，而不是"永不过期"）。
//
// ★ 为什么 ttl<=0 不是"永不过期"：一个「永不过期」的租户缓存意味着
//   停用一家租户后，所有节点会一直认为它 active —— 直到进程重启。
//   与其提供这么危险的行为，不如退化为无缓存（每次查库，慢但正确）。
func NewRegistryWithCache(pool *pgxpool.Pool, ttl time.Duration) *Registry {
	if ttl <= 0 {
		return NewRegistry(pool)
	}
	return &Registry{pool: pool, cache: map[string]cachedTenant{}, cacheTTL: ttl}
}

// Invalidate 清空缓存（供"停用/变更租户"后主动失效）。
//
// ★ 单进程缓存天然做不到跨节点失效 —— 这是它只适合短 TTL 的原因。
//   若部署多节点且需要立即失效，应当换成外部缓存（Redis）或直接关掉缓存。
func (r *Registry) Invalidate() {
	if r == nil || r.cache == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = map[string]cachedTenant{}
}

// ErrTenantRegistryUnavailable 表示注册表不可用（池为 nil）。
//
// ★ 单独一个哨兵：解析器需要区分「租户不存在」（not_found）与
//   「注册表本身坏了」（internal）。前者是 404，后者是 503 —— 混在一起
//   会让运维把配置错误当成"用户瞎输 ID"。
var ErrTenantRegistryUnavailable = errors.New("tenant: 注册表不可用")

// Lookup 按 uuid 查租户。返回 (nil, nil) 表示**不存在**（而非错误）。
//
// ★ 契约（与 tenant.LookupFunc 一致）：
//
//	· 不存在 ⇒ (nil, nil)  —— 让 Resolver 报 not_found
//	· 查询失败 ⇒ (nil, err) —— 让 Resolver 报 internal（fail-closed）
//
//	这两种必须分开：若都返回 err，Resolver 无法区分 404/503；
//	若都返回 (nil,nil)，一次 DB 抖动会被当成"租户不存在"，
//	于是所有活跃用户瞬间被登出 —— 一次抖动引发全站 401。
func (r *Registry) Lookup(ctx context.Context, tenantID string) (*Tenant, error) {
	if r == nil || r.pool == nil {
		return nil, ErrTenantRegistryUnavailable
	}
	if !IsUUID(tenantID) {
		// 形态非法 ⇒ 确定不存在，不必查库（也避免把垃圾送进 pg 触发解析开销）。
		return nil, nil
	}
	if t := r.getCached(tenantID); t != nil {
		return t, nil
	}
	t, err := r.query(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	// ★ 只缓存"存在"的租户，**不缓存"不存在"**：
	//   负缓存会让「刚开通的租户在 TTL 内仍不可用」，而开通是低频高价值操作，
	//   这正是最不能出现"等一下才行"的场景。
	if t != nil {
		r.putCached(tenantID, t)
	}
	return t, nil
}

// getCached 读缓存（含懒过期判断）。
func (r *Registry) getCached(tenantID string) *Tenant {
	if r.cache == nil {
		return nil
	}
	r.mu.RLock()
	ce, ok := r.cache[tenantID]
	r.mu.RUnlock()
	if !ok || time.Now().After(ce.expiresAt) {
		return nil
	}
	return ce.t
}

// putCached 写缓存。
func (r *Registry) putCached(tenantID string, t *Tenant) {
	if r.cache == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[tenantID] = cachedTenant{t: t, expiresAt: time.Now().Add(r.cacheTTL)}
}

// query 真正打库。
func (r *Registry) query(ctx context.Context, tenantID string) (*Tenant, error) {
	// ★ 只取需要的列：不要把 quota 这类会演进的结构一次性全 drag 进来，
	//   更不要 SELECT *（加了列就会让扫描错位）。
	//
	// ★ schema_name 用可空 string 接：shared 档它就是 NULL，
	//   而 tenant.Tenant.SchemaName 是 *string —— 恰好对应
	//   「shared 无 schema，dedicated 必有 schema」这条不变量。
	const q = `
		SELECT id::text, code, name, tier, status, schema_name
		  FROM dim_tenant
		 WHERE id = $1`
	var (
		t          Tenant
		schemaName *string
	)
	err := r.pool.QueryRow(ctx, q, tenantID).
		Scan(&t.ID, &t.Code, &t.Name, &t.Tier, &t.Status, &schemaName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // 不存在：是"无"，不是"错"
		}
		return nil, fmt.Errorf("tenant: 查租户 %s 失败: %w", tenantID, err)
	}
	t.SchemaName = schemaName
	return &t, nil
}

// List 列出全部租户（供管理台）。按创建时间倒序。
//
// ★ 这是**平台级**操作（跨租户）：dim_tenant 是平台表，本身不受 RLS 过滤。
//	调用方必须是平台管理员通道 —— 本类型不做权限判断（它没有身份上下文），
//	故调用点必须自己保证「只有管理台能调到」。
func (r *Registry) List(ctx context.Context) ([]Tenant, error) {
	if r == nil || r.pool == nil {
		return nil, ErrTenantRegistryUnavailable
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, code, name, tier, status, schema_name
		  FROM dim_tenant
		 ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("tenant: 列租户失败: %w", err)
	}
	defer rows.Close()
	out := []Tenant{}
	for rows.Next() {
		var (
			t          Tenant
			schemaName *string
		)
		if err := rows.Scan(&t.ID, &t.Code, &t.Name, &t.Tier, &t.Status, &schemaName); err != nil {
			return nil, err
		}
		t.SchemaName = schemaName
		out = append(out, t)
	}
	return out, rows.Err()
}

// ResolverFor 用本注册表构造一个解析器。
//
// ★ 默认 DisallowHost=true 与 LastSource=nil 的取舍见 Resolver 的注释。
func (r *Registry) ResolverFor() *Resolver {
	return &Resolver{Lookup: r.Lookup, DisallowHost: true}
}

// ───────────────────────────── 从解析结果到租户上下文 ─────────────────────────────

// ContextFor 走完整条解析链：提示 → 校验 → 查注册表 → TenantContext。
//
// ★ 这是**唯一**推荐的入口。上层（认证中间件 / api 层）不应自己拼 Hint
//   再调 Resolve —— 那样每处都会重新决定「什么算缺失」「允不允许 host」，
//   迟早漂移成两套策略。
//
// 返回 (Resolution, error)：error 仅在**基础设施故障**时非 nil
// （注册表不可用 / 打库失败），此时上层应回 503。
// 租户层面的拒绝（缺失/非法/不存在/未激活）走 Resolution.Rejected()，
// 上层映射为 400/404/403 —— 二者不可混淆。
func (r *Registry) ContextFor(ctx context.Context, h Hint) (Resolution, error) {
	if r == nil || r.pool == nil {
		return Rejected(ReasonInternal), ErrTenantRegistryUnavailable
	}
	res := r.ResolverFor().Resolve(ctx, h)
	if res.Allowed() {
		return res, nil
	}
	// ★ 把"基础设施故障"从业务拒绝里摘出来：
	//   Resolver 把 Lookup 出错折叠成了 ReasonInternal，但上层需要区分
	//   「注册表抖了一下（重试即可）」与「这个租户确实不该被服务」。
	//   做法：internal 时再探一次池健康度，不健康 ⇒ 返回 error。
	if res.Reason() == ReasonInternal {
		if err := r.pool.Ping(ctx); err != nil {
			return res, fmt.Errorf("tenant: 注册表不可达: %w", err)
		}
	}
	return res, nil
}
