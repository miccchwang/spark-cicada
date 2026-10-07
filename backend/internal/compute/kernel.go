// Package compute —— Go 侧的计算内核客户端。
//
// **边界纪律（docs/01 §4.1）**：Go **永不实现业务公式**。
// 所有数值法则必须经此包转发到 Rust 内核（gRPC 或子进程）。
//
// 提供两种实现：
//   * SubprocessKernel —— 调用 spark-compute 二进制（--eval），零依赖、可立即运行。
//   * （后续）GRPCKernel —— 走 contracts/compute.proto 定义的服务。
//
// 两者计算逻辑完全一致（同一份 Rust 代码），因此切换实现不产生口径漂移。
package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Scalar 可空标量（与 Rust scalar::Scalar 语义一致）。
// nil 指针表示缺失（Missing）；非 nil 表示有确定数值（含 0）。
type Scalar = *float64

// BareName 把带前缀的 ID 折成本内核公式里的**变量名**：
//
//	slot.revenue → revenue
//	algo.gp      → gp
//
// 这是「注册表 ID ↔ 公式自由变量」之间**唯一**的约定，Go 侧的唯一权威实现。
// 内核 `spark-compute` 对查不到的变量返回 `Scalar::Missing`
//（lib.rs: `unwrap_or(Scalar::Missing)`）—— 所以一旦这里和公式写岔了，
// 结果是**静默取 Missing**，而不是报错。`gate.CheckFormulaVariablesBound`
// 用同一条规则做静态校验，两侧必须同步。
func BareName(id string) string {
	if i := strings.LastIndex(id, "."); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

// Kernel 是计算内核的统一接口。
type Kernel interface {
	// EvalFormula 按公式与变量求值。缺失沿依赖链传播（fail-closed）。
	EvalFormula(ctx context.Context, formula string, vars map[string]Scalar) (Scalar, error)
	// Health 返回内核版本与就绪状态。
	Health(ctx context.Context) (string, error)
}

// ───────────────────────────── 子进程实现 ─────────────────────────────

// SubprocessKernel 通过调用 spark-compute 可执行文件完成求值。
// 适合：本地开发、CI、gRPC 尚未部署时的降级通道。
type SubprocessKernel struct {
	BinPath string
	Timeout time.Duration
}

// NewSubprocessKernel 构造子进程内核客户端。
func NewSubprocessKernel(binPath string) *SubprocessKernel {
	return &SubprocessKernel{BinPath: binPath, Timeout: 5 * time.Second}
}

type evalReq struct {
	Formula string                    `json:"formula"`
	Vars    map[string]json.RawMessage `json:"vars"`
}

type evalResp struct {
	Result  *float64 `json:"result"`
	Skipped bool     `json:"skipped"`
	Reason  string   `json:"reason,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// EvalFormula 实现 Kernel。
func (k *SubprocessKernel) EvalFormula(ctx context.Context, formula string, vars map[string]Scalar) (Scalar, error) {
	req := evalReq{Formula: formula, Vars: map[string]json.RawMessage{}}
	for name, v := range vars {
		if v == nil {
			req.Vars[name] = json.RawMessage("null")
		} else {
			// 用 strconv 保证数值精度与 JSON 合法性
			req.Vars[name] = json.RawMessage(strconv.FormatFloat(*v, 'f', -1, 64))
		}
	}
	in, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("compute: marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, k.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, k.BinPath, "--eval")
	cmd.Stdin = bytes.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("compute: run kernel: %w (stderr: %s)", err, errb.String())
	}

	var resp evalResp
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("compute: unmarshal: %w (raw: %s)", err, out.String())
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("compute: kernel error: %s", resp.Error)
	}
	return resp.Result, nil
}

// Health 实现 Kernel。
func (k *SubprocessKernel) Health(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, k.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.BinPath, "--health")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("compute: health: %w", err)
	}
	var h struct {
		KernelVersion string `json:"kernel_version"`
		Ready         bool   `json:"ready"`
	}
	if err := json.Unmarshal(out, &h); err != nil {
		return "", err
	}
	if !h.Ready {
		return h.KernelVersion, fmt.Errorf("compute: kernel not ready")
	}
	return h.KernelVersion, nil
}
