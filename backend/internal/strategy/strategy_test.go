// strategy_test.go —— M-STRATEGY 纯逻辑层单测。
//
// 重点覆盖三类容易出错的边界：
//  1. **候选集守卫**：用户不可能选到卡外的选项（越权/脏数据）；
//  2. **回滚语义**：目标必须是「变更前的值」，遇到不可逆变更要停；
//  3. **保持现状**：要留痕，但不得触发重算。
package strategy

import (
	"errors"
	"testing"
	"time"
)

// ───────────────────────────── 夹具 ─────────────────────────────

func opt(key string, r Risk, rev bool) Option {
	return Option{
		Key:            key,
		Label:          "选项 " + key,
		Description:    "说明 " + key,
		ExpectedEffect: "净利率 34.45% → 36.14%",
		Risk:           r,
		Reversible:     rev,
	}
}

// card 构造一张合法卡：3 个选项，current = "a"。
func card() *Choice {
	return &Choice{
		ID:         "ch.fee.caliber",
		Title:      "渠道费率口径",
		Context:    "Shopee 费率采集暂缓，当前用估算值，需决定是否切换口径",
		CurrentKey: "a",
		Options: []Option{
			opt("a", RiskLow, true),
			opt("b", RiskMedium, true),
			opt("c", RiskHigh, false),
		},
		Impact: ImpactPreview{
			Metrics: []string{"net_rate", "gp"},
			Delta: map[string]map[string]string{
				"a": {"net_rate": "不变"},
				"b": {"net_rate": "34.45% → 36.14%"},
				"c": {"net_rate": "34.45% → 38.90%"},
			},
			AffectedBuckets: []string{"pnl_month"},
		},
		Blocking: true,
	}
}

func entry(id, from, to string, at time.Time, rev bool, kind ActionKind) HistoryEntry {
	return HistoryEntry{
		ID: id, ChoiceID: "ch.fee.caliber",
		FromOption: from, ToOption: to,
		DecidedBy: "ceo", DecidedAt: at,
		SnapshotHash: "sha256:abc",
		Kind:         kind, Reversible: rev,
	}
}

// ───────────────────────────── Validate ─────────────────────────────

func TestValidate_合法卡无问题(t *testing.T) {
	if p := card().Validate(); len(p) != 0 {
		t.Fatalf("期望合法，实际问题：%v", p)
	}
}

func TestValidate_nil卡(t *testing.T) {
	var c *Choice
	if p := c.Validate(); len(p) == 0 {
		t.Fatal("nil 卡应报问题")
	}
}

func TestValidate_选项数下界(t *testing.T) {
	c := card()
	c.Options = c.Options[:1] // 只剩 1 个
	p := c.Validate()
	if len(p) == 0 {
		t.Fatal("单选项卡应被拒绝（伪决策）")
	}
}

func TestValidate_选项数上界(t *testing.T) {
	c := card()
	c.Options = append(c.Options, opt("d", RiskLow, true), opt("e", RiskLow, true)) // 5 个
	if len(c.Validate()) == 0 {
		t.Fatal("5 个选项应被拒绝（超过上界 4）")
	}
}

func TestValidate_currentKey必须在候选集内(t *testing.T) {
	c := card()
	c.CurrentKey = "zzz"
	p := c.Validate()
	if len(p) == 0 {
		t.Fatal("currentKey 不在候选集内应被拒绝")
	}
}

func TestValidate_缺context被拒绝(t *testing.T) {
	c := card()
	c.Context = "   "
	if len(c.Validate()) == 0 {
		t.Fatal("缺 context 应被拒绝（用户不知道为什么要决策）")
	}
}

func TestValidate_缺expectedEffect被拒绝(t *testing.T) {
	c := card()
	c.Options[1].ExpectedEffect = ""
	if len(c.Validate()) == 0 {
		t.Fatal("缺 expectedEffect 应被拒绝")
	}
}

func TestValidate_非法risk被拒绝(t *testing.T) {
	c := card()
	c.Options[1].Risk = "very-high"
	if len(c.Validate()) == 0 {
		t.Fatal("未知风险等级应被拒绝（不默认兜底）")
	}
}

