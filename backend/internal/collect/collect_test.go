// collect_test.go —— M-COLLECT 纯逻辑闸门。
//
// 覆盖三块（对应三条用户要求）：
//   1. 自适应降速档位 0.5s → 120s，含边界与封顶
//   2. 断点续传状态推进与恢复决策
//   3. 完整性守卫判定（**含大量负向用例** —— 守卫放行残缺数据是本项目最高危缺陷）
package collect_test

import (
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/collect"
)

// ───────────────────── 1. 自适应降速档位 ─────────────────────

// TestGearTable_SpansHalfSecondTo120s 断言档位表覆盖用户要求的区间端点。
//
// 用户原话：「从 0.5s 到 120 秒需要有档位自动调整」。
// 端点写错（例如末档只有 60s）会让「撞限流时退不够」，是需求级的错。
func TestGearTable_SpansHalfSecondTo120s(t *testing.T) {
	first := collect.GearTable[0]
	last := collect.GearTable[len(collect.GearTable)-1]

	if first.Delay != 500*time.Millisecond {
		t.Errorf("首档应为 0.5s，实际 %v", first.Delay)
	}
	if last.Delay != 120*time.Second {
		t.Errorf("末档应为 120s（用户明确要求封顶），实际 %v", last.Delay)
	}
}

// TestGearTable_StrictlyIncreasing 断言档位间隔严格单调递增。
//
// 若某档比前一档还短，升档后反而更快 ⇒ 必然继续撞限流，退避机制形同虚设。
func TestGearTable_StrictlyIncreasing(t *testing.T) {
	for i := 1; i < len(collect.GearTable); i++ {
		prev, cur := collect.GearTable[i-1], collect.GearTable[i]
		if cur.Delay <= prev.Delay {
			t.Errorf("档位 %d(%v) 未严格大于上一档 %d(%v) —— "+
				"升档必须变慢，否则退避失效", cur.Index, cur.Delay, prev.Index, prev.Delay)
		}
		if cur.Index != i {
			t.Errorf("档位序号必须等于数组下标（%d），实际 %d", i, cur.Index)
		}
	}
}

// TestGearAt_ClampsOutOfRange 断言越界档位被钳到边界而非 panic/零值。
//
// 档位来自 DB，可能被 IT 手工改坏。钳位的意义是「采集循环不因坏档位崩掉」。
func TestGearAt_ClampsOutOfRange(t *testing.T) {
	cases := []struct {
		in       int
		wantIdx  int
		wantWait time.Duration
	}{
		{-99, 0, 500 * time.Millisecond},
		{0, 0, 500 * time.Millisecond},
		{999, len(collect.GearTable) - 1, 120 * time.Second},
	}
	for _, c := range cases {
		got := collect.GearAt(c.in)
		if got.Index != c.wantIdx || got.Delay != c.wantWait {
			t.Errorf("GearAt(%d) = idx %d / %v，期望 idx %d / %v",
				c.in, got.Index, got.Delay, c.wantIdx, c.wantWait)
		}
	}
}

// TestGovernor_ThrottleEscalatesOneStepPerHit 断言每次限流**只升一档**。
//
// 为什么不是一次跳到底：一次 429 可能只是瞬时拥塞，直接退到 120s
// 会让正常采集被无谓拖慢数小时。分级试探才是对的。
func TestGovernor_ThrottleEscalatesOneStepPerHit(t *testing.T) {
	g := collect.NewGovernor(0, 0)
	if g.Delay() != 500*time.Millisecond {
		t.Fatalf("初始应为 0.5s，实际 %v", g.Delay())
	}
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 15 * time.Second, 30 * time.Second,
		60 * time.Second, 120 * time.Second,
	}
	for i, w := range want {
		ceiling := g.OnThrottled()
		if g.Delay() != w {
			t.Fatalf("第 %d 次限流后应为 %v，实际 %v", i+1, w, g.Delay())
		}
		// 只有最后一次（到达末档）才应报封顶
		wantCeiling := i == len(want)-1
		if ceiling != wantCeiling {
			t.Errorf("第 %d 次限流后 atCeiling=%v，期望 %v", i+1, ceiling, wantCeiling)
		}
	}
}

