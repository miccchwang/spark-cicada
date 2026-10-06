// Package collect —— M-COLLECT 采集模块（docs/02 M-COLLECT）。
//
// 职责：第三方取数 —— 认证、限流、分页、重试、**原始落库（临时仓库）**。
//
// 本文件只做**纯逻辑**（不碰 DB、不碰网络），因此可被穷举单测：
//   * 自适应降速档位（0.5s → 120s）
//   * 断点续传的推进/恢复决策
//   * 幂等键构造
//   * 完整性守卫判定
//
// ★ 为什么要拆出纯逻辑：
//   这些规则是「采集能不能跑完」的核心，却最难在真库/真网络下穷举验证
//   （限流要真被限、断点要真断电）。做成纯函数后，可以精确断言
//   「第 3 次 429 后档位是几、下次间隔多少秒」。IO 部分（store.go）只做搬运。
package collect

import (
	"fmt"
	"time"
)

// ───────────────────────── 自适应降速（离散档位）─────────────────────────

// Gear 一个降速档位。用户要求「从 0.5s 到 120s 有档位自动调整」，
// 采用**离散档位**而非连续退避：可预测、可审计、可枚举断言。
type Gear struct {
	Index        int           // 档位序号，0 = 最快
	Delay        time.Duration // 该档位的调用间隔
	Label        string        // 人类可读（运维日志/告警用）
	RecoverAfter int           // 连续成功多少次才允许降一档
}

// GearTable 默认档位表：0.5s → 120s，共 9 档，倍率约 ×2，末档封顶。
//
// 与迁移 0005 的 collect_rate_gear 种子表**必须一致**；
// 一致性由 TestGearTableMatchesMigration 断言（防两处漂移）。
var GearTable = []Gear{
	{0, 500 * time.Millisecond, "0.5 秒 · 正常", 5},
	{1, 1 * time.Second, "1 秒", 5},
	{2, 2 * time.Second, "2 秒", 5},
	{3, 4 * time.Second, "4 秒 · 轻限流", 8},
	{4, 8 * time.Second, "8 秒", 8},
	{5, 15 * time.Second, "15 秒 · 明显限流", 10},
	{6, 30 * time.Second, "30 秒", 12},
	{7, 60 * time.Second, "60 秒 · 重限流", 15},
	{8, 120 * time.Second, "120 秒 · 封顶", 20},
}

// GearTableMaxIndex 末档（封顶）序号。
func GearTableMaxIndex() int { return len(GearTable) - 1 }

// GearAt 取某档位；越界时钳到 [0, 末档]。
//
// ★ 钳位而非报错：档位来自 DB（可能被 IT 手工改坏），
//   采集循环不应因为一个坏档位而整体崩掉 —— 退到边界继续跑，并记日志。
func GearAt(i int) Gear {
	if i < 0 {
		i = 0
	}
	if i > GearTableMaxIndex() {
		i = GearTableMaxIndex()
	}
	return GearTable[i]
}

// RateGovernor 自适应降速状态机。
//
// 规则（用户要求「自动降速」）：
//   - 遇限流（429/频控）：**升档**（间隔变长），并清零成功计数。
//   - 连续成功达该档 recover_after 次：**降一档**（逐步恢复速度）。
//
// 为什么降档比升档保守：升档是对「已经发生的限流」反应（必须立刻退让），
// 降档是对「推测已经恢复」下注（猜错会再撞限流，代价更高）。所以降档门槛更高。
type RateGovernor struct {
	index    int
	okStreak int
}

// NewGovernor 从持久化状态恢复（断点续传时用）。
func NewGovernor(gearIndex, okStreak int) *RateGovernor {
	return &RateGovernor{index: gearIndex, okStreak: okStreak}
}

// Index 当前档位。
func (g *RateGovernor) Index() int { return g.index }

// OkStreak 当前连续成功次数。
func (g *RateGovernor) OkStreak() int { return g.okStreak }

// Delay 当前应使用的调用间隔。
func (g *RateGovernor) Delay() time.Duration { return GearAt(g.index).Delay }

// OnThrottled 记录一次「被限流」，返回是否**已到封顶**（用于告警：
// 到封顶仍被限流说明配额/权限有问题，不只是一时拥塞）。
func (g *RateGovernor) OnThrottled() (atCeiling bool) {
	g.okStreak = 0
	if g.index < GearTableMaxIndex() {
		g.index++
	}
	return g.index >= GearTableMaxIndex()
}

