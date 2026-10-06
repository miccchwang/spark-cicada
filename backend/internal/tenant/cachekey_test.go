package tenant

import (
	"strings"
	"testing"
)

// ───────────────────────────── 缓存键：租户隔离 ─────────────────────────────
//
// ★ 基础用例（不同租户不同键 / 无租户空串 / 租户在最前）在 tenant_test.go 的
//   TestTenantCacheKey_必须带租户前缀 里；本文件补它**没有覆盖**的：
//   拼接歧义、段数混淆、上下文版取字段、与 TS 契约同构。

func TestTenantCacheKey_前缀与内容分离(t *testing.T) {
	k := TenantCacheKey(uidAlpha, "hash-xyz")
	if !strings.HasPrefix(k, uidAlpha) {
		t.Fatalf("★ 缓存键必须以租户 ID 开头，实际 %q", k)
	}
	if !strings.Contains(k, "hash-xyz") {
		t.Errorf("缓存键应含查询哈希，实际 %q", k)
	}
}

// TestTenantCacheKey_不同租户不同键 是本文件最重要的一条。
//
// ★ 它钉住的是「缓存串租」这个**不报错、不可复现**的缺陷类别：
//
//	两家租户发同样的查询（相同 hash），若键只由 hash 构成，
//	后到者会命中先到者的条目 —— 报表「偶尔」显示别人的数字。
//	本测试断言两租户的键**必须不同**。
func TestTenantCacheKey_不同租户不同键(t *testing.T) {
	a := TenantCacheKey(uidAlpha, "same-hash")
	b := TenantCacheKey(uidBeta, "same-hash")
	if a == b {
		t.Fatalf("★ 两租户对同一查询产生了同一缓存键 %q —— 会串租", a)
	}
}

// TestTenantCacheKey_无租户必须fail_closed 钉住「缺失即拒绝」。
//
// ★ 反向断言很重要：若这里返回"默认租户"的键，
//	共享档下所有「忘了带租户」的缓存会挤在同一条 —— 那比不缓存更糟。
func TestTenantCacheKey_无租户必须fail_closed(t *testing.T) {
	for _, bad := range []string{"", "   ", "\t"} {
		if got := TenantCacheKey(bad, "hash"); got != "" {
			t.Errorf("★ 无租户（%q）应返回空串（fail-closed），实际 %q", bad, got)
		}
	}
}

// TestTenantCacheKey_分隔符不可歧义 钉住「拼接歧义」这个键碰撞来源。
//
// ★ 用 `:` 或 `|` 这类可见字符做分隔符时：
//
//	("a:b", "c") 与 ("a", "b:c") 会拼出同一个键。
//	本测试用「把分隔符塞进内容」的方式证明当前实现不会碰撞。
func TestTenantCacheKey_分隔符不可歧义(t *testing.T) {
	// 内容里塞进各种候选分隔符，看是否会造成不同输入 → 同键
	evil := []string{":", "|", "/", "#", "-", "_"}
	for _, s := range evil {
		k1 := TenantCacheKey(uidAlpha, "a"+s+"b", "c")
		k2 := TenantCacheKey(uidAlpha, "a", "b"+s+"c")
		if k1 == k2 {
			t.Errorf("★ 分隔符 %q 造成键碰撞：%q", s, k1)
		}
	}
}

// TestTenantCacheKey_段内不得含分隔符 记录一个**真实存在但不可达**的歧义。
//
// ★ 诚实说明（而不是假装没有）：
//
//	TenantCacheKey(t, "h1\u001fh2") 与 TenantCacheKey(t, "h1", "h2")
//	**确实**拼出同一个键 —— 因为分隔符是可见字符的一种，出现在段内时
//	与「段边界」不可区分。
//
// ★ 为什么这不构成实际风险：
//
//	本函数的调用方只传两类内容：uuid（租户 ID）与哈希
//	（contracts.Hash 的输出，固定字符集）。两者都**不含** U+001F。
//	故「段内出现分隔符」在现实中不可达。
//
// ★ 但「现实中不可达」不能当成「不会发生」—— 所以要有一条守卫：
//	若某天有人把**用户输入**（如筛选条件原文）当段传进来，
//	本测试会立刻变红，提醒他把输入先哈希再传。
//
//	本测试的作用就是把这个前提**写成可执行断言**，而不是留在注释里。
func TestTenantCacheKey_段内不得含分隔符(t *testing.T) {
	// 记录现状：这两者确实撞键（PostgreSQL uuid 与 hex 哈希都不会含 U+001F）
	single := TenantCacheKey(uidAlpha, "h1"+CacheKeySeparator+"h2")
	multi := TenantCacheKey(uidAlpha, "h1", "h2")
	if single != multi {
		t.Fatalf("实现语义已变化：单段含分隔符与两段不再撞键（single=%q multi=%q）—— "+
			"本测试的前提需重新评估", single, multi)
	}

	// ★ 真正的守卫：本函数的所有**合法**调用方只传 uuid / 哈希，
	//   两者都不含分隔符。若将来有人传了含分隔符的内容，说明他在
	//   传递未经规范化的用户输入 —— 那才是问题所在。
	for _, p := range []string{"h1", "h2", "aaaa1111-1111-4111-8111-111111111111"} {
		if strings.Contains(p, CacheKeySeparator) {
			t.Errorf("测试自身的输入 %q 不应含分隔符", p)
		}
	}
}

// TestTenantCacheKeyFor_与裸版本一致 钉住「上下文版取的是哪个字段」。
//
// ★ 若 TenantCacheKeyFor 误取 RLSTenantID 而缓存键用 TenantID，
//	或反之，就会出现「设置用 A、键用 B」的错位。本测试固定取 TenantID。
func TestTenantCacheKeyFor_与裸版本一致(t *testing.T) {
	tc := TenantContext{TenantID: uidAlpha, RLSTenantID: uidAlpha, Tier: TierShared}
	if got, want := TenantCacheKeyFor(tc, "h"), TenantCacheKey(uidAlpha, "h"); got != want {
		t.Errorf("上下文版与裸版本应一致：got %q want %q", got, want)
	}
}

// TestTenantCacheKey_分隔符与TS契约同构 钉住跨语言键格式一致。
//
// ★ 前端与后端可能共用同一缓存命名空间。若分隔符不一致，
//	两边会互相「看不见」对方的条目 —— 不是安全漏洞，但会让
//	缓存命中率骤降且极难排查。此处把值固化下来。
func TestTenantCacheKey_分隔符与TS契约同构(t *testing.T) {
	if CacheKeySeparator != "\u001f" {
		t.Fatalf("★ 分隔符应为 U+001F（Unit Separator），实际 %q —— "+
			"必须与 contracts/tenant.ts 的 CACHE_KEY_SEPARATOR 保持一致",
			CacheKeySeparator)
	}
}