func TestValidate_重复key被拒绝(t *testing.T) {
	c := card()
	c.Options[1].Key = "a"
	if len(c.Validate()) == 0 {
		t.Fatal("重复 key 应被拒绝")
	}
}

func TestValidate_delta含未知选项被拒绝(t *testing.T) {
	c := card()
	c.Impact.Delta["ghost"] = map[string]string{"net_rate": "?"}
	if len(c.Validate()) == 0 {
		t.Fatal("delta 里的未知选项应被拒绝（预览与卡片不同步）")
	}
}

// ───────────────────────────── Resolve ─────────────────────────────

func TestResolve_choose合法选项(t *testing.T) {
	got, err := card().Resolve(Action{Kind: ActionChoose, OptionKey: "b"})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if got != "b" {
		t.Fatalf("期望 b，得 %q", got)
	}
}

func TestResolve_choose卡外选项被硬拒绝(t *testing.T) {
	_, err := card().Resolve(Action{Kind: ActionChoose, OptionKey: "zzz"})
	if !errors.Is(err, ErrUnknownOption) {
		t.Fatalf("期望 ErrUnknownOption，得 %v", err)
	}
}

func TestResolve_choose缺optionKey被拒绝(t *testing.T) {
	_, err := card().Resolve(Action{Kind: ActionChoose})
	if !errors.Is(err, ErrUnknownOption) {
		t.Fatalf("期望 ErrUnknownOption，得 %v", err)
	}
}

func TestResolve_keepCurrent解析为当前key(t *testing.T) {
	got, err := card().Resolve(Action{Kind: ActionKeepCurrent})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if got != "a" {
		t.Fatalf("keep_current 应解析为当前 key a，得 %q", got)
	}
}

func TestResolve_非法卡先被拦下(t *testing.T) {
	c := card()
	c.Options = c.Options[:1]
	_, err := c.Resolve(Action{Kind: ActionKeepCurrent})
	if !errors.Is(err, ErrInvalidChoice) {
		t.Fatalf("期望 ErrInvalidChoice，得 %v", err)
	}
}

func TestParseAction(t *testing.T) {
	for _, k := range []string{"choose", "keep_current"} {
		if _, err := ParseAction(k); err != nil {
			t.Fatalf("%q 应合法：%v", k, err)
		}
	}
	if _, err := ParseAction("write_formula"); err == nil {
		t.Fatal("未知动作 write_formula 必须被拒绝")
	}
}

func TestIsChange(t *testing.T) {
	c := card()
	if c.IsChange("a") {
		t.Fatal("目标等于当前值 ⇒ 非变更")
	}
	if !c.IsChange("b") {
		t.Fatal("目标不同于当前值 ⇒ 是变更")
	}
}

// ───────────────────────────── History / LastStable ─────────────────────────────

func TestHistory_Append不改入参(t *testing.T) {
	h := History{entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose)}
	h2 := h.Append(entry("e2", "b", "c", time.Unix(2, 0), false, ActionChoose))
	if len(h) != 1 {
		t.Fatalf("原切片被修改：len=%d", len(h))
	}
	if len(h2) != 2 {
		t.Fatalf("新切片应为 2，得 %d", len(h2))
	}
}

func TestLastStable_返回变更前的值(t *testing.T) {
	// a → b（可逆）。回滚目标应是 a（撤销这次变更），而不是 b。
	h := History{entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose)}
	got, ok := h.LastStable()
	if !ok {
		t.Fatal("应找到稳定版本")
	}
	if got != "a" {
		t.Fatalf("回滚目标应为变更前的值 a，得 %q（返回 ToOption 会让回滚变成空操作）", got)
	}
}

func TestLastStable_跳过保持现状(t *testing.T) {
	h := History{
		entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose),
		entry("e2", "b", "b", time.Unix(2, 0), true, ActionKeepCurrent), // 保持现状
	}
	got, ok := h.LastStable()
	if !ok || got != "a" {
		t.Fatalf("期望 a，得 %q ok=%v", got, ok)
	}
}

func TestLastStable_最近变更不可逆则拒绝(t *testing.T) {
	h := History{
		entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose),
		entry("e2", "b", "c", time.Unix(2, 0), false, ActionChoose), // 不可逆
	}
	_, ok := h.LastStable()
	if ok {
		t.Fatal("最近一次变更不可逆 ⇒ 必须报「无可回滚版本」，不得越过它回滚到 a")
	}
}

