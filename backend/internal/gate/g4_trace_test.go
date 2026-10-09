package gate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// G4 第十四侧：算法的**可回溯声明**（trace）必须真实可解析、与 DB 默认值同源，
// 并且**在出站路径上真的被执行**（每条派生列都要有 AlgoTrace 且带 dataSlots）。
//
// 病：`trace` 被读进来、写进 DB（registry_algorithm.trace），却没有任何判定消费
// 其声明值（第二十二个变种）；更严重的是**输出侧也没被消费** ——
// query.go 组装 AlgoTrace 时从不填 dataSlots，docs/01 §5.5 / docs/02 验收
// 「可回溯算法与数据槽」在实现侧做不到，且没有任何东西会变红。

// ── 1. 声明层：解析 ─────────────────────────────────────────────────────────

func TestParseTrace_OnlyAcceptsRealBool(t *testing.T) {
	if v, ok := gate.ParseTrace(true); !ok || !v {
		t.Fatalf("true 应解析为 (true, true)，得到 (%v, %v)", v, ok)
	}
	if v, ok := gate.ParseTrace(false); !ok || v {
		t.Fatalf("false 应解析为 (false, true)，得到 (%v, %v)", v, ok)
	}
	// 这些「看起来像布尔但不是」的取值必须被拒（fail-closed）——
	// 它们正是「写了等于没写」的入口。
	for _, bad := range []any{"true", "false", "yes", "no", 1, 0, nil, 1.0} {
		if _, ok := gate.ParseTrace(bad); ok {
			t.Fatalf("ParseTrace(%#v) 应为不可判定（false），却返回 true", bad)
		}
	}
}

func TestDefaultTrace_IsTrue(t *testing.T) {
	if !gate.DefaultTrace {
		t.Fatal("DefaultTrace 必须为 true —— 缺省应「可回溯」（与 0002 DEFAULT true 同源）")
	}
}

// ── 2. 声明层：CheckTraceDeclared ────────────────────────────────────────────

func TestCheckTraceDeclared_RejectsNonBool(t *testing.T) {
	docs := []gate.TraceDoc{
		{AlgoID: "algo.x", Declared: true, Valid: false, Trace: false, Materializes: false},
	}
	v := gate.CheckTraceDeclared(docs)
	if len(v) == 0 {
		t.Fatal("声明了但值不是布尔 ⇒ 必须报违规")
	}
	if !strings.Contains(v[0], "不是布尔") {
		t.Fatalf("违规原因应指出「不是布尔」，得到：%s", v[0])
	}
}

func TestCheckTraceDeclared_RejectsFalseForMaterializingAlgo(t *testing.T) {
	docs := []gate.TraceDoc{
		{AlgoID: "algo.gp", Declared: true, Valid: true, Trace: false, Materializes: true},
	}
	v := gate.CheckTraceDeclared(docs)
	if len(v) == 0 {
		t.Fatal("物化算法声明 trace: false ⇒ 必须报违规（输出将不可回溯）")
	}
	if !strings.Contains(v[0], "trace: true") {
		t.Fatalf("违规原因应给出可行动提示，得到：%s", v[0])
	}
}

func TestCheckTraceDeclared_AllowsAbsentTrueAndNonMaterializing(t *testing.T) {
	docs := []gate.TraceDoc{
		// 未声明 ⇒ 缺省 true（与 0002 DEFAULT 同源），不报错。
		{AlgoID: "algo.a", Declared: false, Valid: true, Trace: true, Materializes: true},
		// 显式 true + 物化 ⇒ 合法。
		{AlgoID: "algo.b", Declared: true, Valid: true, Trace: true, Materializes: true},
		// 显式 false 但**不物化**（不写桶）⇒ 合法（没有输出要回溯）。
		{AlgoID: "algo.c", Declared: true, Valid: true, Trace: false, Materializes: false},
	}
	if v := gate.CheckTraceDeclared(docs); len(v) != 0 {
		t.Fatalf("这些声明都应合法，却报：%v", v)
	}
}

// ── 3. 声明层：与 0002 默认值同源 ─────────────────────────────────────────────

