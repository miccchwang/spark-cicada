// Package audit —— M-AUDIT 审计模块：append-only 留痕的**真实生产链路**（G10）。
//
// 背景（第六个变种，还是同一个病）：
//
//	docs/01 把 M-AUDIT 列为一个模块（「审计模块 | append-only 留痕 | Go」），
//	但 `internal/audit` **是一个空目录**。全仓唯一写审计的地方是
//	`store.Admin.InsertAudit` 里的一句裸 `INSERT`；而
//	`gate.CheckAuditAppendOnly`（G10 的「审计 append-only」判定）
//	**全仓没有任何非测试调用点** —— 唯一「调用」它的就是它自己的测试。
//
//	于是「尝试修改/删除审计行必须失败」（docs/05 G10）在 Go 侧**恒真**：
//	根本没有任何生产代码会带着 UPDATE/DELETE 走到它面前。
//	（数据库侧的 0001 触发器 `trg_audit_log_no_update` 是真的，但那是**兜底**，
//	不是「Go 侧有闸门」——docs/05 的 G10 行写的是两者。）
//
// 本包把 G10 的链路真正接上（纯 Go，不依赖数据库）：
//
//	1. 写审计（Record）—— **唯一**的追加入口，每一步都过 `gate.CheckAuditAppendOnly`；
//	2. 变更尝试（Mutate）—— 唯一允许携带非 INSERT 动词的入口，非 INSERT
//	   在**触达 Sink 之前**就被拒（下游存储根本不会被调用）；
//	3. 回滚护栏（GuardRollback / EnsureExcludedFromRollback）—— 回滚**永不触碰**审计表。
//
// ★ 关键设计：审计的租户上下文在**构造期**绑定（与 templatestore / caliber_audit 同一纪律）。
//
//	Entry 不带 TenantID —— 参数可空 ⇒ 漏传不报错 ⇒ 静默越权。
//	绑在 Sink 上，「这条审计属于谁」在装配时就定了。
//
// ★ 为什么脏数据要在**入表前**拒掉（Validate）：
//
//	审计表是 append-only 的，进去就再也删不掉。
//	一条 actor 为空的审计 = 一条永远无法归责的记录 = 表里的永久噪声。
//	宁可拒写，也不写脏。
//
// docs/01 §M-AUDIT / §18（凭据防泄漏）；docs/05 G10 / G12。
package audit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// Table 是审计表的**唯一**规范名。
//
// ★ 单列常量而不是各处写字面量：回滚排除清单、SQL 静态闸门、DB 触发器
//   都指向同一张表；名字分叉一次，「永不回滚」就漏一处。
const Table = "audit_log"

// Op 是审计表上的操作动词。
type Op string

// 审计表上可能出现的操作。
//
// 只有 OpInsert 合法 —— 其余三个是「**必须被拒**」的那一侧。
const (
	OpInsert   Op = "INSERT"
	OpUpdate   Op = "UPDATE"
	OpDelete   Op = "DELETE"
	OpTruncate Op = "TRUNCATE"
)

// Entry 是一条审计记录。
//
// ★ 刻意**不含** TenantID：租户在构造 Sink 时绑定（见包注释）。
type Entry struct {
	// At 记录时间；零值 ⇒ 由 Sink 用数据库 now() 兜底。
	At time.Time
	// Actor 操作人（账号）。必填。
	Actor string
	// Action 动作标识（如 "admin.module.mount"）。必填。
	Action string
	// Target 作用对象（可为空 ⇒ 存 NULL）。
	Target string
	// Detail 结构化上下文。不得含密钥明文（docs/09）。
	Detail map[string]any
	// RequestID 请求追踪号（可为空）。
	RequestID string
	// Region 地域（分地域审计，G12；可为空）。
	Region string
}

// 审计校验错误。
var (
	// ErrEmptyActor 审计必须能归责。
	ErrEmptyActor = errors.New("audit: 审计必须记录操作人（actor 不得为空）")
	// ErrEmptyAction 审计必须能说明做了什么。
	ErrEmptyAction = errors.New("audit: 审计必须记录动作（action 不得为空）")
	// ErrSecretInDetail 审计明细里出现疑似凭据键名。
	ErrSecretInDetail = errors.New("audit: 审计明细疑似含凭据（docs/09）")
	// ErrNotAppendOnly 非 INSERT 操作被 append-only 铁律拒绝。
	ErrNotAppendOnly = errors.New("audit: 审计表 append-only，仅允许 INSERT")
	// ErrNoSink 未装配持久化后端。
	ErrNoSink = errors.New("audit: 未装配 Sink")
	// ErrRollbackTouchedAudit 回滚触动了审计表。
	ErrRollbackTouchedAudit = errors.New("audit: 回滚触动了审计表（append-only 被破坏）")
	// ErrAuditTableNotExcluded 回滚排除清单里没有审计表。
	ErrAuditTableNotExcluded = errors.New("audit: 回滚排除清单必须包含审计表")
)

// secretDetailKeys 是审计明细里**禁止出现**的键名（归一化后精确匹配）。
//
// ★ 为什么只在键名上做粗筛：审计表 append-only，一旦把密钥写进去就再也删不掉。
//   这里不做「值扫描」（会误伤业务数值），只拦明显是凭据的**字段名**。
//   归一化 = 小写 + 去掉 `_`/`-`，故 "api_key"/"apiKey"/"API-KEY" 命中同一个 "apikey"。
var secretDetailKeys = map[string]bool{
	"password":     true,
	"passwd":       true,
	"secret":       true,
	"clientsecret": true,
	"token":        true,
	"accesstoken":  true,
	"refreshtoken": true,
	"apikey":       true,
	"privatekey":   true,
	"credential":   true,
}