// TestGovernor_ThrottleAtCeilingStaysAtCeiling 断言到封顶后继续限流不会溢出错档。
//
// 越界会让 OnThrottled 把 index 推过数组长度；若 GearAt 没有钳位保护，
// 就会 panic 或退化成 0（反而变成最快速度）—— 那是灾难性的。
func TestGovernor_ThrottleAtCeilingStaysAtCeiling(t *testing.T) {
	g := collect.NewGovernor(collect.GearTableMaxIndex(), 0)
	for i := 0; i < 10; i++ {
		if !g.OnThrottled() {
			t.Fatalf("已在末档，第 %d 次限流应仍报封顶", i+1)
		}
	}
	if g.Index() != collect.GearTableMaxIndex() {
		t.Errorf("档位溢出：应为 %d，实际 %d", collect.GearTableMaxIndex(), g.Index())
	}
	if g.Delay() != 120*time.Second {
		t.Errorf("封顶后间隔应保持 120s，实际 %v", g.Delay())
	}
}

// TestGovernor_SuccessDowngradesOnlyAfterThreshold 断言降档门槛。
//
// 这是「自动降速」的另一半：被限流后不能永远停在慢档，
// 但恢复速度必须**滞后于**成功次数（猜错代价高）。
func TestGovernor_SuccessDowngradesOnlyAfterThreshold(t *testing.T) {
	// 先升到档位 3（4s，recover_after=8）
	g := collect.NewGovernor(0, 0)
	for i := 0; i < 3; i++ {
		g.OnThrottled()
	}
	if g.Index() != 3 {
		t.Fatalf("前置失败：应升到档位 3，实际 %d", g.Index())
	}

	need := collect.GearAt(3).RecoverAfter
	for i := 1; i < need; i++ {
		if g.OnSuccess() {
			t.Fatalf("第 %d 次成功就降档了，应等到第 %d 次", i, need)
		}
	}
	if !g.OnSuccess() {
		t.Fatalf("第 %d 次成功应触发降档", need)
	}
	if g.Index() != 2 {
		t.Errorf("降档后应为 2，实际 %d", g.Index())
	}
	if g.OkStreak() != 0 {
		t.Errorf("降档后成功计数应清零，实际 %d", g.OkStreak())
	}
}

// TestGovernor_SuccessAtFastestGearNeverGoesNegative 断言最快档不会降到负档位。
//
// 负档位若被写入 DB，下次恢复时 GearAt(-1) 会读出错档 —— 而正常情况
// 下「一直成功」是最常见的路径，所以这条必须成立。
func TestGovernor_SuccessAtFastestGearNeverGoesNegative(t *testing.T) {
	g := collect.NewGovernor(0, 0)
	for i := 0; i < 50; i++ {
		if g.OnSuccess() {
			t.Fatalf("第 %d 次成功在最快档不该降档", i+1)
		}
	}
	if g.Index() != 0 {
		t.Errorf("档位应为 0，实际 %d", g.Index())
	}
}

// TestGovernor_ThrottleResetsOkStreak 断言限流会清零成功计数。
//
// 否则「限流前的部分成功」会累计，导致刚被限流就立刻降档 —— 来回抖动。
func TestGovernor_ThrottleResetsOkStreak(t *testing.T) {
	g := collect.NewGovernor(3, 0)
	need := collect.GearAt(3).RecoverAfter
	for i := 1; i < need; i++ {
		g.OnSuccess()
	}
	g.OnThrottled()
	if g.OkStreak() != 0 {
		t.Errorf("限流后成功计数应清零，实际 %d", g.OkStreak())
	}
}

// ───────────────────── 2. 断点续传 ─────────────────────

// TestAdvance_PersistsCursorAndCounters 断言分页成功后游标与计数正确推进。
func TestAdvance_PersistsCursorAndCounters(t *testing.T) {
	j := &collect.JobState{ID: "job.x", Status: collect.StatusRunning}
	j.Advance("cursor-page2", 100)
	if j.Cursor != "cursor-page2" {
		t.Errorf("游标应为 cursor-page2，实际 %q", j.Cursor)
	}
	if j.PagesDone != 1 || j.RowsStaged != 100 {
		t.Errorf("计数应为 1 页/100 行，实际 %d 页/%d 行", j.PagesDone, j.RowsStaged)
	}
	if j.Status != collect.StatusRunning {
		t.Errorf("仍有下一页，状态应保持 RUNNING，实际 %s", j.Status)
	}
}

