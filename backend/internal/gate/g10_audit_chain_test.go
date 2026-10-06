package gate_test

// G10 · 审计 append-only 闭环 ——**链路级**闸门。
//
// ★ 为什么单列一个文件（第六个变种，还是同一个病）：
//
//	`gate.CheckAuditAppendOnly` **全仓没有任何非测试调用点** ——
//	唯一「调用」它的就是 `gate_test.go` 里那两条断言。
//	而 `internal/audit` 是一个**空目录**，全仓唯一写审计的地方是
//	`store.Admin.InsertAudit` 里的一句裸 INSERT。
//
//	于是「尝试修改/删除审计行必须失败」（docs/05 G10）在 Go 侧**恒真**：
//	没有任何生产代码会带着 UPDATE/DELETE 走到闸门面前。
//	（数据库侧的 0001 触发器是真的，但那是兜底，不是「Go 侧有闸门」。）
//
// 本文件补的是**链路断言**：从 internal/audit 与 internal/dr 的真实入口进，
// 以「业务结果」出 —— 闸门若被摘掉/被改坏，链路行为立刻变化，断言即失败。
//
// ★ 诚实说明：写路径（INSERT）上闸门**恒放行**，它的价值是把「非 INSERT」
// 这一侧变成一条**可被拒绝的调用**（见 Mutate），而不是在写路径上制造拒绝。
// 因此本文件的强断言落在 Mutate 与回滚排除清单上，而不是写路径。
//
// docs/01 §M-AUDIT；docs/05 G10 / G12。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/audit"
	"github.com/miccchwang/spark-cicada/backend/internal/dr"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// TestG10_AuditWriteChain 从 audit.Log.Record 入口进，以「Sink 多一行」出。
func TestG10_AuditWriteChain(t *testing.T) {
	// 锚点：闸门本身必须会拒非 INSERT（防「把闸门改成恒 nil」的破坏）。
	if len(gate.CheckAuditAppendOnly("DELETE")) == 0 {
		t.Fatal("锚点失效：CheckAuditAppendOnly(DELETE) 应报违规")
	}
	if len(gate.CheckAuditAppendOnly("INSERT")) != 0 {
		t.Fatal("锚点失效：CheckAuditAppendOnly(INSERT) 应放行")
	}

	sink := audit.NewMemSink()
	log := audit.NewLog(sink)
	ctx := context.Background()

	if err := log.Record(ctx, audit.Entry{
		Actor: "t1", Action: "admin.module.mount", Target: "m.strategy",
		Detail: map[string]any{"module": "m.strategy"},
	}); err != nil {
		t.Fatalf("合法审计应落库：%v", err)
	}
	if n, _ := sink.Count(ctx); n != 1 {
		t.Fatalf("应落 1 行，实际 %d", n)
	}
	got := sink.Entries()[0]
	if got.Actor != "t1" || got.Action != "admin.module.mount" || got.Target != "m.strategy" {
		t.Fatalf("审计内容不符：%+v", got)
	}
	if got.At.IsZero() {
		t.Fatal("审计必须带时间戳")
	}

	// 脏记录不得落库（append-only 表进了就删不掉）。
	if err := log.Record(ctx, audit.Entry{Action: "x"}); !errors.Is(err, audit.ErrEmptyActor) {
		t.Fatalf("空 actor 应被拒：%v", err)
	}
	if n, _ := sink.Count(ctx); n != 1 {
		t.Fatalf("被拒记录不得落库，行数应仍为 1，实际 %d", n)
	}
}

// TestG10_MutationRejectedBeforeSink 证明「改/删审计」在**触达存储之前**被拒。
func TestG10_MutationRejectedBeforeSink(t *testing.T) {
	for _, op := range []audit.Op{audit.OpUpdate, audit.OpDelete, audit.OpTruncate} {
		t.Run(string(op), func(t *testing.T) {
			sink := audit.NewMemSink()
			log := audit.NewLog(sink)
			err := log.Mutate(context.Background(), op, audit.Entry{Actor: "t1", Action: "x"})
			if !errors.Is(err, audit.ErrNotAppendOnly) {
				t.Fatalf("op=%s 应被 append-only 拒绝：%v", op, err)
			}
			// 拒绝必须携带闸门给出的人类可读原因 —— 否则闸门被摘掉不可被检出。
			if !strings.Contains(err.Error(), "append-only") {
				t.Fatalf("拒绝应携带闸门原因：%v", err)
			}
			// ★ 下游存储根本不应被调用。
			if n, _ := sink.Count(context.Background()); n != 0 {
				t.Fatalf("op=%s 被拒时下游存储不应被调用，实际落了 %d 行", op, n)
			}
		})
	}
}

