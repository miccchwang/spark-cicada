// Package strategy —— M-STRATEGY（策略实验室）的纯逻辑层。
//
// 职责（docs/01 §7）：
//
//  1. 把「一次决策」建模为**受限的选择**（选型卡），而不是任意输入；
//  2. 校验用户的决策动作是否合法（选项必须来自本卡的候选集）；
//  3. 维护策略历史（append-only），并支持**回滚到上一个稳定版本**；
//  4. 在变更落定后给出「需要重算哪些预计算桶」的清单。
//
// ══════════════════════════════════════════════════════════════════════════
// ★★ 本模块的第一纪律：**永远不接受用户输入的公式或任意数字。**
//
//	需求原文是「以选型方式提供」。一旦开放「填公式」入口，就等于把
//	算法内核的边界打开：用户可以写出 `net_revenue = gross_listing` 这类
//	看似无害、实则让所有毛利归零的表达式，而且系统无法预知影响面
//	（impactPreview 直接失效）。因此本层的公开 API **没有任何**
//	接收「自由表达式」的入口 —— 决策只能是 `choose(optionKey)` 或
//	`keep_current`。想加新方案的人，必须先在 Propose 阶段把它变成一张卡。
//
// ★ 第二纪律：**「保持现状」必须是一等公民，且必须留痕。**
//
//	很多系统只记录「改了什么」，于是「评估了但决定不改」这段推理就丢了；
//	下次同类信号再出现时，没人知道上次为什么不改，只能重新评估一遍。
//	本层把 keep_current 也落一条历史（from == to），让「决策过」可被审计。
//
// ★ 第三纪律：**回滚不是「删记录」，而是「再记一条」。**
//
//	历史是 append-only（KODP 的教训：口径变更不追溯，导致无法解释
//	「为什么这个数字变了」）。回滚 = 把值改回上一稳定版本 + 追加一条
//	to = 稳定版本的记录。若回滚靠 UPDATE/DELETE 历史行，那么
//	「曾经激进过又退回来」这段事实就被抹掉了，而它恰恰是最有价值的信号。
//
// ★ 第四纪律：**变更必须能算清影响面。**
//
//	任何参数改动都会让基于旧参数的预计算桶失效。若不同步标记 STALE，
//	报表会继续用旧参数的数字回答新口径的问题 —— 这是最典型的一种
//	「数字对不上但没人报错」。所以 Apply 的返回值里**必须**带
//	affectedBuckets，且调用方不得忽略它。
//
// ══════════════════════════════════════════════════════════════════════════
package strategy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Version 策略契约版本（与 contracts/strategy-choice.ts 对齐）。
const Version = "1.0"

// 常见错误：调用方可据此区分「参数非法」「越权决策」「无稳定版本可回滚」。
var (
	// ErrInvalidChoice 选型卡本身不合法（选项数越界、current 不在 options 里等）。
	ErrInvalidChoice = errors.New("strategy: 选型卡不合法")
	// ErrUnknownOption 用户选了本卡候选集之外的选项。
	//
	// ★ 这是**越权/脏数据**信号，不是「参数写错」：说明前端拿到了
	//   过期或被篡改的候选列表。必须硬拒绝，绝不能「宽容地」接受。
	ErrUnknownOption = errors.New("strategy: 所选选项不在候选集内")
	// ErrNoStableVersion 历史里找不到可回滚的稳定版本。
	ErrNoStableVersion = errors.New("strategy: 无可回滚的稳定版本")
	// ErrIrreversible 该变更被标记为不可逆，禁止回滚。
	ErrIrreversible = errors.New("strategy: 该变更不可回滚")
)

// ───────────────────────────── 选项 ─────────────────────────────

// Risk 风险等级（与契约的联合类型逐字对应）。
type Risk string

// 三档风险。刻意与契约字符串一致，避免一层映射。
const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

// Valid 判断风险等级是否合法（未知等级一律视为非法，不做默认兜底）。
func (r Risk) Valid() bool {
	return r == RiskLow || r == RiskMedium || r == RiskHigh
}