// OnSuccess 记录一次成功。达到该档 recover_after 后降一档并重置计数。
// 返回是否发生了降档（可观测性）。
func (g *RateGovernor) OnSuccess() (downgraded bool) {
	g.okStreak++
	if g.index <= 0 {
		g.okStreak = 0
		return false
	}
	if g.okStreak >= GearAt(g.index).RecoverAfter {
		g.index--
		g.okStreak = 0
		return true
	}
	return false
}

// ───────────────────────── 断点续传 ─────────────────────────

// JobState 采集作业的可持久化状态（对应 collect_job 一行）。
//
// 这是断点续传的**全部状态**：只要这行还在，无论进程怎么死都能接着跑。
type JobState struct {
	ID          string
	SlotID      string
	Scope       string
	Region      string
	Cursor      string // 上游不透明游标；空 = 尚未开始或已到底
	PagesDone   int
	RowsStaged  int64
	GearIndex   int
	OkStreak    int
	Status      string
	LastError   string
}

// 作业状态常量（与迁移 0005 的 CHECK 约束一致）。
const (
	StatusPending   = "PENDING"
	StatusRunning   = "RUNNING"
	StatusPaused    = "PAUSED"
	StatusThrottled = "THROTTLED"
	StatusComplete  = "COMPLETE"
	StatusFailed    = "FAILED"
)

// Advance 在一次分页成功后推进状态（**断点续传的核心**）。
//
// ★ 纪律：调用者必须在**同一事务内**先写 staging_page（该页数据）、
//   再调用本函数并落库游标。这样「数据已落」与「游标已推」是原子的 ——
//   否则会出现「数据落了但游标没推」⇒ 重启后重复调用同一页（浪费配额），
//   或「游标推了但数据没落」⇒ 永久丢页（更危险）。
//
// nextCursor 为空表示上游已到底 ⇒ 置 COMPLETE。
func (j *JobState) Advance(nextCursor string, rowsInPage int64) {
	j.Cursor = nextCursor
	j.PagesDone++
	j.RowsStaged += rowsInPage
	if nextCursor == "" {
		j.Status = StatusComplete
		return
	}
	j.Status = StatusRunning
}

// Resume 判断作业能否续跑，以及从什么状态继续。
//
// 返回 (可续跑, 原因)。不可续跑的情形：
//   - COMPLETE：已跑完，无需再跑（**避免重复调用撞限额**）
//   - FAILED  ：硬失败需人工（如认证失效），自动重跑只会继续失败并烧配额
//
// 关键：THROTTLED / PAUSED / RUNNING 都**可以续跑** —— 它们不是失败，
// 只是「暂时没跑完」。把限流当失败是 KODP 时代最贵的错误：
// 一次 429 导致整月重放，然后再次 429，永远跑不完。
func (j *JobState) Resume() (bool, string) {
	switch j.Status {
	case StatusComplete:
		return false, "已完成，跳过以避免重复调用上游"
	case StatusFailed:
		return false, "硬失败需人工处理： " + j.LastError
	default:
		return true, ""
	}
}

// idemSeparator 是拼接幂等键用的分隔符。
//
// ★ 为什么不能用 "\x00"（真实事故，真库抓出）：
//   第一版用了 NUL 做分隔符，本地单测全绿（Go 字符串能装 NUL），
//   但真库一跑就死：
//     ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
//   —— **PostgreSQL 的 text/varchar 类型不允许存储 NUL 字节**。
//   也就是说，单元测试永远发现不了它：Go 侧完全合法，是 PG 侧拒绝。
//   这类「语言合法但存储格式非法」的坑，只有真库集成测试能抓。
//
// 改用 US（Unit Separator, U+001F）：ASCII 控制字符，在 PG text 中合法，
// 且**不会出现在任何正常的业务键**（订单号/SKU/店铺 id）里，
// 因此仍能满足「防拼接歧义」的要求（见 TestIdemKey_AvoidsAmbiguousConcatenation）。
const idemSeparator = "\x1f"

// IdemKey 构造幂等键。
//
// 为什么不用自增或时间戳：幂等键的作用是「同一上游实体重复到达时判定为同一行」。
// 因此它必须由**业务标识**派生，而非由到达顺序派生。
//
// 口径：jobID + SEP + naturalKey（上游的行标识）。
//   - 同一 job 重复拉到同一行 ⇒ 同键 ⇒ UNIQUE 冲突 ⇒ 不产生第二行。
//   - 不同 job（如重采同一月份的新 jobID）会被视为不同批次，这是有意的：
//     重采要能覆盖/对照，而不是被旧批次静默吞掉。
func IdemKey(jobID, naturalKey string) string {
	return jobID + idemSeparator + naturalKey
}

