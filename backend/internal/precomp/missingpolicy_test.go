package precomp_test

import (
	"context"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/precomp"
)

// G4 第十三侧（运行时）：`missing_policy` 的三值语义必须**真的被分路执行**。
//
// 病：`precomp.BuildRow` 此前唯一的判定是 `val == nil && a.MissingPolicy == "skip"`，
// 其余一切取值（null / error / 任何拼错的值）统统落到同一个默认分支 —— 写 NULL、
// 不报错。于是 `missing_policy: error`（声明「缺失即报错」）与 `null` 与「随便写」
// **行为完全等价** ⇒ 声明等于装饰。
//
// 本文件从 BuildRow 入口进、以单元格结果/错误出，逐一验证三种策略：
//   - skip  ⇒ 跳过（Skipped=true，记 skipped_fields，不写 0）；
//   - null  ⇒ 显式置空（Skipped=false，Value=nil）；
//   - error ⇒ 构建失败（fail-closed，绝不静默产出空值）；
//   - 乱写  ⇒ 构建失败（不可解析即拒绝，不猜默认值）。

// builderWithPolicy 造一个只产出 algo.gp 的 Builder，策略由参数指定。
func builderWithPolicy(policy string) *precomp.Builder {
	return &precomp.Builder{
		Kernel: fakeKernel{},
		Slots: func(_ context.Context, ids []string) ([]precomp.SlotState, error) {
			out := make([]precomp.SlotState, 0, len(ids))
			for _, id := range ids {
				out = append(out, precomp.SlotState{ID: id, Status: "ACTIVE", Coverage: 0.99, Gate: 0.80})
			}
			return out, nil
		},
		Algos: func(_ context.Context, _ []string) ([]precomp.AlgoDef, error) {
			return []precomp.AlgoDef{{
				ID: "algo.gp", Version: 1, Formula: "revenue - cogs", Unit: "THB",
				DependsOnSlots: []string{"slot.revenue", "slot.cogs"}, MissingPolicy: policy,
			}}, nil
		},
	}
}

// buildMissingCogs 触发「依赖值缺失」：只给 revenue、不给 cogs ⇒ 公式求值为 nil。
func buildMissingCogs(t *testing.T, policy string) (*precomp.RowResult, error) {
	t.Helper()
	b := builderWithPolicy(policy)
	def := precomp.BucketDef{ID: "pnl_month", ProducedBy: []string{"algo.gp"}}
	return b.BuildRow(context.Background(), def, map[string]*float64{"revenue": f(1000)})
}

func TestMissingPolicy_Runtime_SkipRecordsSkipped(t *testing.T) {
	row, err := buildMissingCogs(t, "skip")
	if err != nil {
		t.Fatalf("skip 策略不应报错：%v", err)
	}
	if len(row.Cells) != 1 || !row.Cells[0].Skipped {
		t.Fatalf("skip ⇒ 单元格应 Skipped=true，得到 %+v", row.Cells)
	}
	if row.Cells[0].Value != nil {
		t.Fatalf("skip 的字段必须为 nil（NULL），绝不写 0")
	}
	if len(row.SkippedFields) != 1 || row.SkippedFields[0] != "gp" {
		t.Fatalf("skip 应记入 skipped_fields=[gp]，得到 %v", row.SkippedFields)
	}
}

func TestMissingPolicy_Runtime_NullWritesNullWithoutSkipping(t *testing.T) {
	row, err := buildMissingCogs(t, "null")
	if err != nil {
		t.Fatalf("null 策略不应报错：%v", err)
	}
	if len(row.Cells) != 1 {
		t.Fatalf("null ⇒ 应有 1 个单元格，得到 %d", len(row.Cells))
	}
	c := row.Cells[0]
	if c.Skipped {
		t.Fatalf("null ⇒ 单元格**不应**被标为 Skipped（否则与 skip 无法区分），得到 %+v", c)
	}
	if c.Value != nil {
		t.Fatalf("null ⇒ 值应为 nil（写 NULL），得到 %v", c.Value)
	}
	if len(row.SkippedFields) != 0 {
		t.Fatalf("null ⇒ 不应记入 skipped_fields（那是 skip 的语义），得到 %v", row.SkippedFields)
	}
	if !strings.Contains(c.Reason, "null") {
		t.Fatalf("null ⇒ 原因应点明 missing_policy=null（可审计），得到 %q", c.Reason)
	}
}

func TestMissingPolicy_Runtime_ErrorFailsClosed(t *testing.T) {
	_, err := buildMissingCogs(t, "error")
	if err == nil {
		t.Fatal("★ error 策略下依赖值缺失必须**构建失败**（fail-closed），却静默成功 ⇒ 声明等于装饰")
	}
	if !strings.Contains(err.Error(), "error") {
		t.Fatalf("报错应点明 missing_policy=error，实际：%v", err)
	}
}

func TestMissingPolicy_Runtime_UnknownPolicyFailsClosed(t *testing.T) {
	_, err := buildMissingCogs(t, "ignore")
	if err == nil {
		t.Fatal("★ 不可解析的策略必须 fail-closed 拒绝构建（绝不猜默认值 —— 猜错方向会让「该报错」静默变「写空」）")
	}
	if !strings.Contains(err.Error(), "missing_policy") {
		t.Fatalf("报错应指出 missing_policy，实际：%v", err)
	}
}

// 反向：三种策略在**依赖值齐全**时都必须正常算出结果（闸门不得误伤正常路径）。
func TestMissingPolicy_Runtime_AllPoliciesComputeWhenComplete(t *testing.T) {
	for _, policy := range []string{"skip", "null", "error"} {
		b := builderWithPolicy(policy)
		def := precomp.BucketDef{ID: "pnl_month", ProducedBy: []string{"algo.gp"}}
		row, err := b.BuildRow(context.Background(), def, map[string]*float64{"revenue": f(1000), "cogs": f(620)})
		if err != nil {
			t.Fatalf("policy=%s 依赖齐全时不应报错：%v", policy, err)
		}
		if len(row.Cells) != 1 || row.Cells[0].Value == nil || *row.Cells[0].Value != 380 {
			t.Fatalf("policy=%s 依赖齐全时应算出 380，得到 %+v", policy, row.Cells)
		}
	}
}
