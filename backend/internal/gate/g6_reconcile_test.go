// g6_reconcile_test.go —— G6 预计算一致性（对平 + 受影响桶）测试。
package gate_test

import (
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

func ff(v float64) *float64 { return &v }

func TestG6_ReconcileAligned(t *testing.T) {
	items := []gate.ReconcileItem{
		{Key: "k1", Field: "gp", Bucket: ff(380), Recomputed: ff(380)},
		{Key: "k2", Field: "cogs", Bucket: nil, Recomputed: nil}, // 都缺失 = 一致
	}
	if v := gate.Reconcile(items); len(v) != 0 {
		t.Fatalf("G6 对平数据不应报错: %v", v)
	}
}

func TestG6_ReconcileDetectsMismatch(t *testing.T) {
	// 值不等
	if v := gate.Reconcile([]gate.ReconcileItem{
		{Key: "k1", Field: "gp", Bucket: ff(380), Recomputed: ff(390)},
	}); len(v) == 0 {
		t.Fatal("G6 应检出值不对平")
	}
	// 桶缺失但重算有值（缺失语义被破坏）
	if v := gate.Reconcile([]gate.ReconcileItem{
		{Key: "k1", Field: "gp", Bucket: nil, Recomputed: ff(1)},
	}); len(v) == 0 {
		t.Fatal("G6 应检出「桶缺失但重算有值」")
	}
	// 桶有值但重算缺失（疑似补 0）
	if v := gate.Reconcile([]gate.ReconcileItem{
		{Key: "k1", Field: "gp", Bucket: ff(0), Recomputed: nil},
	}); len(v) == 0 {
		t.Fatal("G6 应检出「桶有值但重算缺失」（疑似补 0）")
	}
}

func TestG6_AffectedBuckets(t *testing.T) {
	ba := map[string][]string{
		"pnl_month":     {"algo.gp", "algo.net_contrib"},
		"gmroi_week":    {"algo.gmp"},
		"inventory_day": {"algo.dos"},
	}
	// 升 algo.gp ⇒ 仅 pnl_month 受影响
	got := gate.AffectedBuckets(ba, "algo.gp")
	if len(got) != 1 || got[0] != "pnl_month" {
		t.Fatalf("G6 受影响桶错误：%v want [pnl_month]", got)
	}
	// 升 algo.gmp ⇒ 仅 gmroi_week
	if got := gate.AffectedBuckets(ba, "algo.gmp"); len(got) != 1 || got[0] != "gmroi_week" {
		t.Fatalf("G6 受影响桶错误：%v", got)
	}
	// 升未被任何桶使用的算法 ⇒ 无（不得全量重刷）
	if got := gate.AffectedBuckets(ba, "algo.unused"); len(got) != 0 {
		t.Fatalf("G6 不应重算未受影响的桶：%v", got)
	}
}
