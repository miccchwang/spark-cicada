package gate_test

// f8_cosign_sendside_test.go —— F8 会签「发送侧」闸门。
//
// 背景（为什么需要这个文件）：
//
//	仓库里早就有 TestG7_CosignTrigger，它测的是「给定 RouteInput，会签触发判定对不对」。
//	但那条断言测的是**入参**，不是**链路**：它自己 new 出 RouteInput{ExpiryDays: 91}，
//	而生产路径（req.Validate）从来没有填过 ExpiryDays / BatchSize —— 两个字段
//	在那条链路上恒为 0，于是
//	    「有效期 > 90 天 ⇒ 会签」与「批量授予 ≥ 10 ⇒ 会签」
//	这两条触发条件在**真实提交时永远不成立**，而闸门一直全绿。
//
//	这是本仓反复出现的同一个病：断言测的是"函数的入参"，而没人保证链路上真的
//	会传这些入参。判据（复用）：grep 该断言依赖的核心字段/函数，看**生产路径**
//	里有没有真实赋值点；只有测试在构造、生产从不填的值 = 恒真 = 没有断言。
//
// 本文件补的是「链路上真的会发生」的断言：从**提交**入口进，到**状态**出口出。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/authz"
	"github.com/miccchwang/spark-cicada/backend/internal/chain"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/req"
)

// f8Fixture 造一条最小但完整的组织链：top(T1) ← vp(T2) ← me(T3)。
//   - me 的上级 = vp ⇒ +1 = vp，vp 的上级 = top ⇒ +2 = top
//   - 权限沿链向上放行（approver 必须自身覆盖被申请内容），故 top/vp 取 T1 水平授权
//   - 复用 gate_test.go 的 rbFixtures，避免"夹具写两遍、两处各错一半"
func f8Fixture(t *testing.T) (*req.Service, *chain.OrgDirectory) {
	t.Helper()
	org, res, ents := rbFixtures()
	svc := req.NewService(org, res, ents)
	svc.CcPolicy = chain.DefaultCcPolicy()
	svc.Now = func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }
	return svc, org
}

func f8Submit(t *testing.T, svc *req.Service, d req.Draft, expiry *time.Time) *req.Request {
	t.Helper()
	r := &req.Request{
		ID: "F8-T", Applicant: "me", Purpose: "闸门用例",
		Draft: d, RequestedExpiry: expiry,
		CreatedAt: svc.Now(),
	}
	v, err := svc.Submit(r, nil)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if !v.OK {
		t.Fatalf("预校验未通过: %s", v.Message)
	}
	return r
}

// ───────────────── 断言 1：有效期 > 90 天 ⇒ 真实提交后进入 COSIGN_PENDING ─────────────────
//
// 这是**链路级**断言：RequestedExpiry 是申请单上的字段，由提交入口传给 Validate，
// 绝不由测试手工塞进 RouteInput。修好之前，这条必然红（ExpiryDays 恒 0）。
func TestG7_CosignByExpiry_ThroughSubmitPath(t *testing.T) {
	svc, _ := f8Fixture(t)
	now := svc.Now()

	// 91 天 ⇒ 超过 90 天阈值
	long := now.Add(91 * 24 * time.Hour)
	r := f8Submit(t, svc, req.Draft{MaxLevel: authz.L2}, &long)

	if r.Status != req.StatusCosignPending {
		t.Fatalf("有效期 91 天应触发会签（COSIGN_PENDING），实际状态=%s；"+
			"说明链路上 ExpiryDays 未被填充（闸门在测入参而非链路）", r.Status)
	}
	if v := gate.CheckCosignTrigger(f8ChainOf(r), true); len(v) != 0 {
		t.Fatalf("G7 会签触发断言失败: %v", v)
	}

	// 边界：正好 90 天 ⇒ 不触发（阈值是「>」，等于不算超）
	exact := now.Add(90 * 24 * time.Hour)
	r2 := f8Submit(t, svc, req.Draft{MaxLevel: authz.L2}, &exact)
	if r2.Status != req.StatusApproving {
		t.Fatalf("有效期正好 90 天不应触发会签，实际状态=%s", r2.Status)
	}

	// 边界：90 天零 1 小时 ⇒ 取整必须向上，仍应触发
	over := now.Add(90*24*time.Hour + time.Hour)
	r3 := f8Submit(t, svc, req.Draft{MaxLevel: authz.L2}, &over)
	if r3.Status != req.StatusCosignPending {
		t.Fatalf("90 天零 1 小时应触发会签（天数须向上取整），实际状态=%s", r3.Status)
	}

	// 边界：无有效期 ⇒ 不触发（不是"默认长期"）
	r4 := f8Submit(t, svc, req.Draft{MaxLevel: authz.L2}, nil)
	if r4.Status != req.StatusApproving {
		t.Fatalf("未填有效期不应触发会签，实际状态=%s", r4.Status)
	}
}

