// contract_guard_test.go —— G2/G3 出站契约守卫测试。
//
// 证明 gate.CheckDefaultCollapsed / CheckNoZeroImputation 在**生产路径**上有调用点。
package slot_test

import (
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

func TestContractGuard_AcceptsValidContract(t *testing.T) {
	g := slot.NewContractGuard(nil)
	dc := &contracts.DataContract{
		V: contracts.DataContractVersion,
		Levels: []contracts.LevelSummary{
			{Level: "L0", Key: "overview", DefaultExpanded: true},
			{Level: "L1", Key: "domain", DefaultExpanded: false},
		},
		Columns: []contracts.ColumnDef{
			{Key: "gp", Perm: "L3", Kind: "currency", AlgoID: "algo.gp"},
		},
		Rows: []contracts.Row{
			{"gp": contracts.NewNum(1234.5)},
		},
		AlgoTrace: []contracts.AlgoTrace{
			{Field: "gp", AlgoID: "algo.gp", Skipped: false},
		},
	}
	if err := g.MustValidContract(dc); err != nil {
		t.Fatalf("合法契约不应被拒: %v", err)
	}
}

// G2：把非 L0 层级默认展开 ⇒ 出站必须被拒。
func TestContractGuard_RejectsExpandedNonL0(t *testing.T) {
	g := slot.NewContractGuard(nil)
	dc := &contracts.DataContract{
		V: contracts.DataContractVersion,
		Levels: []contracts.LevelSummary{
			{Level: "L0", Key: "overview", DefaultExpanded: true},
			{Level: "L2", Key: "channel", DefaultExpanded: true}, // ★ 违规
		},
	}
	err := g.MustValidContract(dc)
	if err == nil {
		t.Fatal("G2：非 L0 层级默认展开应被拒")
	}
}

// G3：给缺失值补 0 ⇒ 出站必须被拒。
func TestContractGuard_RejectsZeroImputation(t *testing.T) {
	g := slot.NewContractGuard(nil)
	dc := &contracts.DataContract{
		V: contracts.DataContractVersion,
		Columns: []contracts.ColumnDef{
			{Key: "cogs", Perm: "L4", Kind: "currency", AlgoID: "algo.cogs"},
		},
		Rows: []contracts.Row{
			{"cogs": contracts.NewNum(0)}, // ★ 违规：疑似占位补零
		},
		AlgoTrace: []contracts.AlgoTrace{
			{Field: "cogs", AlgoID: "algo.cogs", Skipped: false},
		},
	}
	if err := g.MustValidContract(dc); err == nil {
		t.Fatal("G3：补 0 占位应被拒")
	}
}

// G3：skipped 字段却带值 ⇒ 出站必须被拒。
func TestContractGuard_RejectsSkippedWithValue(t *testing.T) {
	g := slot.NewContractGuard(nil)
	dc := &contracts.DataContract{
		V: contracts.DataContractVersion,
		Columns: []contracts.ColumnDef{
			{Key: "net_contrib", Perm: "L4", Kind: "currency", AlgoID: "algo.net_contrib"},
		},
		Rows: []contracts.Row{
			{"net_contrib": contracts.NewNum(999)},
		},
		AlgoTrace: []contracts.AlgoTrace{
			{Field: "net_contrib", AlgoID: "algo.net_contrib", Skipped: true, Reason: "slot.affiliate MISSING"},
		},
	}
	if err := g.MustValidContract(dc); err == nil {
		t.Fatal("G3：skipped=true 却带值应被拒")
	}
}

// G3：真实 0 在白名单内 ⇒ 放行（不误伤）。
func TestContractGuard_AllowsWhitelistedZero(t *testing.T) {
	g := slot.NewContractGuard(map[string]bool{"cogs": true})
	dc := &contracts.DataContract{
		V: contracts.DataContractVersion,
		Columns: []contracts.ColumnDef{
			{Key: "cogs", Perm: "L4", Kind: "currency", AlgoID: "algo.cogs"},
		},
		Rows: []contracts.Row{
			{"cogs": contracts.NewNum(0)},
		},
		AlgoTrace: []contracts.AlgoTrace{
			{Field: "cogs", AlgoID: "algo.cogs", Skipped: false},
		},
	}
	if err := g.MustValidContract(dc); err != nil {
		t.Fatalf("白名单内的真实 0 不应被拒: %v", err)
	}
}

// nil 契约不得出站。
func TestContractGuard_RejectsNil(t *testing.T) {
	g := slot.NewContractGuard(nil)
	if err := g.MustValidContract(nil); err == nil {
		t.Fatal("nil 契约应被拒")
	}
}

// 空 levels 允许（部分查询不带层级汇总），但带 levels 就必须合法。
func TestContractGuard_EmptyLevelsAllowed(t *testing.T) {
	g := slot.NewContractGuard(nil)
	dc := &contracts.DataContract{V: contracts.DataContractVersion}
	if err := g.MustValidContract(dc); err != nil {
		t.Fatalf("空 levels 应被允许: %v", err)
	}
}