// Option 一个候选选项（契约 ChoiceOption 的服务端投影）。
//
// 字段与契约一一对应；刻意不复用 web 的命名风格，但保持同名。
type Option struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// ExpectedEffect 预期影响的人话描述，如「净利率 34.45% → 36.14%」。
	//
	// ★ 必须是**已算好的字符串**，不是让前端自己拼的模板：
	//   若让前端拼，同一个变化在不同页面会出现不同措辞，
	//   用户会以为看到了两个不同的结论。
	ExpectedEffect string `json:"expectedEffect"`
	Risk           Risk   `json:"risk"`
	// Reversible 是否可回滚。
	//
	// ★ 这是**选项自己的属性**，不是全卡共享的：同一张卡里
	//   「小幅放宽阈值」可回滚，而「切换统计口径」可能不可回滚
	//   （历史数据已按新口径重算，回不去）。
	Reversible bool `json:"reversible"`
	// Recommended 是否被系统推荐（采纳学习的输出）。
	Recommended bool `json:"recommended,omitempty"`
}

// ───────────────────────────── 影响预览 ─────────────────────────────

// ImpactPreview 每个选项的预期影响。
type ImpactPreview struct {
	// Metrics 受影响的指标列表（顺序即展示顺序）。
	Metrics []string `json:"metrics"`
	// Delta optionKey → metricKey → 变化描述。
	Delta map[string]map[string]string `json:"delta"`
	// AffectedBuckets 选定后需重算的预计算桶。
	//
	// ★ 放在预览里（而不是 Apply 后才知道）是有意的：
	//   用户决策前就该知道「这次改动会让多少东西过期」。
	//   若只在 Apply 后返回，用户已经点下去了才知道要重算全月数据 ——
	//   那就变成了「先斩后奏」。
	AffectedBuckets []string `json:"affectedBuckets"`
}

// Choice 一张选型卡（契约 StrategyChoice 的服务端投影）。
type Choice struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Context string `json:"context"`
	// CurrentKey 当前生效选项的 key。
	//
	// ★ 只存 key 而不存整个 Option：当前选项一定是 options 里的一员，
	//   存两份必然漂移（改了 options 忘了改 current）。Current() 由 key 推出。
	CurrentKey string        `json:"currentKey"`
	Options    []Option      `json:"options"`
	Impact     ImpactPreview `json:"impactPreview"`
	Blocking   bool          `json:"blocking"`
	DecideBy   string        `json:"decideBy,omitempty"`
}

// Current 返回当前生效的选项；找不到时返回 nil（由 Validate 先行拦下）。
func (c *Choice) Current() *Option {
	if c == nil {
		return nil
	}
	for i := range c.Options {
		if c.Options[i].Key == c.CurrentKey {
			return &c.Options[i]
		}
	}
	return nil
}

// Option 按 key 查候选选项。
func (c *Choice) Option(key string) (*Option, bool) {
	if c == nil {
		return nil, false
	}
	for i := range c.Options {
		if c.Options[i].Key == key {
			return &c.Options[i], true
		}
	}
	return nil, false
}

// MinOptions / MaxOptions 候选数量的上下界（契约：2–4 个，含 current）。
//
// ★ 下界 2 而不是 1：只有 1 个选项的「决策」是伪决策，
//
//	它会让用户以为自己做了选择，实际没有备选。这种卡应该根本不出。
//
// ★ 上界 4：超过 4 个选项时人无法认真比较，会退化成「随便点一个」，
//
//	反而是决策质量的下降。真要给更多方案，应先在 Propose 阶段收敛。
const (
	MinOptions = 2
	MaxOptions = 4
)