// f8ChainOf 把申请单还原成 ApprovalChain 视图，供 gate 断言复用。
//
// 只还原 gate.CheckCosignTrigger 需要的字段（CcRecords / CcRule）；
// 刻意不还原 Applicant/Approver —— 断言只针对会签触发，多余字段会让
// "断言失败时以为是别的原因"。
func f8ChainOf(r *req.Request) *chain.ApprovalChain {
	rule := chain.CcRuleNone
	for _, cc := range r.CCs {
		if cc.Reason == chain.CosignReasonL4 ||
			cc.Reason == chain.CosignReasonCrossDept ||
			cc.Reason == chain.CosignReasonLongTerm ||
			cc.Reason == chain.CosignReasonSensitive ||
			cc.Reason == chain.CosignReasonBatch {
			rule = chain.CcRulePlusTwo
			break
		}
	}
	return &chain.ApprovalChain{
		Applicant: r.Applicant,
		CcRecords: r.CCs,
		CcRule:    rule,
	}
}

// ───────────────── 断言 2：批量授予 ≥ 10 ⇒ 真实提交后进入 COSIGN_PENDING ─────────────────

func TestG7_CosignByBatch_ThroughSubmitPath(t *testing.T) {
	svc, _ := f8Fixture(t)

	// 5 个品牌 × includeDescendants ⇒ 规模 10 ⇒ 触发
	big := req.Draft{MaxLevel: authz.L2, Dimensions: []req.DimGrant{
		{Dim: "brand", Values: []string{"b1", "b2", "b3", "b4", "b5"}, IncludeDescendants: true},
	}}
	r := f8Submit(t, svc, big, nil)
	if r.Status != req.StatusCosignPending {
		t.Fatalf("批量规模 10 应触发会签，实际状态=%s；"+
			"说明链路上 BatchSize 未被填充（闸门在测入参而非链路）", r.Status)
	}

	// 4 个品牌 ×2 = 8 < 10 ⇒ 不触发（防"阈值被写成 >=1 也过"）
	small := req.Draft{MaxLevel: authz.L2, Dimensions: []req.DimGrant{
		{Dim: "brand", Values: []string{"b1", "b2", "b3", "b4"}, IncludeDescendants: true},
	}}
	r2 := f8Submit(t, svc, small, nil)
	if r2.Status != req.StatusApproving {
		t.Fatalf("批量规模 8 不应触发会签，实际状态=%s", r2.Status)
	}

	// 10 个品牌、不含后代 ⇒ 规模 10 ⇒ 仍触发（不带 includeDescendants 也要算）
	flat := req.Draft{MaxLevel: authz.L2, Dimensions: []req.DimGrant{
		{Dim: "brand", Values: []string{"b1", "b2", "b3", "b4", "b5", "b6", "b7", "b8", "b9", "b10"}},
	}}
	r3 := f8Submit(t, svc, flat, nil)
	if r3.Status != req.StatusCosignPending {
		t.Fatalf("10 个明确取值应触发会签，实际状态=%s", r3.Status)
	}
}

// ───────────────── 断言 3：会签名单与会签模式必须落在**同一次**路由结果上 ─────────────────

