//! 可空标量 —— 计算内核的唯一数值载体。
//!
//! 设计纪律（docs/01 §5、docs/03、闸门 G3）：
//!   * `Missing` 与 `Num(0.0)` 是**两个不同**的东西，绝不可混为一谈。
//!   * 缺失只能沿依赖链向上传播为 `Missing`，**不得**被 0 / 均值 / 摊分替代。
//!   * 任何"分母为 0"等无定义情形返回 `Missing`（而非 0），
//!     以免被误读为"零值"（如毛利率 0%）。

use std::fmt;

/// 可空标量：要么有确定数值，要么缺失。
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Scalar {
    /// 无数据（未采集 / 覆盖率不达标 / 依赖缺失）
    Missing,
    /// 有确定数值（注意：0.0 也是"有值"）
    Num(f64),
}

impl Scalar {
    /// 是否有值。
    pub fn is_set(self) -> bool {
        matches!(self, Scalar::Num(_))
    }

    /// 取值；缺失返回 None。
    pub fn get(self) -> Option<f64> {
        match self {
            Scalar::Num(v) => Some(v),
            Scalar::Missing => None,
        }
    }

    /// 从 Option 构造。
    pub fn from_opt(v: Option<f64>) -> Self {
        match v {
            Some(x) if x.is_finite() => Scalar::Num(x),
            _ => Scalar::Missing,
        }
    }

    /// 二元运算：任一缺失 ⇒ 结果缺失（fail-closed 传播）。
    pub fn map2<F>(self, other: Scalar, f: F) -> Scalar
    where
        F: FnOnce(f64, f64) -> Scalar,
    {
        match (self, other) {
            (Scalar::Num(a), Scalar::Num(b)) => f(a, b),
            _ => Scalar::Missing,
        }
    }

    /// 安全除法：任一缺失或分母为 0 ⇒ 缺失（不返回 0）。
    pub fn safe_div(self, denom: Scalar) -> Scalar {
        match (self, denom) {
            (Scalar::Num(a), Scalar::Num(b)) if b != 0.0 => Scalar::Num(a / b),
            _ => Scalar::Missing,
        }
    }
}

impl fmt::Display for Scalar {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Scalar::Missing => write!(f, "—"),
            Scalar::Num(v) => write!(f, "{v}"),
        }
    }
}

/// 银行家舍入（四舍六入五成双）—— 承袭 KODP 财务口径教训，避免 0.5 偏置累积。
pub fn banker_round(v: f64, decimals: u32) -> f64 {
    let m = 10f64.powi(decimals as i32);
    let x = v * m;
    let r = x.round();
    // 若恰好 .5，则向偶数靠
    let diff = (x - x.trunc()).abs();
    let out = if (diff - 0.5).abs() < f64::EPSILON {
        let t = x.trunc();
        if t % 2.0 == 0.0 { t } else { t + x.signum() }
    } else {
        r
    };
    out / m
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_is_not_zero() {
        assert_ne!(Scalar::Missing, Scalar::Num(0.0));
        assert!(!Scalar::Missing.is_set());
        assert!(Scalar::Num(0.0).is_set());
        assert_eq!(Scalar::Num(0.0).get(), Some(0.0));
    }

    #[test]
    fn propagation_is_fail_closed() {
        // 任一缺失 ⇒ 结果缺失，绝不把缺失当 0
        let r = Scalar::Num(100.0).map2(Scalar::Missing, |a, b| Scalar::Num(a - b));
        assert_eq!(r, Scalar::Missing);
    }

    #[test]
    fn safe_div_by_zero_is_missing_not_zero() {
        // 毛利率 5/0 必须返回 Missing，而不是 0（否则被误读为"零毛利"）
        let r = Scalar::Num(5.0).safe_div(Scalar::Num(0.0));
        assert_eq!(r, Scalar::Missing);
    }

    #[test]
    fn safe_div_normal() {
        let r = Scalar::Num(50.0).safe_div(Scalar::Num(200.0));
        assert_eq!(r.get(), Some(0.25));
    }

    #[test]
    fn banker_rounding() {
        assert_eq!(banker_round(2.5, 0), 2.0);   // 五成双 → 偶数
        assert_eq!(banker_round(3.5, 0), 4.0);   // 五成双 → 偶数
        assert_eq!(banker_round(2.675, 2), 2.67); // 浮点近似下向偶
        assert_eq!(banker_round(1.4, 0), 1.0);
    }
}