// TestAdvance_EmptyCursorMeansComplete 断言游标耗尽即完成。
//
// ★ 这条是「避免重复调用撞限额」的关键：若空游标不置 COMPLETE，
//   下一轮会把「到底」误解成「还没开始」，从头重放整个范围。
func TestAdvance_EmptyCursorMeansComplete(t *testing.T) {
	j := &collect.JobState{ID: "job.x", Status: collect.StatusRunning}
	j.Advance("", 50)
	if j.Status != collect.StatusComplete {
		t.Errorf("空游标应置 COMPLETE，实际 %s", j.Status)
	}
	if ok, _ := j.Resume(); ok {
		t.Error("已完成的作业不应允许再次续跑（会重复调用上游）")
	}
}

// TestResume_ThrottledAndPausedAreResumable 断言限流/暂停**不是**失败。
//
// ★ 这是 KODP 时代最贵教训的固化：把 429 当失败 ⇒ 整月重放 ⇒ 再次 429 ⇒
//   永远跑不完。限流只是「暂时没跑完」，必须可续。
func TestResume_ThrottledAndPausedAreResumable(t *testing.T) {
	for _, st := range []string{
		collect.StatusThrottled, collect.StatusPaused, collect.StatusRunning,
		collect.StatusPending,
	} {
		j := &collect.JobState{ID: "job.x", Status: st}
		ok, why := j.Resume()
		if !ok {
			t.Errorf("状态 %s 应可续跑，却被拒绝：%s", st, why)
		}
	}
}

// TestResume_FailedRequiresHuman 断言硬失败不被自动重跑。
//
// 自动重跑认证失效的作业只会持续失败并烧配额，还可能触发风控。
func TestResume_FailedRequiresHuman(t *testing.T) {
	j := &collect.JobState{
		ID: "job.x", Status: collect.StatusFailed,
		LastError: "401 invalid_grant",
	}
	ok, why := j.Resume()
	if ok {
		t.Fatal("FAILED 作业不应自动续跑")
	}
	if !strings.Contains(why, "401 invalid_grant") {
		t.Errorf("拒绝原因应带上原始错误以便人工排查，实际 %q", why)
	}
}

// TestIdemKey_SameNaturalKeySameKey 断言幂等键由业务标识决定。
func TestIdemKey_SameNaturalKeySameKey(t *testing.T) {
	a := collect.IdemKey("job.1", "order-999")
	b := collect.IdemKey("job.1", "order-999")
	if a != b {
		t.Errorf("同一 (job, naturalKey) 必须产生同一幂等键：%q vs %q", a, b)
	}
	if collect.IdemKey("job.1", "order-999") == collect.IdemKey("job.1", "order-1000") {
		t.Error("不同 naturalKey 必须是不同幂等键")
	}
	if collect.IdemKey("job.1", "x") == collect.IdemKey("job.2", "x") {
		t.Error("不同 job 必须隔离幂等空间（重采需可对照，不能被旧批次吞掉）")
	}
}

// TestIdemKey_AvoidsAmbiguousConcatenation 断言分隔符能防拼接歧义。
//
// 若直接用 + 拼接，(job="a", key="bc") 与 (job="ab", key="c") 会撞成同键
// 「abc」—— 两条完全不同的记录被判为同一行，静默丢数据。
func TestIdemKey_AvoidsAmbiguousConcatenation(t *testing.T) {
	x := collect.IdemKey("a", "bc")
	y := collect.IdemKey("ab", "c")
	if x == y {
		t.Errorf("幂等键存在拼接歧义：(a,bc) 与 (ab,c) 均为 %q —— "+
			"会让不同记录被误判为重复而丢数", x)
	}
}

// TestIdemKey_ContainsNoNULByte 断言幂等键不含 NUL 字节。
//
// ★ 真实事故（真库抓出，单测抓不到）：
//   第一版用 "\x00" 做分隔符，本地单测全绿 —— 因为 Go 字符串完全允许 NUL。
//   但真库一跑就整体失败：
//     ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
//   PostgreSQL 的 text/varchar **不允许存 NUL**。也就是说这是
//   「Go 合法、PG 非法」的跨层约束，纯单元测试在原理上就不可能发现。
//
//   本用例的意义：把那条只有真库才会报的约束，前移到单测里。
//   任何人若把分隔符改回 NUL，这里立刻红，而不用等到真库集成阶段。
func TestIdemKey_ContainsNoNULByte(t *testing.T) {
	k := collect.IdemKey("job.1", "order-1")
	if strings.ContainsRune(k, 0) {
		t.Errorf("幂等键含 NUL 字节（%q）—— PostgreSQL text 类型会以 "+
			"SQLSTATE 22021 拒绝，导致采集**整体不可用**。"+
			"分隔符须改用 PG 可存的字符（如 U+001F）。", k)
	}
}

