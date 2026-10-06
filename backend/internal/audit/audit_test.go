package audit_test

// M-AUDIT 单元测试（G10）。
//
// ★ 每个「被拒」用例都额外断言 **Sink 里一行都没有** ——
//   否则一个「先写库再判闸门」的错误实现也能通过（第四类缺陷：断言太弱）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/audit"
)

func validEntry() audit.Entry {
	return audit.Entry{
		Actor:  "lead.sea",
		Action: "admin.module.mount",
		Target: "m.strategy",
		Detail: map[string]any{"module": "m.strategy"},
	}
}

func count(t *testing.T, s *audit.MemSink) int {
	t.Helper()
	n, err := s.Count(context.Background())
	if err != nil {
		t.Fatalf("Count 失败：%v", err)
	}
	return n
}

// ───────────────────────────── 追加路径 ─────────────────────────────

func TestRecord_AppendsValidEntry(t *testing.T) {
	sink := audit.NewMemSink()
	log := audit.NewLog(sink)

	if err := log.Record(context.Background(), validEntry()); err != nil {
		t.Fatalf("合法审计应落库：%v", err)
	}
	if n := count(t, sink); n != 1 {
		t.Fatalf("应落 1 行，实际 %d", n)
	}
	got := sink.Entries()[0]
	if got.Actor != "lead.sea" || got.Action != "admin.module.mount" || got.Target != "m.strategy" {
		t.Fatalf("审计内容不符：%+v", got)
	}
	// Record 必须补时间戳（零值 ⇒ now）。
	if got.At.IsZero() {
		t.Fatal("Record 应自动补时间戳")
	}
}

func TestRecord_KeepsExplicitTime(t *testing.T) {
	sink := audit.NewMemSink()
	log := audit.NewLog(sink)
	at := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	e := validEntry()
	e.At = at
	if err := log.Record(context.Background(), e); err != nil {
		t.Fatalf("应通过：%v", err)
	}
	if !sink.Entries()[0].At.Equal(at) {
		t.Fatalf("显式时间不应被覆盖：%v", sink.Entries()[0].At)
	}
}

// ───────────────────────────── 校验：脏数据不入 append-only 表 ─────────────────────────────

func TestRecord_RejectsDirtyEntry(t *testing.T) {
	cases := []struct {
		name string
		e    audit.Entry
		want error
	}{
		{"空 actor", audit.Entry{Action: "a"}, audit.ErrEmptyActor},
		{"纯空白 actor", audit.Entry{Actor: "   ", Action: "a"}, audit.ErrEmptyActor},
		{"空 action", audit.Entry{Actor: "x"}, audit.ErrEmptyAction},
		{"纯空白 action", audit.Entry{Actor: "x", Action: "\t\n"}, audit.ErrEmptyAction},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := audit.NewMemSink()
			log := audit.NewLog(sink)
			err := log.Record(context.Background(), c.e)
			if !errors.Is(err, c.want) {
				t.Fatalf("应返回 %v，实际 %v", c.want, err)
			}
			// ★ 关键：被拒的记录绝不能落进 append-only 表（进了就再也删不掉）。
			if n := count(t, sink); n != 0 {
				t.Fatalf("被拒记录绝不能落库，实际落了 %d 行", n)
			}
		})
	}
}

// TestRecord_RejectsSecretKeys docs/09：审计表 append-only，密钥写进去就再也删不掉。
func TestRecord_RejectsSecretKeys(t *testing.T) {
	bad := []string{"password", "Passwd", "api_key", "apiKey", "API-KEY",
		"clientSecret", "ACCESS-TOKEN", "private-key", "credential"}
	for _, k := range bad {
		t.Run(k, func(t *testing.T) {
			sink := audit.NewMemSink()
			log := audit.NewLog(sink)
			e := validEntry()
			e.Detail = map[string]any{k: "whatever"}
			err := log.Record(context.Background(), e)
			if !errors.Is(err, audit.ErrSecretInDetail) {
				t.Fatalf("键 %q 应被拒：%v", k, err)
			}
			if n := count(t, sink); n != 0 {
				t.Fatalf("含凭据的记录绝不能落库，实际落了 %d 行", n)
			}
		})
	}
}

// TestRecord_AllowsInnocentKeys 反向：相似但合法的键名不得误伤（防过度拦截）。
func TestRecord_AllowsInnocentKeys(t *testing.T) {
	good := []string{"tokens", "tokenCount", "secretary", "key_id", "password_policy"}
	for _, k := range good {
		sink := audit.NewMemSink()
		log := audit.NewLog(sink)
		e := validEntry()
		e.Detail = map[string]any{k: 1}
		if err := log.Record(context.Background(), e); err != nil {
			t.Fatalf("键 %q 不应被误伤：%v", k, err)
		}
	}
}

// ───────────────────────────── 变更尝试：非 INSERT 一律在触达 Sink 前被拒 ─────────────────────────────