func TestLastStable_空历史(t *testing.T) {
	empty := History{}
	if _, ok := empty.LastStable(); ok {
		t.Fatal("空历史不应有稳定版本")
	}
}

func TestPlanRollback_生成计划(t *testing.T) {
	c := card()
	c.CurrentKey = "b" // 已从 a 改到 b
	h := History{entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose)}
	p, err := PlanRollback(c, h)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if p.FromOption != "b" || p.ToOption != "a" {
		t.Fatalf("期望 b→a，得 %s→%s", p.FromOption, p.ToOption)
	}
	if p.UndoOfEntryID != "e1" {
		t.Fatalf("应指向被撤销的记录 e1，得 %q", p.UndoOfEntryID)
	}
	// ★ 回滚也必须重算：只改参数不重算桶 = 数字错得毫无征兆
	if len(p.AffectedBuckets) == 0 {
		t.Fatal("回滚计划必须带 affectedBuckets（否则桶不重算）")
	}
}

func TestPlanRollback_无稳定版本(t *testing.T) {
	c := card()
	if _, err := PlanRollback(c, History{}); !errors.Is(err, ErrNoStableVersion) {
		t.Fatalf("期望 ErrNoStableVersion，得 %v", err)
	}
}

func TestPlanRollback_已在稳定版本(t *testing.T) {
	c := card() // current = a
	h := History{entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose)}
	// 当前 a 已经是 e1 之前的值 ⇒ 再回滚就是空操作，应明确报错
	if _, err := PlanRollback(c, h); !errors.Is(err, ErrNoStableVersion) {
		t.Fatalf("期望 ErrNoStableVersion（回滚会变成空操作），得 %v", err)
	}
}

func TestPlanRollback_nil卡(t *testing.T) {
	var c *Choice
	if _, err := PlanRollback(c, History{}); !errors.Is(err, ErrInvalidChoice) {
		t.Fatalf("期望 ErrInvalidChoice，得 %v", err)
	}
}

// ───────────────────────────── 采纳学习 ─────────────────────────────

func TestLearnPreference_样本不足(t *testing.T) {
	h := History{
		entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose),
		entry("e2", "b", "c", time.Unix(2, 0), true, ActionChoose),
	}
	p := LearnPreference("ceo", h, func(_, k string) (Risk, bool) { return RiskHigh, true })
	if p.Samples != 2 {
		t.Fatalf("样本应为 2，得 %d", p.Samples)
	}
	// 2 < 3 ⇒ 不应给倾向性标签，而应说明「依据不足」
	if !contains(p.Note, "暂不做倾向性推荐") {
		t.Fatalf("样本不足时说明措辞不对：%q", p.Note)
	}
	if contains(p.Note, "保守") || contains(p.Note, "激进") {
		t.Fatalf("样本不足时不应给出倾向性标签：%q", p.Note)
	}
	if p.RiskAppetite == 0 && p.Samples > 0 {
		// 两次 high ⇒ 分值应为 1，不该是 0
		t.Fatalf("分值计算错误：%v", p.RiskAppetite)
	}
}

func TestLearnPreference_只统计实质变更(t *testing.T) {
	h := History{
		entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose),
		entry("e2", "b", "b", time.Unix(2, 0), true, ActionKeepCurrent), // 不计入
	}
	p := LearnPreference("ceo", h, func(_, k string) (Risk, bool) { return RiskLow, true })
	if p.Samples != 1 {
		t.Fatalf("保持现状不应计入样本，得 %d", p.Samples)
	}
}

func TestLearnPreference_按账号隔离(t *testing.T) {
	h := History{entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose)}
	p := LearnPreference("someone.else", h, func(_, k string) (Risk, bool) { return RiskLow, true })
	if p.Samples != 0 {
		t.Fatalf("他人决策不应计入，得 %d", p.Samples)
	}
}

func TestLearnPreference_无样本(t *testing.T) {
	p := LearnPreference("ceo", History{}, nil)
	if p.Samples != 0 || p.Note != "暂无决策样本" {
		t.Fatalf("无样本时措辞不对：%+v", p)
	}
}