// Validate 校验一张选型卡是否合法。返回问题列表（空 = 合法）。
//
// 只校验**结构与候选集自洽**，不校验业务语义（那是 Propose 生成方的事）。
func (c *Choice) Validate() []string {
	if c == nil {
		return []string{"选型卡为空"}
	}
	var problems []string
	if strings.TrimSpace(c.ID) == "" {
		problems = append(problems, "id 不能为空")
	}
	if strings.TrimSpace(c.Title) == "" {
		problems = append(problems, "title 不能为空（用户靠标题判断在决策什么）")
	}
	if strings.TrimSpace(c.Context) == "" {
		// 缺 context 的卡等于「不知道为什么要选」，用户只能凭感觉点。
		problems = append(problems, "context 不能为空（必须说明为什么要决策）")
	}
	n := len(c.Options)
	if n < MinOptions || n > MaxOptions {
		problems = append(problems, fmt.Sprintf(
			"候选选项数 %d 越界（应为 %d–%d 个，含当前值）", n, MinOptions, MaxOptions))
	}
	seen := map[string]bool{}
	for i, o := range c.Options {
		where := fmt.Sprintf("选项[%d]", i)
		if strings.TrimSpace(o.Key) == "" {
			problems = append(problems, where+" 的 key 不能为空")
		} else if seen[o.Key] {
			// 重复 key 会让 delta / history 无法定位到唯一选项
			problems = append(problems, fmt.Sprintf("选项 key %q 重复", o.Key))
		}
		seen[o.Key] = true
		if strings.TrimSpace(o.Label) == "" {
			problems = append(problems, where+" 的 label 不能为空")
		}
		if strings.TrimSpace(o.ExpectedEffect) == "" {
			// 契约要求每个选项都给出预期影响 —— 没有影响描述的选项
			// 会让用户只能靠「哪个词听起来好」来选，这是选型交互的失败。
			problems = append(problems, where+" 缺 expectedEffect（每个选项都必须给出预期影响）")
		}
		if !o.Risk.Valid() {
			problems = append(problems, fmt.Sprintf("%s 的 risk %q 非法（应为 low/medium/high）", where, o.Risk))
		}
	}
	if strings.TrimSpace(c.CurrentKey) == "" {
		problems = append(problems, "currentKey 不能为空")
	} else if !seen[c.CurrentKey] {
		// ★ 当前值必须在候选集里：否则「保持现状」指向一个不在列表里的选项，
		//   前端渲染不出选中态，用户会以为当前值丢了。
		problems = append(problems, fmt.Sprintf("currentKey %q 不在候选集内", c.CurrentKey))
	}
	// delta 的键必须是本卡的候选 key（多出来的键说明预览与卡片不同步）
	for k := range c.Impact.Delta {
		if !seen[k] {
			problems = append(problems, fmt.Sprintf("impactPreview.delta 含未知选项 %q", k))
		}
	}
	return problems
}

// ───────────────────────────── 决策动作 ─────────────────────────────

// ActionKind 用户可执行的决策动作种类。
type ActionKind string

// 只有这三种（契约 DecisionAction 的两种形态，加上服务端的「非法」）。
const (
	ActionChoose      ActionKind = "choose"
	ActionKeepCurrent ActionKind = "keep_current"
)

// Action 一次决策动作。
//
// ★ 结构上就**没有**「写公式」的位置 —— 这不是靠校验堵住，而是靠类型不给。
type Action struct {
	Kind ActionKind `json:"kind"`
	// OptionKey 仅当 kind == choose 时有意义。
	OptionKey string `json:"optionKey,omitempty"`
}

// ParseAction 解析并校验动作类型（未知 kind 一律拒绝，不做默认兜底）。
func ParseAction(kind string) (ActionKind, error) {
	switch ActionKind(kind) {
	case ActionChoose:
		return ActionChoose, nil
	case ActionKeepCurrent:
		return ActionKeepCurrent, nil
	default:
		return "", fmt.Errorf("strategy: 未知动作 %q（只允许 choose / keep_current）", kind)
	}
}

// Resolve 把一次决策动作解析为「目标选项 key」。
//
// 语义：
//
//	choose(x)       → x，且 x 必须在本卡候选集内
//	keep_current    → 当前 key（即「不改」）
//
// ★ keep_current 一律解析为**当前 key**，而不是某个特殊常量。
// 这样它在历史里就是一条 from == to 的普通记录，
// 下游（评估 / 统计「决策过但没改」）无需特殊分支。
func (c *Choice) Resolve(a Action) (string, error) {
	if problems := c.Validate(); len(problems) > 0 {
		return "", fmt.Errorf("%w: %s", ErrInvalidChoice, strings.Join(problems, "; "))
	}
	switch a.Kind {
	case ActionKeepCurrent:
		return c.CurrentKey, nil
	case ActionChoose:
		if strings.TrimSpace(a.OptionKey) == "" {
			return "", fmt.Errorf("%w: choose 动作必须给出 optionKey", ErrUnknownOption)
		}
		if _, ok := c.Option(a.OptionKey); !ok {
			// 不回显候选集：调用方已有卡片，回显只会让错误信息变长。
			return "", fmt.Errorf("%w: %q", ErrUnknownOption, a.OptionKey)
		}
		return a.OptionKey, nil
	default:
		return "", fmt.Errorf("strategy: 未知动作 %q", a.Kind)
	}
}

// IsChange 判断解析结果相对当前值是否构成**实质变更**。
//
// ★ 显式区分「决策了但没改」与「改了」：前者也要留痕（见包注释第二纪律），
// 但不应触发预计算重算 —— 否则每次「保持现状」都会让全月数据变 STALE，
// 用户会看到「什么都没改，报表却全在重算」。
func (c *Choice) IsChange(target string) bool {
	return c != nil && target != c.CurrentKey
}