func TestG7_CosignRecordsMatchStatus(t *testing.T) {
	svc, _ := f8Fixture(t)
	now := svc.Now()

	// L4 ⇒ 会签；此时名单里必须**真的**有 cosign 记录，
	// 不能出现"状态说会签、名单全是 notify"（会签被静默跳过）。
	r := f8Submit(t, svc, req.Draft{MaxLevel: authz.L4}, nil)
	if r.Status != req.StatusCosignPending {
		t.Fatalf("L4 应进入 COSIGN_PENDING，实际=%s", r.Status)
	}
	hasCosign := false
	for _, cc := range r.CCs {
		if cc.Mode == chain.CcCosign {
			hasCosign = true
		}
	}
	if !hasCosign {
		t.Fatalf("状态=COSIGN_PENDING 但抄送名单里没有一条 cosign 记录 %+v —— "+
			"会签人被静默跳过（状态与名单不同源）", r.CCs)
	}

	// 反向：低风险单绝不能残留任何 cosign 记录，否则会永远卡在待会签。
	low := f8Submit(t, svc, req.Draft{MaxLevel: authz.L2}, ptrTime(now.Add(7*24*time.Hour)))
	if low.Status != req.StatusApproving {
		t.Fatalf("低风险单不应会签，实际=%s", low.Status)
	}
	for _, cc := range low.CCs {
		if cc.Mode == chain.CcCosign {
			t.Fatalf("低风险单残留 cosign 记录 %+v —— 该单会永远无法通过审批", cc)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// ───────────────── 断言 4：+1/+2 抄送名单不得出现本人、重复项 ─────────────────

func TestG7_CcListNoSelfNoDuplicates(t *testing.T) {
	svc, org := f8Fixture(t)
	r := f8Submit(t, svc, req.Draft{MaxLevel: authz.L2}, nil)

	// 名单内容的自洽断言（与 CcRule 无关，任何规则下都必须成立）：
	//   - 申请人本人不得出现在抄送名单
	//   - 审批人不得同时被抄送（"+2 抄送"退化成"抄送给自己"）
	//   - 同一人不得出现两次（待办计数虚增、会签时"两个人"其实是同一人）
	seen := map[string]int{}
	for _, cc := range r.CCs {
		seen[cc.Cc]++
		if cc.Cc == r.Applicant {
			t.Fatalf("申请人本人出现在抄送名单：%s", cc.Cc)
		}
		for _, a := range r.Approvals {
			if cc.Cc == a.Approver {
				t.Fatalf("审批人 %s 同时被抄送 —— '+2 抄送'退化成'抄送给自己'", cc.Cc)
			}
		}
	}
	for who, n := range seen {
		if n > 1 {
			t.Fatalf("抄送名单出现重复项 %s ×%d —— 待办计数虚增", who, n)
		}
	}

	// 再把真实记录交给 gate 断言交叉校验。
	//
	// ★ 两个必须踩准的点（本轮实测踩过，属"断言看似失败、其实夹具给错字段"）：
	//   1. CheckPlusOnePlusTwo 读的是 ac.**CcList**（+1/+2 层），不是 ac.CcRecords
	//      —— 传错字段会让它认为"+2 抄送缺失"，而实现其实完全正确。
	//   2. 虚线汇报人**不在 CcList 里**（进的是 DottedLineCc），故只需剥掉
	//      CcRecords 中的虚线项后与 CcList 对齐即可，不要做多余的过滤。
	if v := gate.CheckPlusOnePlusTwo(org, &chain.ApprovalChain{
		Approver:  r.Approvals[0].Approver,
		CcList:    ccAccounts(r.CCs, false),
		CcRecords: r.CCs,
		CcRule:    ccRuleOf(r),
	}, r.Applicant); len(v) != 0 {
		t.Fatalf("G7 +1审批/+2抄送断言失败: %v", v)
	}
}

// ccAccounts 抽出抄送账号；includeDotted=true 时含虚线汇报上级。
func ccAccounts(recs []chain.CcRecord, includeDotted bool) []string {
	out := []string{}
	for _, cc := range recs {
		if !includeDotted && cc.Reason == "虚线汇报" {
			continue
		}
		out = append(out, cc.Cc)
	}
	return out
}

// TestG7_CcRecordJSONKeys —— 抄送记录会序列化进 fact_permission_request.ccs
// 这个 jsonb 列，0006 的 v_cosign_pending 视图按 `cc->>'decidedAt'` 取键判定
// 「会签人是否已表态」。
//
// ★ 键名一旦漂移（如改成 PascalCase），视图永远拿到 NULL ⇒ 把
//   「已表态」全判成「未表态」，待办列表虚增；反向写错则漏掉真正卡住的单。
//   这条断言不依赖数据库，是纯序列化层的钉子。
func TestG7_CcRecordJSONKeys(t *testing.T) {
	rec := chain.CcRecord{
		Cc: "u1", Reason: chain.CosignReasonL4, Mode: chain.CcCosign,
		NotifiedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"cc", "reason", "mode", "notifiedAt", "vetoed", "decidedAt"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("ccs jsonb 缺键 %q（0006 的 v_cosign_pending 依赖该键名），实际=%s", k, b)
		}
	}
	// nil 的 decidedAt 必须显式序列化为 null，不能因 omitempty 消失 ——
	// 省略该键会让读取方无法区分「未表态」与「旧数据没这个字段」。
	if m["decidedAt"] != nil {
		t.Fatalf("未表态时 decidedAt 应为 null，实际=%v", m["decidedAt"])
	}
	if s := string(b); !strings.Contains(s, `"decidedAt":null`) {
		t.Fatalf("未表态时必须是显式 null（不得 omitempty），实际=%s", s)
	}
}

// ccRuleOf 从申请单还原抄送层级规则。
//
// ★ 为什么要还原而不是直接读实现内部字段：这条断言的意义是**交叉验证** ——
//   申请单上落盘的抄送记录（会进 jsonb、会被前端与审计读）必须与
//   "+2 应为审批人的上级"这个 F9 口径自洽。若只读实现的计算结果，
//   就等于"用实现验证实现"。
func ccRuleOf(r *req.Request) string {
	if len(r.CCs) == 0 {
		return chain.CcRuleNone
	}
	for _, cc := range r.CCs {
		switch cc.Reason {
		case "+2":
			return chain.CcRulePlusTwo
		case "+1 的直属上级":
			return chain.CcRulePlusOneSupervisor
		}
	}
	return chain.CcRuleNone
}

// ───────────────── 断言 5：empty_cc_behavior = skip ⇒ 无可抄送人时不留记录 ─────────────────

// orgNoCc 造一条没有 +2、且 +1 就是申请人上级的链：
// top(T1) ← me(T3)，me 的上级 = top，top 无上级 ⇒ +2 不存在。
// 授权用 T1 模板（tpl.lead），保证 approver 自身覆盖被申请内容。
func orgNoCc(t *testing.T) (*req.Service, *chain.OrgDirectory) {
	t.Helper()
	org := chain.NewOrgDirectory()
	org.Put(&chain.OrgNode{Account: "top", Tier: "T1", PrimaryDept: "HQ",
		CanApprove: true, Active: true})
	org.Put(&chain.OrgNode{Account: "me", Supervisor: "top", Tier: "T3",
		PrimaryDept: "D1", CanApprove: true, Active: true})

	templates := map[string]*authz.Entitlement{
		"tpl.lead": {
			Account:    "tpl.lead",
			Modules:    authz.ModuleGrant{Enabled: []string{"m.ops", "m.roi", "m.inventory"}},
			MaxLevel:   authz.L4,
			Dimensions: []authz.DimensionGrant{{Dim: "brand", Values: nil}},
		},
	}
	res := authz.NewResolver(templates)
	ents := func(account string) (*authz.Entitlement, []*authz.Entitlement) {
		return &authz.Entitlement{Account: account, BaseTemplate: "tpl.lead"}, nil
	}
	svc := req.NewService(org, res, ents)
	svc.Now = func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }
	return svc, org
}

func TestG7_EmptyCcBehavior(t *testing.T) {
	// skip：+2 不存在且降级目标 == approver ⇒ 不产生抄送记录
	svc, _ := orgNoCc(t)
	svc.CcPolicy = chain.DefaultCcPolicy()
	svc.CcPolicy.EmptyCcBehavior = chain.EmptyCcSkip
	r := f8Submit(t, svc, req.Draft{MaxLevel: authz.L4}, nil)
	if len(r.CCs) != 0 {
		t.Fatalf("empty_cc_behavior=skip 时不应产生抄送记录，实际 %+v", r.CCs)
	}
	// ★ 但会签**不因此消失**：会签人本就在抄送名单里 ——
	//   名单为空 ⇒ 状态判定循环一轮都不会进 ⇒ 状态若被判成 COSIGN_PENDING，
	//   该单永远无法通过（没有任何人可会签）。本实现的口径：名单为空时不置
	//   COSIGN_PENDING，而是按"无会签人可表态"处理为 APPROVING。
	if r.Status != req.StatusApproving {
		t.Fatalf("抄送名单为空时状态不应停在 COSIGN_PENDING（无人可表态 ⇒ 永远卡死），实际=%s", r.Status)
	}

	// notify（默认）：+2 不存在时**不得**降级成 +1 的上级（= approver 本人）
	svc2, _ := orgNoCc(t)
	svc2.CcPolicy = chain.DefaultCcPolicy() // EmptyCcNotify
	r2 := f8Submit(t, svc2, req.Draft{MaxLevel: authz.L2}, nil)
	for _, cc := range r2.CCs {
		if cc.Cc == r2.Approvals[0].Approver {
			t.Fatalf("+2 缺失时降级抄送给了审批人本人 %s —— 抄送形同虚设", cc.Cc)
		}
	}
}

// ───────────────── 断言 6：BatchSize 未被填 ⇒ 会签漏判（防回归的自证） ─────────────────

// 该用例直接对 CosignTrigger 断言"入参语义"，与上面的链路断言互补：
// 链路断言保证生产会传值，这里保证判定本身正确。
func TestG7_CosignTrigger_UnitSemantics(t *testing.T) {
	ccp := chain.DefaultCcPolicy()

	cases := []struct {
		name   string
		in     chain.RouteInput
		expect bool
	}{
		{"L4", chain.RouteInput{DraftLevel: "L4"}, true},
		{"跨部门", chain.RouteInput{CrossDept: true}, true},
		{"有效期91", chain.RouteInput{ExpiryDays: 91}, true},
		{"有效期90(阈值以下)", chain.RouteInput{ExpiryDays: 90}, false},
		{"含grp.roi", chain.RouteInput{DraftGroups: []string{"grp.roi"}}, true},
		{"批量10", chain.RouteInput{BatchSize: 10}, true},
		{"批量9(阈值以下)", chain.RouteInput{BatchSize: 9}, false},
		{"全都不命中", chain.RouteInput{DraftLevel: "L2", ExpiryDays: 7}, false},
	}
	for _, c := range cases {
		got, reason := chain.CosignTrigger(c.in, ccp)
		if got != c.expect {
			t.Fatalf("%s: CosignTrigger=%v want=%v (reason=%s)", c.name, got, c.expect, reason)
		}
		if got && reason == "" {
			t.Fatalf("%s: 触发会签但未给出原因 —— 审计无法复盘'为什么这单要会签'", c.name)
		}
	}
}

// ───────────────── 断言 7：SetCosignMode 只增不减、不增删名单 ─────────────────

func TestG7_SetCosignMode(t *testing.T) {
	recs := []chain.CcRecord{
		{Cc: "a", Mode: chain.CcNotify, Reason: "+2"},
		{Cc: "b", Mode: chain.CcNotify, Reason: "+2"},
	}
	before := len(recs)

	// 升级：只改目标那一条
	if !chain.SetCosignMode(recs, "a", true, chain.CosignReasonBatch) {
		t.Fatal("SetCosignMode 未命中已存在的抄送人")
	}
	if recs[0].Mode != chain.CcCosign || recs[0].Reason != chain.CosignReasonBatch {
		t.Fatalf("升级失败: %+v", recs[0])
	}
	if recs[1].Mode != chain.CcNotify {
		t.Fatalf("误伤其他抄送人: %+v", recs[1])
	}
	if len(recs) != before {
		t.Fatalf("SetCosignMode 改动了名单长度：%d → %d", before, len(recs))
	}

	// 不降级：已是 cosign 时传 false 不得降回 notify（会签是唯一阻断机制）
	chain.SetCosignMode(recs, "a", false, "")
	if recs[0].Mode != chain.CcCosign {
		t.Fatal("SetCosignMode 把已会签的记录降级为知会 —— 高风险授予被静默放行")
	}

	// 未命中：返回 false 且不动任何记录
	if chain.SetCosignMode(recs, "nobody", true, "x") {
		t.Fatal("SetCosignMode 对不在名单的账号返回了 true")
	}
	if recs[0].Mode != chain.CcCosign || recs[1].Mode != chain.CcNotify {
		t.Fatalf("未命中时改动了记录: %+v", recs)
	}
}