func TestLearnPreference_偏保守措辞(t *testing.T) {
	var h History
	for i := 0; i < 4; i++ {
		h = h.Append(entry("e"+string(rune('1'+i)), "x", "y", time.Unix(int64(i+1), 0), true, ActionChoose))
	}
	p := LearnPreference("ceo", h, func(_, k string) (Risk, bool) { return RiskLow, true })
	if p.RiskAppetite != -1 {
		t.Fatalf("全选 low ⇒ 分值应为 -1，得 %v", p.RiskAppetite)
	}
	if !contains(p.Note, "保守") {
		t.Fatalf("措辞应体现保守：%q", p.Note)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ───────────────────────────── 排序 ─────────────────────────────

func TestSortHistoryAsc_确定顺序(t *testing.T) {
	h := History{
		entry("e2", "b", "c", time.Unix(2, 0), true, ActionChoose),
		entry("e1", "a", "b", time.Unix(1, 0), true, ActionChoose),
		entry("e0", "z", "a", time.Unix(1, 0), true, ActionChoose), // 与 e1 同时刻
	}
	out := SortHistoryAsc(h)
	if out[0].ID != "e0" || out[1].ID != "e1" || out[2].ID != "e2" {
		t.Fatalf("同时刻应按 id 兜底排序，得 %s,%s,%s", out[0].ID, out[1].ID, out[2].ID)
	}
	// 入参不被修改
	if h[0].ID != "e2" {
		t.Fatal("入参被修改")
	}
}

func TestSortOptions_推荐优先然后风险升序(t *testing.T) {
	opts := []Option{
		opt("c", RiskHigh, true),
		opt("a", RiskLow, true),
		opt("b", RiskMedium, true),
	}
	opts[0].Recommended = true
	out := SortOptions(opts)
	if out[0].Key != "c" {
		t.Fatalf("推荐项应排最前，得 %q", out[0].Key)
	}
	if out[1].Key != "a" || out[2].Key != "b" {
		t.Fatalf("其余应风险升序，得 %q,%q", out[1].Key, out[2].Key)
	}
}

// ───────────────────────────── Current / Option ─────────────────────────────

func TestCurrent_命中(t *testing.T) {
	if c := card().Current(); c == nil || c.Key != "a" {
		t.Fatalf("应命中 a，得 %+v", c)
	}
}

func TestOption_未命中(t *testing.T) {
	o, ok := card().Option("zzz")
	if ok || o != nil {
		t.Fatal("未命中应返回 false 且 nil")
	}
}

func TestAuditActionNames(t *testing.T) {
	// 动作名是事后追责的唯一入口，改动必须是有意识的
	if AuditActionDecide != "strategy.decide" {
		t.Fatalf("审计动作名漂移：%q", AuditActionDecide)
	}
	if AuditActionRollback != "strategy.rollback" {
		t.Fatalf("审计动作名漂移：%q", AuditActionRollback)
	}
	// ★ 接口层留痕的动作名必须与历史动作名**互不相同**。
	//
	//	两者同名会让 History()（按 action 过滤）把不带 reversible 的
	//	接口层记录也算进历史，LastStable() 遂即停下 —— 回滚永久不可达。
	//	这条断言就是那个真实故障的守门人。
	if AuditActionDecideAttempt == AuditActionDecide {
		t.Fatalf("接口层留痕动作名不得与历史动作名相同（%q）—— 会锁死回滚", AuditActionDecide)
	}
	if AuditActionRollbackAttempt == AuditActionRollback {
		t.Fatalf("接口层留痕动作名不得与历史动作名相同（%q）—— 会锁死回滚", AuditActionRollback)
	}
	if AuditActionDecideAttempt != "strategy.decide.attempt" {
		t.Fatalf("接口层决策留痕动作名漂移：%q", AuditActionDecideAttempt)
	}
	if AuditActionRollbackAttempt != "strategy.rollback.attempt" {
		t.Fatalf("接口层回滚留痕动作名漂移：%q", AuditActionRollbackAttempt)
	}
}

func TestVersion(t *testing.T) {
	if Version != "1.0" {
		t.Fatalf("契约版本应跟随 contracts/strategy-choice.ts 的 1.0，得 %q", Version)
	}
}