// ───────────────────────────── 策略历史 ─────────────────────────────

// HistoryEntry 一条策略变更记录（契约 StrategyHistoryEntry 的服务端投影）。
//
// 对应 docs/01 §7.5 要求的七要素：
// 策略 ID / 旧值 / 新值 / 决策人 / 时间 / 决策依据(快照哈希) / 实际效果。
type HistoryEntry struct {
	ID         string    `json:"id"`
	ChoiceID   string    `json:"choiceId"`
	FromOption string    `json:"fromOption"`
	ToOption   string    `json:"toOption"`
	DecidedBy  string    `json:"decidedBy"`
	DecidedAt  time.Time `json:"decidedAt"`
	// SnapshotHash 决策当时的数据快照哈希。
	//
	// ★ 这是「可解释」的关键：日后有人问「为什么 3 月的净利率变了」，
	//   能顺着 hash 找到当时依据的那份数据，而不是只能说「有人改过参数」。
	SnapshotHash string `json:"dataSnapshotHash"`
	// ActualEffect 事后评估的实际效果（异步回填，可空）。
	ActualEffect string `json:"actualEffect,omitempty"`
	// Kind 记录这是实质变更还是「保持现状」。
	//
	// ★ 单独一个字段而不是用 from==to 推断：显式可读，
	//   且避免「恰好改回了原值」被误判为 keep_current。
	Kind ActionKind `json:"kind"`
	// Reversible 当时所选选项是否可回滚（回滚守卫的依据）。
	Reversible bool `json:"reversible"`
	// AffectedBuckets 本次变更需要重算的桶（保持现状时为空）。
	AffectedBuckets []string `json:"affectedBuckets,omitempty"`
}

// IsChange 该记录是否为实质变更。
func (h HistoryEntry) IsChange() bool { return h.Kind == ActionChoose && h.FromOption != h.ToOption }

// History 某张卡的策略历史（append-only，按时间正序）。
//
// ★ 用切片而不是 map：历史天然有序，且「最近一次稳定版本」
//
//	这个查询依赖顺序。存 map 会在序列化后丢序。
type History []HistoryEntry

// Append 追加一条记录，返回新切片（不改入参）。
//
// ★ 为什么强制「返回新切片」：调用方若持有旧引用并原地 append，
// 两个调用方会共享底层数组，出现「我追加的记录出现在他那里」的串味。
func (h History) Append(e HistoryEntry) History {
	out := make(History, 0, len(h)+1)
	out = append(out, h...)
	out = append(out, e)
	return out
}

// LastStable 找到最近一次**可回滚的稳定版本**，即应回滚到的目标选项。
//
// 语义（这是本模块最容易写错的地方，逐条说明）：
//
//	从历史里自后向前找第一条「实质变更 且 结果选项可回滚」的记录，
//	返回它的 **FromOption** —— 也就是「那次变更之前的值」。
//
// ★ 为什么不返回 ToOption：
//
//	回滚的语义是「撤销上一次变更」。若返回 ToOption，回滚就成了
//	「再执行一次同样的变更」（幂等空操作），用户点了回滚却发现没变。
//
// ★ 为什么要跳过「不可回滚」的记录：
//
//	  如果最近一次变更本身被标记不可逆（例如口径切换、历史数据已重算），
//	  那么它之前的那个值已无法通过「改参数」回去 —— 强行回滚会得到一个
//	  参数值与数据值不一致的错位状态。此时应**继续往前**找，
//	  或者干脆报 ErrNoStableVersion 让用户知道「这里回不去了」。
//
//		但注意：这里选择「继续往前找」是有争议的。判断依据是
//		「不可回的变更」是否真的**污染**了更早的值。当前实现取保守策略：
//		**遇到不可回滚的变更即停止**，不越过它去找更早的值 ——
//		因为越过它回滚，等于把一个基于「已被重算过的数据」的旧参数
//		重新贴上，那份数据的口径已经不是当时的了。
func (h History) LastStable() (string, bool) {
	for i := len(h) - 1; i >= 0; i-- {
		e := h[i]
		if !e.IsChange() {
			continue // 保持现状不是可回滚的「版本」
		}
		if !e.Reversible {
			// 最近一次实质变更不可逆 ⇒ 停在这里，不再往前。
			// 用户需要的是「知道回不去」，而不是一个更早的、口径已对不上的值。
			return "", false
		}
		return e.FromOption, true
	}
	return "", false
}