// TestIdemKey_SeparatorIsNonPrintableButStorable 断言分隔符的选取原则：
// 必须是不可能出现在业务键里的字符，才能兼顾「防歧义」与「可入库」。
//
// 业务键（订单号/SKU/店铺 id）都是可见字符，因此用 ASCII 控制字符
// US(U+001F) 既安全又不冲突。
func TestIdemKey_SeparatorIsNonPrintableButStorable(t *testing.T) {
	k := collect.IdemKey("job", "key")
	if len(k) != len("job")+len("key")+1 {
		t.Fatalf("幂等键长度异常（分隔符应为 1 字节）：%q len=%d", k, len(k))
	}
	sep := k[len("job")]
	// 必须是控制字符（不可能出现在业务键里），但不能是 NUL（PG 存不了）
	if sep == 0 {
		t.Error("分隔符是 NUL —— PG 拒绝存储")
	}
	if sep >= 0x20 {
		t.Errorf("分隔符 %q 是可打印字符 —— 业务键里可能出现它，"+
			"会让『防拼接歧义』失效（如 a|bc 与 ab|c 撞键）", sep)
	}
}

// ───────────────────── 3. 完整性守卫 ─────────────────────

func fullInput() collect.GuardInput {
	return collect.GuardInput{
		ExpectedPages: 3, ActualPages: 3,
		ExpectedRows: 300, ActualRows: 300,
		CursorEnded: true, RequiredOK: true,
		Coverage: 1.0, HasCoverage: true,
	}
}

// TestEvaluate_PassesWhenFullyComplete 断言「真的完整」才通过（正向基线）。
//
// 没有这条，后面所有负向用例都可能因为守卫「恒不通过」而假绿。
func TestEvaluate_PassesWhenFullyComplete(t *testing.T) {
	v := collect.Evaluate(fullInput())
	if !v.Passed {
		t.Fatalf("完整输入应通过，却被拒：%v", v.Reasons)
	}
	if len(v.Reasons) != 0 {
		t.Errorf("通过时不应有原因，实际 %v", v.Reasons)
	}
}

// TestEvaluate_RejectsEachIncompleteness 负向用例：**逐个**破坏每个子项。
//
// 这是守卫最重要的测试。守卫的价值全在「能不能挡住残缺」——
// 正向用例只能证明它不误杀，负向用例才能证明它**会响**。
func TestEvaluate_RejectsEachIncompleteness(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*collect.GuardInput)
		wantInMsg string
	}{
		{
			name:      "页数少于声明（提前中断）",
			mutate:    func(g *collect.GuardInput) { g.ActualPages = 2 },
			wantInMsg: "页数未闭合",
		},
		{
			name:      "上游未声明页数（无法判定）",
			mutate:    func(g *collect.GuardInput) { g.ExpectedPages = 0 },
			wantInMsg: "未声明总页数",
		},
		{
			name:      "游标未到底",
			mutate:    func(g *collect.GuardInput) { g.CursorEnded = false },
			wantInMsg: "游标未到底",
		},
		{
			name:      "行数不符（少了几行）",
			mutate:    func(g *collect.GuardInput) { g.ActualRows = 299 },
			wantInMsg: "行数不符",
		},
		{
			name:      "行数多出（重复落库）",
			mutate:    func(g *collect.GuardInput) { g.ActualRows = 301 },
			wantInMsg: "行数不符",
		},
		{
			name:      "必填字段缺失",
			mutate:    func(g *collect.GuardInput) { g.RequiredOK = false },
			wantInMsg: "必填字段",
		},
		{
			name:      "覆盖率无法计算",
			mutate:    func(g *collect.GuardInput) { g.HasCoverage = false },
			wantInMsg: "无法计算",
		},
		{
			name:      "覆盖率低于下限",
			mutate:    func(g *collect.GuardInput) { g.Coverage = 0.79 },
			wantInMsg: "低于下限",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := fullInput()
			c.mutate(&in)
			v := collect.Evaluate(in)
			if v.Passed {
				t.Fatalf("守卫放行了不完整数据（%s）—— 残缺数据将进入经营报表", c.name)
			}
			joined := strings.Join(v.Reasons, " | ")
			if !strings.Contains(joined, c.wantInMsg) {
				t.Errorf("原因未指向真问题：期望含 %q，实际 %q", c.wantInMsg, joined)
			}
		})
	}
}