// TestG10_RollbackPlanMustExcludeAudit 从 dr.PlanRollback 入口进：
// 策略漏掉审计表 ⇒ 回滚计划必须被拒（「回滚不动审计」的**事前**防线）。
func TestG10_RollbackPlanMustExcludeAudit(t *testing.T) {
	// 锚点：排除清单检查本身必须会拒（防「把检查改成恒 nil」的破坏）。
	if err := audit.EnsureExcludedFromRollback([]string{"users"}); err == nil {
		t.Fatal("锚点失效：缺审计表的清单应被拒")
	}

	c := g12Cluster()
	req := dr.RollbackRequest{Region: c.Region, Operator: "t1", Tier: "T1",
		Confirmed: true, SnapshotTaken: true, Target: g12Now.AddDate(0, 0, -30)}

	// 默认策略含 audit_log ⇒ 通过，且返回清单里确实有审计表。
	excl, err := dr.PlanRollback(c, dr.DefaultRollbackPolicy(), req, g12Now)
	if err != nil {
		t.Fatalf("默认策略应通过：%v", err)
	}
	if !containsAuditTable(excl) {
		t.Fatalf("返回清单应含审计表：%v", excl)
	}

	// 配置漏掉 audit_log ⇒ 必须被拒（这是修复前会静默通过的那条）。
	bad := dr.DefaultRollbackPolicy()
	bad.ExcludedTables = []string{"users", "orders"}
	if _, err := dr.PlanRollback(c, bad, req, g12Now); !errors.Is(err, dr.ErrAuditNotExcluded) {
		t.Fatalf("排除清单缺审计表必须被拒：%v", err)
	}

	// 空清单同样被拒。
	bad.ExcludedTables = nil
	if _, err := dr.PlanRollback(c, bad, req, g12Now); !errors.Is(err, dr.ErrAuditNotExcluded) {
		t.Fatalf("空排除清单必须被拒：%v", err)
	}
}

func containsAuditTable(tables []string) bool {
	for _, tb := range tables {
		if audit.IsAuditTable(tb) {
			return true
		}
	}
	return false
}

// TestG10_RollbackAuditGuard 回滚后审计行数减少必须报错（G12「回滚不动审计」）。
func TestG10_RollbackAuditGuard(t *testing.T) {
	if len(gate.CheckRollbackKeepsAudit(10, 9)) == 0 {
		t.Fatal("锚点失效：审计行数减少应报违规")
	}
	log := audit.NewLog(audit.NewMemSink())
	if err := log.GuardRollback(100, 100); err != nil {
		t.Fatalf("行数不变应通过：%v", err)
	}
	if err := log.GuardRollback(100, 101); err != nil {
		t.Fatalf("行数增加应通过：%v", err)
	}
	if err := log.GuardRollback(100, 99); !errors.Is(err, audit.ErrRollbackTouchedAudit) {
		t.Fatalf("行数减少应被拒：%v", err)
	}
}

// TestG10_AuditTableNameMatchesTrigger 防「Go 常量与 DB 触发器指向不同表」的漂移。
//
// ★ 为什么要这一条：audit.Table 若与 0001 的触发器表名分叉，
//   「append-only」就只保护了另一张表，而 Go 侧一切断言仍全绿。
func TestG10_AuditTableNameMatchesTrigger(t *testing.T) {
	sql := readMigration(t, "0001_core.sql")
	// 触发器必须是 BEFORE UPDATE OR DELETE ON <audit.Table>。
	want := "BEFORE UPDATE OR DELETE ON " + audit.Table
	if !strings.Contains(sql, want) {
		t.Fatalf("0001 的 append-only 触发器未指向 audit.Table(%q)：缺 %q", audit.Table, want)
	}
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS "+audit.Table) {
		t.Fatalf("0001 未按 audit.Table(%q) 建表", audit.Table)
	}
}
