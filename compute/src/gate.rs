//! 覆盖率门控与规则求值（M-PRECOMP 依赖判定 + M-RULE）。

use crate::scalar::Scalar;

/// 数据槽状态（与 registry_slot.status 对齐）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SlotStatus {
    Active,
    Missing,
    Degraded,
    Disabled,
}

impl SlotStatus {
    pub fn parse(s: &str) -> Self {
        match s.to_ascii_uppercase().as_str() {
            "ACTIVE" => SlotStatus::Active,
            "MISSING" => SlotStatus::Missing,
            "DEGRADED" => SlotStatus::Degraded,
            "DISABLED" => SlotStatus::Disabled,
            _ => SlotStatus::Missing, // 未知状态一律按不可用（fail-closed）
        }
    }

    /// 该状态是否「根本不可用」（无需再看覆盖率）。
    pub fn hard_unavailable(self) -> bool {
        matches!(self, SlotStatus::Missing | SlotStatus::Disabled)
    }
}

/// 槽运行态：状态 + 实测覆盖率 + 门限。
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct SlotHealth {
    pub status: SlotStatus,
    pub coverage: f64,
    pub gate: f64,
}

/// 覆盖率门控判定（闸门 G4）。
///
/// 返回 `Ok(())` 表示可用；否则给出不可用原因。
/// 判定顺序：状态优先（MISSING/DISABLED 直接淘汰）→ 覆盖率 ≥ 门限。
pub fn coverage_gate(h: SlotHealth) -> Result<(), String> {
    if h.status.hard_unavailable() {
        return Err(format!("slot status = {:?} (hard unavailable)", h.status));
    }
    if h.coverage < h.gate {
        return Err(format!(
            "coverage {:.4} < gate {:.4}",
            h.coverage, h.gate
        ));
    }
    Ok(())
}

/// 给定一组依赖槽，判断某算法是否应被跳过（任一槽不可用 ⇒ 跳过）。
/// 返回被跳过的槽清单与原因；空 = 可执行。
pub fn dependencies_ok(slots: &[SlotHealth]) -> Result<(), Vec<(usize, String)>> {
    let mut failures = Vec::new();
    for (i, h) in slots.iter().enumerate() {
        if let Err(reason) = coverage_gate(*h) {
            failures.push((i, reason));
        }
    }
    if failures.is_empty() { Ok(()) } else { Err(failures) }
}

// ───────────────────────────── 规则求值（M-RULE） ─────────────────────────────

/// 规则项（费率/固定额），带生效区间。
#[derive(Debug, Clone)]
pub struct RuleItem {
    pub id: String,
    pub name: String,
    pub rate: Scalar,
    pub flat_per_order: Scalar,
    pub vat_included: bool,
    /// ISO 日期（YYYY-MM-DD）；空 = 无下界
    pub effective_from: String,
    /// ISO 日期；空 = 无上界
    pub effective_to: String,
}

/// 判断规则项在 `as_of`（YYYY-MM-DD）是否生效。
/// 区间语义：[effective_from, effective_to)（含起不含止）。
pub fn is_effective(item: &RuleItem, as_of: &str) -> bool {
    if !item.effective_from.is_empty() && as_of < item.effective_from.as_str() {
        return false;
    }
    if !item.effective_to.is_empty() && as_of >= item.effective_to.as_str() {
        return false;
    }
    true
}

/// 求值规则集：过滤出生效项；返回 (生效项, 被过滤项的 id)。
pub fn eval_rules(items: &[RuleItem], as_of: &str) -> (Vec<RuleItem>, Vec<String>) {
    let mut active = Vec::new();
    let mut dropped = Vec::new();
    for it in items {
        if is_effective(it, as_of) {
            active.push(it.clone());
        } else {
            dropped.push(it.id.clone());
        }
    }
    (active, dropped)
}

/// 汇总规则集的费率（用于口径计算）：rate 之和；缺失项跳过并记录。
pub fn sum_rate(items: &[RuleItem]) -> (Scalar, Vec<String>) {
    let mut acc = Scalar::Num(0.0);
    let mut missing = Vec::new();
    for it in items {
        match it.rate.get() {
            Some(v) => acc = acc.map2(Scalar::Num(v), |a, b| Scalar::Num(a + b)),
            None => missing.push(it.id.clone()),
        }
    }
    (acc, missing)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_status_hard_fails() {
        let h = SlotHealth { status: SlotStatus::Missing, coverage: 0.99, gate: 0.80 };
        assert!(coverage_gate(h).is_err());
    }

    #[test]
    fn coverage_below_gate_fails() {
        let h = SlotHealth { status: SlotStatus::Active, coverage: 0.79, gate: 0.80 };
        assert!(coverage_gate(h).is_err());
    }

    #[test]
    fn coverage_at_gate_passes() {
        let h = SlotHealth { status: SlotStatus::Active, coverage: 0.80, gate: 0.80 };
        assert!(coverage_gate(h).is_ok());
    }

    #[test]
    fn affiliate_slot_blocks_net_contrib() {
        // slot.affiliate 状态 MISSING ⇒ 依赖它的 net_contrib 必须跳过
        let slots = vec![
            SlotHealth { status: SlotStatus::Active, coverage: 0.95, gate: 0.80 },
            SlotHealth { status: SlotStatus::Missing, coverage: 0.0, gate: 0.80 },
        ];
        let err = dependencies_ok(&slots).unwrap_err();
        assert_eq!(err.len(), 1);
        assert_eq!(err[0].0, 1);
    }

    #[test]
    fn rule_effective_window() {
        let it = RuleItem {
            id: "platform_commission".into(),
            name: "平台佣金".into(),
            rate: Scalar::Num(0.10),
            flat_per_order: Scalar::Missing,
            vat_included: false,
            effective_from: "2026-07-01".into(),
            effective_to: String::new(),
        };
        assert!(!is_effective(&it, "2026-06-30"));
        assert!(is_effective(&it, "2026-07-01"));
        assert!(is_effective(&it, "2026-12-31"));
    }

    #[test]
    fn rule_effective_upper_bound_exclusive() {
        let it = RuleItem {
            id: "x".into(), name: "x".into(),
            rate: Scalar::Num(0.05), flat_per_order: Scalar::Missing,
            vat_included: false,
            effective_from: "2026-01-01".into(), effective_to: "2026-06-01".into(),
        };
        assert!(is_effective(&it, "2026-05-31"));
        assert!(!is_effective(&it, "2026-06-01")); // 上界不含
    }

    #[test]
    fn sum_rate_skips_flat_items() {
        // 只有 rate 项参与求和；flat_per_order 项不参与
        let items = vec![
            RuleItem { id: "a".into(), name: "a".into(), rate: Scalar::Num(0.03),
                flat_per_order: Scalar::Missing, vat_included: false,
                effective_from: String::new(), effective_to: String::new() },
            RuleItem { id: "b".into(), name: "b".into(), rate: Scalar::Num(0.10),
                flat_per_order: Scalar::Missing, vat_included: false,
                effective_from: "2026-07-01".into(), effective_to: String::new() },
            RuleItem { id: "infra".into(), name: "基础设施费".into(), rate: Scalar::Missing,
                flat_per_order: Scalar::Num(1.0), vat_included: false,
                effective_from: String::new(), effective_to: String::new() },
        ];
        let (sum, missing) = sum_rate(&items);
        assert!((sum.get().unwrap() - 0.13).abs() < 1e-9);
        assert_eq!(missing, vec!["infra".to_string()]);
    }
}