// RollbackPlan 一次回滚的执行计划。
type RollbackPlan struct {
	ChoiceID string `json:"choiceId"`
	// FromOption 当前值。
	FromOption string `json:"fromOption"`
	// ToOption 回滚目标（= 上一稳定版本的值）。
	ToOption string `json:"toOption"`
	// UndoOfEntryID 被撤销的那条历史记录 id。
	UndoOfEntryID string `json:"undoOfEntryId"`
	// AffectedBuckets 需要重算的桶（回滚同样要重算！）。
	AffectedBuckets []string `json:"affectedBuckets"`
	// Note 给用户看的一句说明。
	Note string `json:"note"`
}

// PlanRollback 生成回滚计划。
//
// ★ 回滚**同样**需要重算预计算桶：这是最容易漏的一步。
//
//	很多人以为「回滚 = 恢复原状 = 数据本来就在」，但预计算桶里
//	存的是**用变更后参数算出来的**数；只改回参数而不重算，
//	报表会拿旧参数去回答已按新参数算好的桶 —— 数字错得毫无征兆。
func PlanRollback(c *Choice, h History) (RollbackPlan, error) {
	if c == nil {
		return RollbackPlan{}, ErrInvalidChoice
	}
	target, ok := h.LastStable()
	if !ok {
		return RollbackPlan{}, fmt.Errorf("%w: 选型卡 %s", ErrNoStableVersion, c.ID)
	}
	cur := c.CurrentKey
	if target == cur {
		// 已经是稳定版本了：明确回报而不是「成功地什么都没做」。
		return RollbackPlan{}, fmt.Errorf("%w: 当前已是可回滚的稳定版本 %q", ErrNoStableVersion, cur)
	}
	// 找到被撤销的那条记录，便于事后对齐「回滚了哪一次变更」。
	undoID := ""
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].IsChange() && h[i].ToOption == cur {
			undoID = h[i].ID
			break
		}
	}
	return RollbackPlan{
		ChoiceID:        c.ID,
		FromOption:      cur,
		ToOption:        target,
		UndoOfEntryID:   undoID,
		AffectedBuckets: append([]string(nil), c.Impact.AffectedBuckets...),
		Note:            fmt.Sprintf("将「%s」从 %s 回滚到上一稳定版本 %s", c.Title, cur, target),
	}, nil
}

// ───────────────────────────── 采纳学习 ─────────────────────────────

// Preference 用户的决策偏好风格（采纳学习的输出）。
type Preference struct {
	// Account 该偏好的归属账号。
	Account string `json:"account"`
	// RiskAppetite 风险偏好得分：-1（极保守）… +1（极激进）。
	//
	// ★ 用连续分值而不是「保守/激进」二元标签：二元标签在
	//   边界上会来回横跳（选了 3 次 low 又选了 3 次 medium，标签反复变），
	//   用户会觉得系统在乱猜。分值 + 平滑更稳。
	RiskAppetite float64 `json:"riskAppetite"`
	// Samples 参与计算的样本数（供前端显示「依据不足」）。
	Samples int `json:"samples"`
	// Note 人话说明，如「依据 3 次决策，暂不做倾向性推荐」。
	Note string `json:"note"`
}

// MinSamplesForRecommendation 低于此样本数时不给出倾向性推荐。
//
// ★ 有意的保守：样本太少时的「学习」本质是过拟合 —
//
//	用户上一次手滑点了 high，系统就开始推荐激进方案，
//	用户会觉得「这系统疯了」。宁可先显示「依据不足」。
const MinSamplesForRecommendation = 3

// riskScore 把风险等级折算为分值（low=-1, medium=0, high=+1）。
func riskScore(r Risk) float64 {
	switch r {
	case RiskLow:
		return -1
	case RiskHigh:
		return 1
	default:
		return 0
	}
}