// TestEvaluate_CoverageAtFloorPasses 断言覆盖率**等于**下限算通过（边界）。
//
// 下限语义是「≥ 0.80」，不是「> 0.80」。写错会让恰好达标的批次被卡住。
func TestEvaluate_CoverageAtFloorPasses(t *testing.T) {
	in := fullInput()
	in.Coverage = collect.CoverageFloor
	if v := collect.Evaluate(in); !v.Passed {
		t.Errorf("覆盖率恰好等于下限应通过，实际被拒：%v", v.Reasons)
	}
}

// TestEvaluate_ZeroRowsWithZeroExpectedIsNotAutoPass 断言「声明 0 行」不被当作完整。
//
// 上游声明 0 行时若只看「行数一致」就会通过 —— 但 0 行几乎总是
// 「查询条件写错 / 权限不足」的表现，而非真的没有数据。
// 此时必须靠「页数声明 + 游标到底」来判定，不能仅凭行数相等放行。
func TestEvaluate_ZeroRowsWithZeroExpectedIsNotAutoPass(t *testing.T) {
	in := fullInput()
	in.ExpectedRows = 0
	in.ActualRows = 0
	// 其余子项若都成立，允许通过（可能确实是空月）；但若游标没到底就不能通过。
	in.CursorEnded = false
	if v := collect.Evaluate(in); v.Passed {
		t.Error("0 行且游标未到底时不应通过 —— 无法区分『确实无数据』与『查询被打断』")
	}
}

// TestEvaluate_PassedVerdictReasonsIsNonNil 断言「通过」时 Reasons 是**非 nil 空切片**。
//
// ★ 真实事故（真库抓出，单测抓不到）：
//   表定义 `reasons text[] NOT NULL DEFAULT '{}'`，看着有默认值很安全；
//   但 **DEFAULT 只在「不提供该列」时生效** —— 显式传 NULL 直接撞 NOT NULL：
//     ERROR: null value in column "reasons" ... (SQLSTATE 23502)
//   Go 的 nil []string 经 pgx 正是编码成 SQL NULL。
//
//   阴险之处：该 bug **只在守卫通过时触发**（通过时无原因= nil，
//   不通过时至少一条原因= 非 nil）。也就是说它专门炸在成功分支上 ——
//   「数据终于补齐了、可以放行了」的那一刻整体失败。
//
//   本用例把这条约束前移到单测：任何让通过路径返回 nil 的改动立刻变红。
func TestEvaluate_PassedVerdictReasonsIsNonNil(t *testing.T) {
	v := collect.Evaluate(fullInput())
	if !v.Passed {
		t.Fatalf("前置失败：完整输入应通过，实际 %v", v.Reasons)
	}
	if v.Reasons == nil {
		t.Error("通过时 Reasons 为 nil —— 入库会以 SQLSTATE 23502 失败" +
			"（DEFAULT '{}' 不会在显式传 NULL 时生效）。应返回 []string{}。")
	}
	if len(v.Reasons) != 0 {
		t.Errorf("通过时不应有原因，实际 %v", v.Reasons)
	}
}

// TestEvaluate_FailedVerdictReasonsIsNonNil 断言「拒绝」时 Reasons 非空且非 nil。
//
// 拒绝时 Reasons 绝不能为空 —— 那是「拒绝了但不说为什么」，
// 运维无从修补数据，只能盲目重跑（反复触碰限流）。
func TestEvaluate_FailedVerdictReasonsIsNonNil(t *testing.T) {
	in := fullInput()
	in.CursorEnded = false
	v := collect.Evaluate(in)
	if v.Passed {
		t.Fatal("不完整输入不应通过")
	}
	if v.Reasons == nil || len(v.Reasons) == 0 {
		t.Error("拒绝时必须给出原因（否则运维无法修补，只能盲目重跑并反复撞限流）")
	}
}

// TestEvaluate_MultipleFailuresReportAllReasons 断言多问题时全部报出。
//
// 只报第一个原因会让运维「修一个跑一次」，反复触碰限流。
func TestEvaluate_MultipleFailuresReportAllReasons(t *testing.T) {
	in := collect.GuardInput{
		ExpectedPages: 5, ActualPages: 2,
		ExpectedRows: 500, ActualRows: 200,
		CursorEnded: false, RequiredOK: false,
		HasCoverage: false,
	}
	v := collect.Evaluate(in)
	if v.Passed {
		t.Fatal("严重残缺输入不应通过")
	}
	if len(v.Reasons) < 5 {
		t.Errorf("应报出全部 5 类问题，实际只报了 %d 条：%v", len(v.Reasons), v.Reasons)
	}
}
