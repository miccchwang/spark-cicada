// G6 · 预计算一致性 —— 桶数据与源数据对平（抽样重算对比）。
//
// docs/05 G6 三条断言：
//   1. 桶 algo_version == 注册表版本           （见 gate.go: VersionDrift）
//   2. 桶数据与源数据对平（抽样重算对比）      ← 本文件
//   3. 算法升级后仅重算受影响桶                ← 本文件（AffectedBuckets）
package gate

import (
	"fmt"
	"math"
	"sort"
)

// ReconcileItem 一条对平样本。
type ReconcileItem struct {
	Key      string  // 行键（如 "2026-09|TK-TH|S123"）
	Field    string  // 字段（如 "gp"）
	Bucket   *float64 // 桶中物化值（nil = 缺失/NULL）
	Recomputed *float64 // 用当前算法重算的值（nil = 缺失）
}

// ε 容忍浮点误差（对平阈值）。
const reconcileEpsilon = 1e-6

// Reconcile 对平桶值与重算值。
//
// 关键语义：
//   - 两侧都 nil（都缺失）⇒ 一致（缺失语义保持）；
//   - 一侧 nil 一侧有值 ⇒ **不一致**（说明缺失语义被破坏，例如把 NULL 当 0 落库）；
//   - 两侧有值 ⇒ 相对/绝对误差在 ε 内即一致。
//
// 返回不一致项描述（空 = 全部对平）。
func Reconcile(items []ReconcileItem) []string {
	var out []string
	for _, it := range items {
		switch {
		case it.Bucket == nil && it.Recomputed == nil:
			// 一致：都缺失
		case it.Bucket == nil && it.Recomputed != nil:
			out = append(out, fmt.Sprintf(
				"桶 %s.%s 缺失但重算有值 %g（缺失语义被破坏）",
				it.Key, it.Field, *it.Recomputed))
		case it.Bucket != nil && it.Recomputed == nil:
			out = append(out, fmt.Sprintf(
				"桶 %s.%s 有值 %g 但重算缺失（疑似补 0 落库）",
				it.Key, it.Field, *it.Bucket))
		default:
			if !almostEqual(*it.Bucket, *it.Recomputed) {
				out = append(out, fmt.Sprintf(
					"桶 %s.%s 不对平：bucket=%g recomputed=%g",
					it.Key, it.Field, *it.Bucket, *it.Recomputed))
			}
		}
	}
	sort.Strings(out)
	return out
}

func almostEqual(a, b float64) bool {
	diff := math.Abs(a - b)
	if diff <= reconcileEpsilon {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return diff/scale <= reconcileEpsilon
}

// AffectedBuckets 返回因某算法版本升级而需重算的桶（G6-3）。
//
// bucketAlgos：桶 ID → 该桶产出的算法集合。
// 返回需重算的桶 ID（升序）；未受影响的桶**不得**被重算（避免全量重刷）。
func AffectedBuckets(bucketAlgos map[string][]string, upgradedAlgo string) []string {
	var out []string
	for bucket, algos := range bucketAlgos {
		for _, a := range algos {
			if a == upgradedAlgo {
				out = append(out, bucket)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