// LearnPreference 从历史里学习用户的风险偏好。
//
// 只统计**实质变更**（choose 且真的改了值）：
//
//	「保持现状」反映的是「对当前方案满意」，不是风险偏好信号。
//	若把它计入，一个从不改动的保守用户会因为「一直保持现状」
//	而被算出中性分 —— 那与他实际行为不符。
func LearnPreference(account string, h History, optionOf func(choiceID, optionKey string) (Risk, bool)) Preference {
	var sum float64
	var n int
	for _, e := range h {
		if e.DecidedBy != account || !e.IsChange() {
			continue
		}
		if optionOf == nil {
			continue
		}
		r, ok := optionOf(e.ChoiceID, e.ToOption)
		if !ok {
			continue
		}
		sum += riskScore(r)
		n++
	}
	p := Preference{Account: account, Samples: n}
	if n == 0 {
		p.Note = "暂无决策样本"
		return p
	}
	p.RiskAppetite = sum / float64(n)
	switch {
	case n < MinSamplesForRecommendation:
		p.Note = fmt.Sprintf("依据 %d 次决策，暂不做倾向性推荐（需 ≥%d 次）", n, MinSamplesForRecommendation)
	case p.RiskAppetite <= -0.34:
		p.Note = fmt.Sprintf("依据 %d 次决策：偏保守（倾向低风险方案）", n)
	case p.RiskAppetite >= 0.34:
		p.Note = fmt.Sprintf("依据 %d 次决策：偏激进（可接受较高风险方案）", n)
	default:
		p.Note = fmt.Sprintf("依据 %d 次决策：风险偏好中性", n)
	}
	return p
}

// ───────────────────────────── 审计动作名 ─────────────────────────────

// 策略实验室的审计动作名。
//
// ★ 集中定义而不是散在 handler 里写字符串字面量：
//
//	审计是事后追责的唯一依据，动作名写错一个字母就等于这类事件
//	在审计里「消失」了（查不到）。
const (
	// AuditActionDecide 一次策略决策（含保持现状）。
	//
	// ★ 这条记录由 store 写在**业务事务里**，是策略历史的**唯一来源**：
	//   它和「更新 current_key」「标记桶 STALE」同进退，
	//   要么全成、要么全不成，因此不会出现「历史说改了、账上没改」。
	AuditActionDecide = "strategy.decide"
	// AuditActionRollback 一次策略回滚。
	//
	// ★ 同样由 store 写在业务事务里，是策略历史的唯一来源。
	AuditActionRollback = "strategy.rollback"

	// AuditActionDecideAttempt 接口层对一次决策调用的**结果留痕**。
	//
	// ★ 为什么必须与 AuditActionDecide 分开命名（这不是洁癖，是一个真实故障）：
	//
	//	接口层原先也写一条 action = "strategy.decide" 的记录，用途是
	//	「如实回报这次调用有没有留痕（audited/warning）」。但它与 store 写的历史
	//	**同名**，而 History() 正是按 action 过滤的 —— 于是历史里每次决策都会
	//	多出一条「影子记录」。
	//
	//	更要命的是影子记录**不带 reversible 字段**：History() 读出时
	//	COALESCE(reversible, false) 把影子记录判成「不可逆」，
	//	而 LastStable() 遇到第一条不可逆变更就停下。
	//	结果：**任何一次决策之后，回滚永久不可达**（rollback/plan 恒返回 409），
	//	与 docs/01 §7.4「任何策略变更可一键回滚」直接冲突。
	//
	//	⇒ 接口层留痕改用独立动作名（本常量），History() 只看上面两条，
	//	  两条用途从此在**数据层**真正分离，而不是只靠注释声明分离。
	AuditActionDecideAttempt = "strategy.decide.attempt"
	// AuditActionRollbackAttempt 接口层对一次回滚调用的结果留痕。
	// 与 AuditActionDecideAttempt 同理，避免污染策略历史。
	AuditActionRollbackAttempt = "strategy.rollback.attempt"
)

// ───────────────────────────── 排序帮手 ─────────────────────────────

// SortHistoryAsc 按时间正序返回新切片；时间相同时按 id 兜底，保证确定。
//
// ★ 必须确定：历史列表若不稳定排序，同一份数据两次查询顺序不同，
//
//	用户会以为「记录被改了」。
func SortHistoryAsc(h History) History {
	out := make(History, len(h))
	copy(out, h)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].DecidedAt.Equal(out[j].DecidedAt) {
			return out[i].DecidedAt.Before(out[j].DecidedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// SortOptions 把选项按「推荐优先 → 风险升序 → key」排序，保证确定。
//
// ★ 不改变 Options 的**存储顺序**（那只在 Validate/渲染时用），
//
//	本函数只用于给前端提供一份稳定的展示顺序。
func SortOptions(opts []Option) []Option {
	out := make([]Option, len(opts))
	copy(out, opts)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Recommended != out[j].Recommended {
			return out[i].Recommended
		}
		ri, rj := riskRank(out[i].Risk), riskRank(out[j].Risk)
		if ri != rj {
			return ri < rj
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func riskRank(r Risk) int {
	switch r {
	case RiskLow:
		return 0
	case RiskMedium:
		return 1
	case RiskHigh:
		return 2
	}
	return 3
}