// normalizeKey 把明细键名归一化后比对。
func normalizeKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	k = strings.ReplaceAll(k, "_", "")
	k = strings.ReplaceAll(k, "-", "")
	return k
}

// Validate 校验一条审计记录是否可入 append-only 表。
func (e Entry) Validate() error {
	if strings.TrimSpace(e.Actor) == "" {
		return ErrEmptyActor
	}
	if strings.TrimSpace(e.Action) == "" {
		return ErrEmptyAction
	}
	// 键名排序后逐个检查，保证报错稳定（map 遍历顺序随机）。
	keys := make([]string, 0, len(e.Detail))
	for k := range e.Detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if secretDetailKeys[normalizeKey(k)] {
			return fmt.Errorf("%w：明细键 %q", ErrSecretInDetail, k)
		}
	}
	return nil
}

// Sink 是审计的持久化抽象（append-only）。
//
// ★ 接口刻意**只有**追加与计数两个方法：没有 Update / Delete，
//   所以「写审计」这条路径在类型层面就不可能改历史。
type Sink interface {
	// Append 追加一条审计（只增不改）。
	Append(ctx context.Context, e Entry) error
	// Count 返回当前可见的审计行数（供回滚护栏比对）。
	Count(ctx context.Context) (int, error)
}

// Log 是 M-AUDIT 的唯一入口。
type Log struct {
	sink Sink
}

// NewLog 构造审计入口。
func NewLog(s Sink) *Log { return &Log{sink: s} }

// guard 把一次操作动词交给 G10 闸门判定。
//
// ★ 这是 `gate.CheckAuditAppendOnly` 的**真实生产调用点**：
//   Record 与 Mutate 都从这里过 —— 生产写审计的每一步都在闸门之下。
func guard(op Op) error {
	if v := gate.CheckAuditAppendOnly(string(op)); len(v) > 0 {
		return fmt.Errorf("%w：%s", ErrNotAppendOnly, strings.Join(v, "；"))
	}
	return nil
}

// Record 追加一条审计（生产写路径）。
//
// 纪律（顺序不可换）：
//  1. 必须装配 Sink；
//  2. **先校验**再落库 —— 脏数据进 append-only 表就再也删不掉了；
//  3. 过 G10 append-only 闸门（此处 op 恒为 INSERT）；
//  4. 追加。
func (l *Log) Record(ctx context.Context, e Entry) error {
	if l == nil || l.sink == nil {
		return ErrNoSink
	}
	if err := e.Validate(); err != nil {
		return err
	}
	if err := guard(OpInsert); err != nil {
		return err
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	return l.sink.Append(ctx, e)
}

// Mutate 是**唯一**允许携带非 INSERT 动词的入口。
//
// ★ 存在的意义：把「有人想改/删审计」这件事变成一条**可被闸门拒绝**的调用，
//   而不是让它在生产里根本没有表达形式（没有表达形式 ⇒ 闸门恒真）。
//
// 非 INSERT 一律在**触达 Sink 之前**返回错误 —— 下游存储根本不会被调用。
// INSERT 则与 Record 同路。
func (l *Log) Mutate(ctx context.Context, op Op, e Entry) error {
	if l == nil || l.sink == nil {
		return ErrNoSink
	}
	if err := guard(op); err != nil {
		// ★ 被拒时不触碰 Sink：这是「下游存储根本不会被调用」的可断言点。
		return err
	}
	// 走到这里 ⇒ op == INSERT（闸门只放行 INSERT）。
	if err := e.Validate(); err != nil {
		return err
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	return l.sink.Append(ctx, e)
}

// Count 返回审计行数（供回滚前后比对）。
func (l *Log) Count(ctx context.Context) (int, error) {
	if l == nil || l.sink == nil {
		return 0, ErrNoSink
	}
	return l.sink.Count(ctx)
}

// GuardRollback 校验一次回滚没有带走审计行（G12「回滚不动审计」）。
//
// ★ 这是 `gate.CheckRollbackKeepsAudit` 的真实生产调用点。
func (l *Log) GuardRollback(before, after int) error {
	if v := gate.CheckRollbackKeepsAudit(before, after); len(v) > 0 {
		return fmt.Errorf("%w：%s", ErrRollbackTouchedAudit, v[0])
	}
	return nil
}

// IsAuditTable 判断一个表名是否指向审计表（大小写不敏感；容忍 schema 前缀）。
func IsAuditTable(name string) bool {
	n := strings.TrimSpace(strings.ToLower(name))
	if i := strings.LastIndex(n, "."); i >= 0 {
		n = n[i+1:]
	}
	n = strings.Trim(n, `"`)
	return n == Table
}

// EnsureExcludedFromRollback 断言回滚排除清单**包含**审计表。
//
// ★ 为什么这条检查必须有：`dr.PlanRollback` 接受**调用方传入**的 RollbackPolicy。
//
//	默认策略（DefaultRollbackPolicy）确实排除了 audit_log，
//	但策略可以从配置/环境加载 —— 一旦那份配置漏掉 audit_log，
//	回滚就会把审计表一起带走，而「回滚不动审计」在 Go 侧不会有人拦。
//	（这就是 F8 轮的教训：闸门测的是入参，而生产路径可以传一个「合法」的入参。）
func EnsureExcludedFromRollback(tables []string) error {
	for _, t := range tables {
		if IsAuditTable(t) {
			return nil
		}
	}
	return fmt.Errorf("%w：当前清单=%v", ErrAuditTableNotExcluded, tables)
}
