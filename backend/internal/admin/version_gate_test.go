// version_gate_test.go —— G4 第十侧在**登记入口**的接线断言。
//
// 为什么单列一个文件：本侧的核心价值不是「版本是个正整数」（那是 gate 包的
// 声明层测试），而是**版本在登记动作里真的被比较了** ——
// 这是「字段被读进来、写进 DB，却没有任何判定消费」这个病在 `version` 上的
// 具体形态。此前的 `ValidateAlgorithm` 对 Version **零校验**：
// 不查 >0、不查倒退、不查与既有声明是否一致。
package admin

import (
	"context"
	"strings"
	"testing"
)

func registerTestSlot(t *testing.T, p *Plane) {
	t.Helper()
	if err := p.RegisterSlot(context.Background(), "admin", Slot{
		ID: "slot.cogs", Name: "成本", SourceKind: "master_table",
		SourceRef: "master.cogs", KeyStrategy: "sku+month", Status: "ACTIVE",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAdmin_RejectsZeroVersion：版本 0 必须被拒（不得悄悄填 1）。
//
// ★ 这条断言钉的是「注册入口的**校验**」而不是「缺省填充」：
//   本方法此前在 Version<=0 时静默写成 1 —— 那是**缺省填充，不是校验**，
//   它让「没写版本」与「版本是 1」在行为上无法区分。现改为显式拒绝。
func TestAdmin_RejectsZeroVersion(t *testing.T) {
	p := New(newMem(), nil)
	ctx := context.Background()
	registerTestSlot(t, p)

	err := p.RegisterAlgorithm(ctx, "admin", Algorithm{
		ID: "algo.cogs", Name: "成本", Version: 0,
		Formula: "unit_cost * qty", Unit: "THB", Permission: "L3",
		DependsOnSlots: []string{"slot.cogs"}, WritesBucket: "pnl_month",
	})
	if err == nil {
		t.Fatal("★ 版本 0 竟被接受 ⇒ 版本号在登记入口没有被校验（会被静默填成 1）")
	}
	if !strings.Contains(err.Error(), "版本") {
		t.Fatalf("报错应指向版本号，实际：%v", err)
	}
}

// TestAdmin_RejectsNegativeVersion：负版本必须被拒。
func TestAdmin_RejectsNegativeVersion(t *testing.T) {
	p := New(newMem(), nil)
	registerTestSlot(t, p)

	err := p.RegisterAlgorithm(context.Background(), "admin", Algorithm{
		ID: "algo.cogs", Name: "成本", Version: -7,
		Formula: "unit_cost * qty", Unit: "THB", Permission: "L3",
		DependsOnSlots: []string{"slot.cogs"}, WritesBucket: "pnl_month",
	})
	if err == nil {
		t.Fatal("★ 负版本竟被接受 ⇒ 版本号失去判据")
	}
}

// TestAdmin_AlgorithmVersionNotRegressing：版本只能前进，倒退必须被拒。
//
// ★ 这是本侧**唯一有依据的比较**：版本的全部意义就在于比较
//   （G6 靠版本差判断「改了公式要重算哪些桶」）。
func TestAdmin_AlgorithmVersionNotRegressing(t *testing.T) {
	p := New(newMem(), nil)
	ctx := context.Background()
	registerTestSlot(t, p)

	base := Algorithm{
		ID: "algo.cogs", Name: "成本", Version: 3,
		Formula: "unit_cost * qty", Unit: "THB", Permission: "L3",
		DependsOnSlots: []string{"slot.cogs"}, WritesBucket: "pnl_month",
	}
	if err := p.RegisterAlgorithm(ctx, "admin", base); err != nil {
		t.Fatalf("首次登记应成功：%v", err)
	}

	// 前进 ⇒ 放行。
	adv := base
	adv.Version = 4
	if err := p.RegisterAlgorithm(ctx, "admin", adv); err != nil {
		t.Fatalf("版本前进应放行：%v", err)
	}

	// 倒退 ⇒ 必须拒。
	back := base
	back.Version = 2
	err := p.RegisterAlgorithm(ctx, "admin", back)
	if err == nil {
		t.Fatal("★ 版本从 4 倒退到 2 竟被放行 ⇒ 版本号没有被真正比较" +
			"（已重算过的桶会被判为『版本超前』，重算结论随之失真）")
	}
	if !strings.Contains(err.Error(), "倒退") {
		t.Fatalf("报错应指出「倒退」，实际：%v", err)
	}

	// 持平 ⇒ 放行（如仅改注释、改 notes）。
	same := base
	same.Version = 4
	if err := p.RegisterAlgorithm(ctx, "admin", same); err != nil {
		t.Fatalf("版本持平应放行：%v", err)
	}
}
