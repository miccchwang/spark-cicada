package main_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/compute"
)

// kernelBin 定位已构建的计算内核二进制（相对测试工作目录回溯到仓库根）。
func kernelBin(t *testing.T) string {
	t.Helper()
	// 测试工作目录 = backend/cmd/sparkd → 仓库根在 ../../../.. 之外
	candidates := []string{
		filepath.Join("..", "..", "spark-compute.exe"),                          // backend/spark-compute.exe
		filepath.Join("..", "..", "..", "compute", "target", "release", "spark-compute.exe"), // compute/target/release
		filepath.Join("..", "..", "..", "compute", "target", "debug", "spark-compute.exe"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	t.Skip("compute binary not built; run `cd compute && cargo build --release`")
	return ""
}

// 真实的 Go↔Rust 端到端：Go 通过 SubprocessKernel 调用 Rust 内核。
func TestKernelIntegration(t *testing.T) {
	bin := kernelBin(t)
	k := compute.NewSubprocessKernel(bin)
	ctx := context.Background()

	v, err := k.Health(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	t.Logf("kernel version: %s", v)

	// gp 正常
	rev, cogs := 1000.0, 620.0
	gp, err := k.EvalFormula(ctx, "rev - cogs", map[string]*float64{"rev": &rev, "cogs": &cogs})
	if err != nil || gp == nil || *gp != 380 {
		t.Fatalf("gp: got %v err %v want 380", gp, err)
	}

	// 缺失传播：cogs 为 nil ⇒ 结果必须 nil（不是 0）
	gpMissing, err := k.EvalFormula(ctx, "rev - cogs", map[string]*float64{"rev": &rev, "cogs": nil})
	if err != nil {
		t.Fatalf("eval err: %v", err)
	}
	if gpMissing != nil {
		t.Fatalf("缺失应返回 nil（不补 0），实际 %v", *gpMissing)
	}

	// 分母 0 ⇒ nil
	zero, thirty := 0.0, 30.0
	gmp, err := k.EvalFormula(ctx, "rev == 0 ? null : gp / rev", map[string]*float64{"rev": &zero, "gp": &thirty})
	if err != nil {
		t.Fatal(err)
	}
	if gmp != nil {
		t.Fatalf("rev=0 时 gmp 必须缺失，实际 %v", *gmp)
	}
}