func TestCheckTraceDefaultMatchesDB_RealMigrationIsConsistent(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "sql", "migrations", "0002_slots_and_rules.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if v := gate.CheckTraceDefaultMatchesDB(string(body)); len(v) != 0 {
		t.Fatalf("真实 0002 迁移应与 Go 侧 DefaultTrace 同源，却报：%v", v)
	}
}

func TestCheckTraceDefaultMatchesDB_DetectsDriftAndMissing(t *testing.T) {
	// 缺声明 ⇒ 报错。
	if v := gate.CheckTraceDefaultMatchesDB("create table t (x int);"); len(v) == 0 {
		t.Fatal("0002 里找不到 trace 列声明 ⇒ 必须报错")
	}
	// 默认值被改反 ⇒ 报错（这正是「Go 加载放行、入库默认成另一个值」的分叉）。
	tampered := "trace boolean NOT NULL DEFAULT false,"
	if v := gate.CheckTraceDefaultMatchesDB(tampered); len(v) == 0 {
		t.Fatal("DB DEFAULT=false 而 Go DefaultTrace=true ⇒ 必须报分叉")
	}
}

// ── 4. 出站层：CheckAlgoTraceCoversColumns ───────────────────────────────────

func TestCheckAlgoTraceCoversColumns_AcceptsFullyTraceable(t *testing.T) {
	dc := &contracts.DataContract{
		Columns: []contracts.ColumnDef{
			{Key: "month", Kind: "text"}, // 非派生列：不要求 AlgoTrace
			{Key: "gp", Kind: "currency", AlgoID: "algo.gp"},
		},
		AlgoTrace: []contracts.AlgoTrace{
			{Field: "gp", AlgoID: "algo.gp", DataSlots: []string{"slot.revenue", "slot.cogs"}},
		},
	}
	if v := gate.CheckAlgoTraceCoversColumns(dc); len(v) != 0 {
		t.Fatalf("完整可回溯的契约不应被拒，却报：%v", v)
	}
}

func TestCheckAlgoTraceCoversColumns_RejectsMissingEntry(t *testing.T) {
	dc := &contracts.DataContract{
		Columns: []contracts.ColumnDef{{Key: "gp", AlgoID: "algo.gp"}},
	}
	v := gate.CheckAlgoTraceCoversColumns(dc)
	if len(v) == 0 {
		t.Fatal("派生列没有 AlgoTrace 条目 ⇒ 必须报违规")
	}
}

func TestCheckAlgoTraceCoversColumns_RejectsAlgoIDMismatch(t *testing.T) {
	dc := &contracts.DataContract{
		Columns:   []contracts.ColumnDef{{Key: "gp", AlgoID: "algo.gp"}},
		AlgoTrace: []contracts.AlgoTrace{{Field: "gp", AlgoID: "algo.cogs", DataSlots: []string{"slot.cogs"}}},
	}
	v := gate.CheckAlgoTraceCoversColumns(dc)
	if len(v) == 0 {
		t.Fatal("列 algoId 与溯源 algoId 不一致 ⇒ 必须报违规")
	}
}

func TestCheckAlgoTraceCoversColumns_RejectsEmptyDataSlots(t *testing.T) {
	dc := &contracts.DataContract{
		Columns:   []contracts.ColumnDef{{Key: "gp", AlgoID: "algo.gp"}},
		AlgoTrace: []contracts.AlgoTrace{{Field: "gp", AlgoID: "algo.gp"}}, // 无 dataSlots
	}
	v := gate.CheckAlgoTraceCoversColumns(dc)
	if len(v) == 0 {
		t.Fatal("AlgoTrace 缺 dataSlots ⇒ 必须报违规（无法回溯到哪些真实数据）")
	}
	if !strings.Contains(v[0], "dataSlots") {
		t.Fatalf("违规原因应指出 dataSlots，得到：%s", v[0])
	}
}

func TestCheckAlgoTraceCoversColumns_RejectsNil(t *testing.T) {
	if v := gate.CheckAlgoTraceCoversColumns(nil); len(v) == 0 {
		t.Fatal("nil 契约 ⇒ 必须报违规")
	}
}