func TestMutate_RejectsNonInsertWithoutTouchingSink(t *testing.T) {
	ops := []audit.Op{audit.OpUpdate, audit.OpDelete, audit.OpTruncate, audit.Op("DROP"), audit.Op("ALTER")}
	for _, op := range ops {
		t.Run(string(op), func(t *testing.T) {
			sink := audit.NewMemSink()
			log := audit.NewLog(sink)
			err := log.Mutate(context.Background(), op, validEntry())
			if !errors.Is(err, audit.ErrNotAppendOnly) {
				t.Fatalf("op=%s 应被 append-only 拒绝：%v", op, err)
			}
			// ★ 拒绝必须携带闸门给出的人类可读原因 —— 否则「把闸门摘掉」不可被检出。
			if !strings.Contains(err.Error(), "append-only") {
				t.Fatalf("拒绝应携带闸门原因：%v", err)
			}
			if n := count(t, sink); n != 0 {
				t.Fatalf("op=%s 被拒时下游存储根本不应被调用，实际落了 %d 行", op, n)
			}
		})
	}
}

func TestMutate_InsertAppends(t *testing.T) {
	sink := audit.NewMemSink()
	log := audit.NewLog(sink)
	if err := log.Mutate(context.Background(), audit.OpInsert, validEntry()); err != nil {
		t.Fatalf("INSERT 应放行：%v", err)
	}
	if n := count(t, sink); n != 1 {
		t.Fatalf("INSERT 应落 1 行，实际 %d", n)
	}
}

func TestMutate_InsertStillValidates(t *testing.T) {
	sink := audit.NewMemSink()
	log := audit.NewLog(sink)
	if err := log.Mutate(context.Background(), audit.OpInsert, audit.Entry{}); !errors.Is(err, audit.ErrEmptyActor) {
		t.Fatalf("INSERT 也要过校验：%v", err)
	}
	if n := count(t, sink); n != 0 {
		t.Fatal("校验失败不得落库")
	}
}

// ───────────────────────────── 未装配 Sink ─────────────────────────────

func TestLog_NoSink(t *testing.T) {
	ctx := context.Background()

	var nilLog *audit.Log
	if err := nilLog.Record(ctx, validEntry()); !errors.Is(err, audit.ErrNoSink) {
		t.Fatalf("nil Log 应报未装配：%v", err)
	}

	empty := audit.NewLog(nil)
	if err := empty.Record(ctx, validEntry()); !errors.Is(err, audit.ErrNoSink) {
		t.Fatalf("nil Sink 应报未装配：%v", err)
	}
	if err := empty.Mutate(ctx, audit.OpInsert, validEntry()); !errors.Is(err, audit.ErrNoSink) {
		t.Fatalf("nil Sink Mutate 应报未装配：%v", err)
	}
	if _, err := empty.Count(ctx); !errors.Is(err, audit.ErrNoSink) {
		t.Fatalf("nil Sink Count 应报未装配：%v", err)
	}
}

// ───────────────────────────── 回滚护栏（G12） ─────────────────────────────

func TestGuardRollback(t *testing.T) {
	log := audit.NewLog(audit.NewMemSink())
	if err := log.GuardRollback(100, 100); err != nil {
		t.Fatalf("行数不变应通过：%v", err)
	}
	if err := log.GuardRollback(100, 101); err != nil {
		t.Fatalf("行数增加应通过：%v", err)
	}
	err := log.GuardRollback(100, 99)
	if !errors.Is(err, audit.ErrRollbackTouchedAudit) {
		t.Fatalf("行数减少应被拒：%v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("拒绝应携带闸门原因：%v", err)
	}
}

// ───────────────────────────── 回滚排除清单 ─────────────────────────────

func TestEnsureExcludedFromRollback(t *testing.T) {
	ok := [][]string{
		{"audit_log"},
		{"users", "audit_log"},
		{"public.audit_log"},
		{`"audit_log"`},
		{"AUDIT_LOG"},
	}
	for _, tables := range ok {
		if err := audit.EnsureExcludedFromRollback(tables); err != nil {
			t.Fatalf("清单 %v 应通过：%v", tables, err)
		}
	}
	bad := [][]string{
		nil,
		{},
		{"users", "orders"},
		{"audit"}, // 相似但不等
		{"audit_logs"},
	}
	for _, tables := range bad {
		if err := audit.EnsureExcludedFromRollback(tables); !errors.Is(err, audit.ErrAuditTableNotExcluded) {
			t.Fatalf("清单 %v 应被拒：%v", tables, err)
		}
	}
}

func TestIsAuditTable(t *testing.T) {
	yes := []string{"audit_log", "AUDIT_LOG", " Audit_Log ", "public.audit_log", `"audit_log"`}
	for _, s := range yes {
		if !audit.IsAuditTable(s) {
			t.Fatalf("%q 应被识别为审计表", s)
		}
	}
	no := []string{"audit", "audit_logs", "user_audit_log", "", "  "}
	for _, s := range no {
		if audit.IsAuditTable(s) {
			t.Fatalf("%q 不应被识别为审计表", s)
		}
	}
}

// ───────────────────────────── MemSink 拷贝语义 ─────────────────────────────

// TestMemSink_CopiesDetail 落库后调用方再改自己的 map，不得改到已落库的审计。
func TestMemSink_CopiesDetail(t *testing.T) {
	sink := audit.NewMemSink()
	log := audit.NewLog(sink)
	detail := map[string]any{"module": "m.strategy"}
	e := validEntry()
	e.Detail = detail
	if err := log.Record(context.Background(), e); err != nil {
		t.Fatalf("应通过：%v", err)
	}
	detail["module"] = "被改了"
	if got := sink.Entries()[0].Detail["module"]; got != "m.strategy" {
		t.Fatalf("已落库审计被调用方后续修改污染：%v", got)
	}
}
