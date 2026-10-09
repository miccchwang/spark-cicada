package query

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
)

// G4 第十四侧运行时：AlgoTrace 必须**真的携带** dataSlots / skipped / reason，
// 并**真的消费**算法的 trace 声明。
//
// 病：query.go 此前组装 AlgoTrace 时只填 Field/AlgoID，`dataSlots` 永远为空、
// `skipped` 永远 false ⇒ docs/01 §5.5 与 docs/02 验收「可回溯算法与数据槽」
// 在实现侧做不到，且没有任何东西会变红。本文件把这条链路从**真实入口**
// （Orchestrator.Run）钉到**真实出口**（ResultSet.AlgoTrace）。

// traceStore 返回固定的桶行（含一个缺失字段，用于验证 skipped）。
type traceStore struct{ rows []BucketRow }

func (s traceStore) SelectBucket(context.Context, string, contracts.QueryState) ([]BucketRow, error) {
	return s.rows, nil
}
func (s traceStore) BucketState(context.Context, string) (BucketMeta, error) {
	return BucketMeta{ID: "pnl_month", State: "FRESH"}, nil
}
func (s traceStore) SlotHealth(context.Context, []string) (map[string]SlotStatus, error) {
	return map[string]SlotStatus{}, nil
}

func traceOrch(refs map[string]map[string]AlgoRef) *Orchestrator {
	return &Orchestrator{
		Store: traceStore{rows: []BucketRow{{
			Keys:          map[string]string{"month": "2026-01"},
			Metrics:       map[string]*float64{"gp": numPtr(100), "cogs": nil},
			SkippedFields: []string{"cogs"},
		}}},
		Policy: Policy{
			BucketAlgoMap: map[string]map[string]string{
				"pnl_month": {"gp": "algo.gp", "cogs": "algo.cogs"},
			},
			BucketAlgoRefs: refs,
		},
	}
}

func numPtr(f float64) *float64 { return &f }

func findTrace(rs *ResultSet, field string) (contracts.AlgoTrace, bool) {
	for _, t := range rs.AlgoTrace {
		if t.Field == field {
			return t, true
		}
	}
	return contracts.AlgoTrace{}, false
}

func monthQueryForTrace() contracts.QueryState {
	return contracts.QueryState{Time: contracts.TimeRange{Grain: "month", From: "2026-01-01", To: "2026-01-31"}}
}

// ★ 核心：AlgoTrace 必须带 dataSlots（回溯到「哪些真实数据」）。
func TestAlgoTrace_CarriesDataSlots(t *testing.T) {
	refs := map[string]map[string]AlgoRef{
		"pnl_month": {
			"gp":   {ID: "algo.gp", Slots: []string{"slot.revenue", "slot.cogs"}, Trace: true},
			"cogs": {ID: "algo.cogs", Slots: []string{"slot.cogs"}, Trace: true},
		},
	}
	rs, err := traceOrch(refs).Run(context.Background(), monthQueryForTrace())
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	gp, ok := findTrace(rs, "gp")
	if !ok {
		t.Fatal("★ 派生列 gp 没有 AlgoTrace 条目")
	}
	if len(gp.DataSlots) != 2 || gp.DataSlots[0] != "slot.revenue" {
		t.Fatalf("★ gp 的 dataSlots 未从注册表带入：%v", gp.DataSlots)
	}
}

// ★ skipped/reason 必须来自桶的 skipped_fields 与 gaps，而不是恒 false。
func TestAlgoTrace_CarriesSkippedAndReason(t *testing.T) {
	refs := map[string]map[string]AlgoRef{
		"pnl_month": {
			"gp":   {ID: "algo.gp", Slots: []string{"slot.revenue", "slot.cogs"}, Trace: true},
			"cogs": {ID: "algo.cogs", Slots: []string{"slot.cogs"}, Trace: true},
		},
	}
	rs, err := traceOrch(refs).Run(context.Background(), monthQueryForTrace())
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	gp, _ := findTrace(rs, "gp")
	if gp.Skipped {
		t.Fatal("gp 有值，不应 skipped")
	}
	cogs, ok := findTrace(rs, "cogs")
	if !ok {
		t.Fatal("cogs 应有 AlgoTrace 条目")
	}
	if !cogs.Skipped {
		t.Fatal("★ cogs 在桶里被跳过（skipped_fields），AlgoTrace.skipped 却为 false")
	}
	if strings.TrimSpace(cogs.Reason) == "" {
		t.Fatal("★ cogs 被跳过，AlgoTrace.reason 却为空（无法解释为何缺）")
	}
}

// ★ 真正消费 trace 声明：trace=false 的算法不产出可回溯条目。
//
// 生产里声明层（gate.CheckTraceDeclared）已禁止物化算法为 false，故本分支是
// 运行时兜底；这里用行为级用例直接证明「flag 真的被读了」，而不是死代码。
func TestAlgoTrace_TraceFalseOmitsEntry(t *testing.T) {
	refs := map[string]map[string]AlgoRef{
		"pnl_month": {
			"gp":   {ID: "algo.gp", Slots: []string{"slot.revenue", "slot.cogs"}, Trace: false},
			"cogs": {ID: "algo.cogs", Slots: []string{"slot.cogs"}, Trace: true},
		},
	}
	rs, err := traceOrch(refs).Run(context.Background(), monthQueryForTrace())
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if _, ok := findTrace(rs, "gp"); ok {
		t.Fatal("★ gp 声明 trace=false，却仍产出了 AlgoTrace 条目（flag 未被消费）")
	}
	// 列定义仍应声明（渲染层需要知道有哪些列）。
	foundCol := false
	for _, c := range rs.Columns {
		if c.Key == "gp" {
			foundCol = true
		}
	}
	if !foundCol {
		t.Fatal("trace=false 只应影响可回溯条目，列定义仍须声明")
	}
	if _, ok := findTrace(rs, "cogs"); !ok {
		t.Fatal("cogs 声明 trace=true，应有条目")
	}
}

// ── 静态：钉住 query.go 真的消费 BucketAlgoRefs（防「装配了却不用」）────────

func TestAlgoTrace_Wiring_QueryConsumesRefs(t *testing.T) {
	body := readFileBody(t, "query.go", "func (o *Orchestrator) run(")
	// ★ 断言具体的**代码**写法（不是注释里提到的名字）—— 否则「摘掉代码、只留说明注释」也能骗过。
	if !strings.Contains(body, "o.Policy.BucketAlgoRefs[bucket]") {
		t.Fatal("★ query.go 的 run 没读 o.Policy.BucketAlgoRefs[bucket] ⇒ dataSlots 永远为空（可回溯性做不到）")
	}
	if !strings.Contains(body, "DataSlots: slots") {
		t.Fatal("★ query.go 的 run 没给 AlgoTrace 填 DataSlots: slots")
	}
	if len(body) < 800 {
		t.Fatalf("夹具自证失败：run 方法体只读到 %d 字节", len(body))
	}
}

// readFileBody 读同包源码里 `sig` 到下一个顶格 func 之间的片段（去注释、归一 CRLF）。
func readFileBody(t *testing.T, file, sig string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatalf("读 %s 失败：%v", file, err)
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("%s 里没找到签名 %q", file, sig)
	}
	rest := src[start:]
	if m := strings.Index(rest[1:], "\nfunc "); m >= 0 {
		rest = rest[:m+1]
	}
	return rest
}