// ───────────────────────── 完整性守卫 ─────────────────────────

// GuardInput 守卫判定的输入（全部来自 staging 的实测，不含推测）。
type GuardInput struct {
	ExpectedPages int    // 上游声明的总页数；0 = 上游未声明（无法判定）
	ActualPages   int    // staging 实际取到的页数
	ExpectedRows  int64  // 上游声明的总行数；0 = 未声明
	ActualRows    int64  // staging 实际行数
	CursorEnded   bool   // 游标是否已到底
	RequiredOK    bool   // 必填字段是否无缺失
	Coverage      float64 // 字段覆盖率 0..1；负值 = 无法计算
	HasCoverage   bool
}

// GuardVerdict 守卫结论。
type GuardVerdict struct {
	Passed  bool
	Reasons []string // 未通过的原因（空 = 通过）
}

// CoverageFloor 覆盖率下限（与 docs/01 §14 KODP 覆盖率门控 cost coverage ≥ 80% 同口径）。
const CoverageFloor = 0.80

// Evaluate 做完整性判定（**fail-closed**）。
//
// 判定纪律：
//   - 任何一项**无法判定**（上游未声明页数/行数、覆盖率算不出）都算**不通过**。
//     「无法确认完整」≠「完整」。这是用户要求「守卫验收数据完整性后过闸」的
//     核心：宁可停 staging 等补齐，也不放行可能残缺的数据进投影层。
//   - 通过必须**全部**子项成立，不存在「部分通过」。
//
// ★ 返回的 Reasons 保证**非 nil**（通过时是空切片 []string{}）。
//
//   真实事故（真库抓出，单测抓不到）：
//     表定义 `reasons text[] NOT NULL DEFAULT '{}'`，看着有默认值很安全；
//     但 **DEFAULT 只在「不提供该列」时生效**，显式传 NULL 直接撞 NOT NULL：
//       ERROR: null value in column "reasons" ... (SQLSTATE 23502)
//     Go 的 nil []string 经 pgx 正是编码成 SQL NULL。而这个 bug
//     **只在守卫通过时触发**（通过时无原因=nil，不通过时至少一条=非 nil）——
//     专炸在「数据终于补齐、可以放行」的成功分支上。
//
//   修在**这里**而不是各调用点：让不变式由生产者保证，
//   这样任何未来的调用者都不会再踩。在调用点逐个兜底只会不断漏。
//
// 这条函数一旦出 bug，后果是残缺数据静默进入经营报表 —— 属于最高危缺陷，
// 因此它被 TestEvaluate_* 系列（含负向用例）密集覆盖。
func Evaluate(in GuardInput) GuardVerdict {
	// 用非 nil 空切片起步：保证任何返回路径都不产生 nil
	reasons := []string{}

	// 1) 页数闭合：上游声明了总页数，就必须取满
	if in.ExpectedPages <= 0 {
		reasons = append(reasons, "上游未声明总页数，无法判定是否取完（不得据『看起来取完了』放行）")
	} else if in.ActualPages < in.ExpectedPages {
		reasons = append(reasons, fmt.Sprintf("页数未闭合：声明 %d 页，实取 %d 页",
			in.ExpectedPages, in.ActualPages))
	}

	// 2) 游标到底：不是「取满页数」就够，必须上游明确表示没有下一页
	if !in.CursorEnded {
		reasons = append(reasons, "游标未到底：上游仍有下一页，本次采集未走完")
	}

	// 3) 行数对账：上游给了 total 就必须一致
	if in.ExpectedRows > 0 && in.ActualRows != in.ExpectedRows {
		reasons = append(reasons, fmt.Sprintf("行数不符：声明 %d 行，实际 %d 行",
			in.ExpectedRows, in.ActualRows))
	}

	// 4) 必填字段
	if !in.RequiredOK {
		reasons = append(reasons, "必填字段存在缺失")
	}

	// 5) 覆盖率
	if !in.HasCoverage {
		reasons = append(reasons, "字段覆盖率无法计算，不得放行")
	} else if in.Coverage < CoverageFloor {
		reasons = append(reasons, fmt.Sprintf("覆盖率 %.2f%% 低于下限 %.2f%%",
			in.Coverage*100, CoverageFloor*100))
	}

	return GuardVerdict{Passed: len(reasons) == 0, Reasons: reasons}
}
