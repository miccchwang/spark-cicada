//! spark-cicada 计算内核（Rust）
//!
//! 职责边界（docs/01 §4.1）：
//!   * **只计算**：公式求值、规则求值、覆盖率门控、批量聚合。
//!   * **不碰** HTTP 编排 / OAuth / 鉴权 / 数据库连接池（那是 Go 的事）。
//!
//! 供两种调用方式复用同一套逻辑：
//!   * gRPC 服务（跨进程，Go ↔ Rust）
//!   * WASM（前端滑块即时重算，契约一致，无口径漂移）

pub mod scalar;
pub mod formula;
pub mod gate;

/// 内核版本（供 M-ADMIN 集成控制面展示，与 Cargo 版本对齐）。
pub const KERNEL_VERSION: &str = env!("CARGO_PKG_VERSION");

/// 便捷入口：按公式 + 变量表求值。任一变量缺失按 fail-closed 传播。
///
/// 这是 Go 侧经 gRPC 调用的核心逻辑（对应 `Compute.EvalAlgo`）。
pub fn eval_formula(
    formula: &str,
    pairs: &[(&str, scalar::Scalar)],
) -> Result<scalar::Scalar, String> {
    let ast = formula::compile(formula)?;
    let env = move |name: &str| -> scalar::Scalar {
        pairs
            .iter()
            .find(|(k, _)| *k == name)
            .map(|(_, v)| *v)
            .unwrap_or(scalar::Scalar::Missing)
    };
    Ok(formula::eval(&ast, &env))
}

#[cfg(test)]
mod tests {
    use super::*;
    use scalar::Scalar;

    #[test]
    fn end_to_end_gp() {
        let r = eval_formula("rev - cogs", &[
            ("rev", Scalar::Num(1000.0)),
            ("cogs", Scalar::Num(620.0)),
        ]).unwrap();
        assert_eq!(r.get(), Some(380.0));
    }

    #[test]
    fn end_to_end_missing_propagates() {
        let r = eval_formula("rev - cogs", &[
            ("rev", Scalar::Num(1000.0)),
            // cogs 完全不在表里 = 缺失
        ]).unwrap();
        assert_eq!(r, Scalar::Missing);
    }
}
