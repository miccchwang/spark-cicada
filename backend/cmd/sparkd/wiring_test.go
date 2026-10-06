// wiring_test.go —— 装配层的关键判定（管理员/主管/IT 基座前缀边界）
//
// ★ 这些断言存在的理由：它们是「一处判定、多处使用」的**唯一真相点**。
//
//	组管理与模板管理共用 isAdmin，若这里判错，两个模块一起错；
//	而 isITBase 的前缀边界是**已经踩过一次**的 bug 类别
//	（`tpl.item` 被误判为 `tpl.it`），必须钉死。
package main

import "testing"

// testTenant 是本文件用的合法租户 uuid（判定函数要求租户格式合法）。
const testTenant = "00000000-0000-4000-8000-0000000000a1"

func TestIsITBase_PrefixBoundary(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"tpl.it", true},
		{"tpl.it.v2", true},
		{"tpl.it.admin", true},
		// ★ 以下三个是历史 bug 类别：朴素的 HasPrefix(tpl,"tpl.it") 会误判
		{"tpl.item", false},
		{"tpl.item.v2", false},
		{"tpl.itsupport", false},
		// 其他
		{"tpl.lead", false},
		{"", false},
		{"tpl.itx", false},
	}
	for _, c := range cases {
		if got := isITBase(c.in); got != c.want {
			t.Errorf("isITBase(%q)=%v，期望 %v", c.in, got, c.want)
		}
	}
}

// 降级模式下（无库）管理员判定：内置夹具里 tpl.it 基座即管理员。
func TestIsAdminFunc_DegradedFixture(t *testing.T) {
	p := &dataPlane{dbReady: false}
	isAdmin := p.isAdminFunc()

	// 内置夹具：it.ops → tpl.it ⇒ 管理员
	if !isAdmin("it.ops") {
		t.Fatal("★ 降级夹具下 it.ops（tpl.it 基座）应被判为管理员")
	}
	// 业务负责人不是管理员
	if isAdmin("lead.sea") {
		t.Fatal("普通业务负责人不应被判为管理员")
	}
	// 未知账号
	if isAdmin("nobody") {
		t.Fatal("未知账号不应被判为管理员")
	}
	// 空账号
	if isAdmin("") {
		t.Fatal("空账号不应被判为管理员")
	}
}

// ★ 主管判定在无库时必须**保守返回 false**（不能退化成「人人都是主管」）。
//
// ★ 0012 起签名带 tenantID：判定须限定在租户内（dim_org 账号不再全局唯一）。
func TestIsSupervisorFunc_NoDBIsConservative(t *testing.T) {
	p := &dataPlane{dbReady: false}
	isSup := p.isSupervisorFunc()
	for _, acct := range []string{"ceo", "vp.sea", "lead.sea", "it.ops", "", "anyone"} {
		if isSup(testTenant, acct) {
			t.Fatalf("★ 无库时 isSupervisor(%q) 必须为 false（否则人人可建团队档模板 = 提权）", acct)
		}
	}
}

// ★ 缺租户 / 非法租户 ⇒ 保守拒绝。
//
// ★★ 为什么单独测这条：本函数走**特权通道**查库（绕过 RLS），
// 正确性完全依赖显式 tenant_id 过滤。若租户为空还去查，
// 就退化成「跨租户按账号匹配」—— 在多租户下会张冠李戴。
func TestIsSupervisorFunc_MissingOrBadTenantIsConservative(t *testing.T) {
	p := &dataPlane{dbReady: false}
	isSup := p.isSupervisorFunc()
	for _, tid := range []string{"", "   ", "not-a-uuid", "12345"} {
		if isSup(tid, "lead.sea") {
			t.Fatalf("★ 租户 %q 非法/缺失时 isSupervisor 必须为 false（fail-closed）", tid)
		}
	}
}

// 无库时 baseTemplateOf 回退内置夹具。
func TestBaseTemplateOf_DegradedFixture(t *testing.T) {
	p := &dataPlane{dbReady: false}
	if got := p.baseTemplateOf("it.ops"); got != "tpl.it" {
		t.Fatalf("it.ops 基座应为 tpl.it，得到 %q", got)
	}
	if got := p.baseTemplateOf("lead.sea"); got != "tpl.lead" {
		t.Fatalf("lead.sea 基座应为 tpl.lead，得到 %q", got)
	}
	if got := p.baseTemplateOf("nobody"); got != "" {
		t.Fatalf("未知账号基座应为空，得到 %q", got)
	}
	if got := p.baseTemplateOf(""); got != "" {
		t.Fatalf("空账号基座应为空，得到 %q", got)
	}
}
