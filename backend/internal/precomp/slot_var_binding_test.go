// slot_var_binding_test.go —— ★ 运行时闸门：槽依赖必须真的落进公式变量表。
//
// 为什么单列一条（2026-10-07 实测）：
//
//	G4 值绑定闸门（gate.CheckFormulaVariablesBound）是**静态**的 ——
//	它只看声明是否自洽（公式自由变量 ⊆ 槽名 ∪ 上游算法名）。但「声明对了」
//	不等于「运行时真的绑上了」：precomp.BuildRow 里曾经**只有**
//	`for k,v := range inputs { vars[k]=v }`，槽依赖从未按 BareName 落进 vars。
//	实测注入证明：摘掉那段槽绑定代码，**原有全部测试仍然全绿** —— 因为既有用例
//	喂的 inputs 键名（rev/cogs）恰好与公式变量名逐字相同，从不经过槽名折算路径。
//
// 本文件的用例**只喂完整槽 ID 作键**（`slot.revenue`），不带任何裸名 ——
// 于是「槽 → 变量名」的折算若缺失，变量表里就只有 `slot.revenue`，
// 公式要的 `revenue` 取不到 ⇒ 必然 Skip。这是对实现的可失败断言。
package precomp_test

import (
	"context"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/precomp"
)

// TestBuildRow_BindsSlotDepsByBareName 槽依赖必须按裸名绑定进变量表。
//
// 场景：公式 `revenue - cogs`，依赖槽 slot.revenue / slot.cogs。
// 调用方**只给完整槽 ID 作键**（这是最贴近真实事实表对齐结果的形态之一）。
// 期望：两个槽各自折算成 revenue / cogs 并绑上 ⇒ 结果为 1000-620=380。
func TestBuildRow_BindsSlotDepsByBareName(t *testing.T) {
	b := newBuilder([]precomp.SlotState{
		{ID: "slot.revenue", Status: "ACTIVE", Coverage: 0.99, Gate: 0.80},
		{ID: "slot.cogs", Status: "ACTIVE", Coverage: 0.95, Gate: 0.80},
	})
	def := precomp.BucketDef{ID: "pnl_month", ProducedBy: []string{"algo.gp"}}

	// ★ 只用完整槽 ID，不给裸名。
	row, err := b.BuildRow(context.Background(), def, map[string]*float64{
		"slot.revenue": f(1000), "slot.cogs": f(620),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Cells) != 1 {
		t.Fatalf("期望 1 个单元格，实际 %+v", row.Cells)
	}
	c := row.Cells[0]
	if c.Value == nil {
		t.Fatalf("★ 槽依赖未按裸名绑定 ⇒ 公式取不到变量而 Skip（reason=%q）。"+
			"槽 slot.revenue 必须折算成变量名 revenue", c.Reason)
	}
	if *c.Value != 380 {
		t.Fatalf("slot.revenue-slot.cogs = %v，期望 380", *c.Value)
	}
}

// TestBuildRow_ExplicitInputWinsOverSlotName 调用方显式给的裸名优先于槽 ID 折算。
//
// 这是既有语义（inputs 是「该行可用的原子量」），不能因为新增折算而被覆盖。
func TestBuildRow_ExplicitInputWinsOverSlotName(t *testing.T) {
	b := newBuilder([]precomp.SlotState{
		{ID: "slot.revenue", Status: "ACTIVE", Coverage: 0.99, Gate: 0.80},
		{ID: "slot.cogs", Status: "ACTIVE", Coverage: 0.95, Gate: 0.80},
	})
	def := precomp.BucketDef{ID: "pnl_month", ProducedBy: []string{"algo.gp"}}
	// 同时给裸名与槽 ID，且**值不同**：裸名应胜出。
	row, err := b.BuildRow(context.Background(), def, map[string]*float64{
		"revenue": f(500), "cogs": f(100), // ← 应被采用
		"slot.revenue": f(1000), "slot.cogs": f(620), // ← 不应覆盖
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Cells) != 1 || row.Cells[0].Value == nil {
		t.Fatalf("期望算出结果，实际 %+v", row.Cells)
	}
	if *row.Cells[0].Value != 400 {
		t.Fatalf("显式裸名应优先：500-100=400，实际 %v", *row.Cells[0].Value)
	}
}
