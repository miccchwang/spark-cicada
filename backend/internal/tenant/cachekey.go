package tenant

import (
	"strings"
)

// ══════════════════════════════════════════════════════════════════════════
// 缓存键的租户隔离（Task #57）
//
// ★★ 为什么缓存键必须含租户，而不是「记得在调用处拼一下」：
//
//	查询缓存的键若只是 QueryState 的哈希，则：
//	  租户 A 与租户 B 发**同样的查询** ⇒ 命中**同一条**缓存条目
//	  ⇒ B 拿到 A 的数字。
//
//	这类缺陷的三个特征让它极易漏过评审：
//	  ① **不报错** —— 缓存命中是正常路径，没有任何异常可捕获；
//	  ② **不可复现** —— 只在「两家租户恰好发同样查询」时出现，
//	     且谁先写入谁的数据被后到者读到（顺序相关）；
//	  ③ **测试常绿** —— 单租户测试永远命中自己的条目。
//
//	故把「键必须含 tenantId」提升为**契约**（见 contracts/tenant.ts 的
//	tenantCacheKey），并在 Go 侧提供**唯一**构造函数，禁止各处自行拼键。
//
// ★ 与 TS 契约保持同构：本文件的 CacheKeySeparator 与
//   contracts/tenant.ts 的 CACHE_KEY_SEPARATOR 必须一致（同为 U+001F）。
//   前端与后端可能共享同一 Redis 命名空间 —— 键格式不一致会让
//   「前端缓存」与「后端缓存」互相看不见（不是安全问题，
//   但会导致缓存命中率骤降与排查困难）。
// ══════════════════════════════════════════════════════════════════════════

// CacheKeySeparator 与 contracts/tenant.ts 的 CACHE_KEY_SEPARATOR 一致。
//
// ★ 选不可见字符 U+001F（Unit Separator）而不是 `:` / `|`：
//
//	可见分隔符会出现在正常内容里（如查询哈希、字段名可能含 `-`、`_`），
//	于是 `("a:b", "c")` 与 `("a", "b:c")` 拼出同一个键 —— 键碰撞
//	会让两个不同的查询共享缓存，进而串数据。
//	U+001F 在 UUID、哈希、字段名中都不会出现，消除这类歧义。
const CacheKeySeparator = "\u001f"

// TenantCacheKey 构造租户隔离的缓存键。
//
// ★ 强制把 tenantID 放在**最前**：即便未来改成「按查询语句前缀分片」，
//	租户维度仍是最外层分区键，不会退化。
//
// ★ fail-closed：无租户 ⇒ 返回空串。
//
//	返回空串而不是「编一个默认租户」：调用方拿到空串后
//	要么以「永远 miss」的方式安全退化（推荐），要么显式报错。
//	若这里编一个 UUID，共享档下所有未标租户的缓存会挤进同一条 ——
//	那正是我们要消灭的形态。
func TenantCacheKey(tenantID string, parts ...string) string {
	if strings.TrimSpace(tenantID) == "" {
		return ""
	}
	if len(parts) == 0 {
		return tenantID
	}
	return tenantID + CacheKeySeparator + strings.Join(parts, CacheKeySeparator)
}

// TenantCacheKeyFor 是 TenantCacheKey 的上下文版本：直接从 TenantContext 取租户。
//
// ★ 存在的理由：调用方手里通常是 TenantContext 而不是裸 uuid。
//	提供这个入口可以让「用 TenantID 还是 RLSTenantID」这个选择
//	只在一处发生 —— 两处取不同值会造成「设置用 A、缓存键用 B」的错位。
func TenantCacheKeyFor(tc TenantContext, parts ...string) string {
	return TenantCacheKey(tc.TenantID, parts...)
}

// CacheKeyForQuery 构造「一次查询结果」的缓存键（租户前缀 + 查询哈希）。
//
// ★ 独立成函数而不是让 query 包直接调 TenantCacheKey(tenantID, hash)：
//
//	缓存键的**结构**（几段、谁是第一段、分隔符是什么）是需要
//	跨包一致的约定。若 query 包自己拼、api 包又自己拼，
//	迟早出现「写入用 2 段、读取用 3 段」而对不上 —— 表现为缓存永不命中，
//	或更糟：命中到别的形态的键。把结构收敛到本函数，只有一处定义。
//
// ★ tenantID 为空 ⇒ 返回空串（fail-closed）。调用方应据此**跳过缓存**，
//	而不是用一个「空租户」的键去读写。
func CacheKeyForQuery(tenantID, queryHash string) string {
	if strings.TrimSpace(tenantID) == "" || queryHash == "" {
		return ""
	}
	return TenantCacheKey(tenantID, queryHash)
}
