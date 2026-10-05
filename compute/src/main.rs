//! spark-compute CLI —— 计算内核的独立可执行入口。
//!
//! 用途：
//!   * `--selftest`：跑内置自检（不依赖 Rust 测试框架，便于 CI 快速验证）。
//!   * `--eval`：从 stdin 读 JSON 请求，输出 JSON 结果（供 Go 侧子进程模式调用，
//!     在 gRPC 尚未就绪时作为可用的降级通道；两者的计算逻辑完全一致）。
//!   * `--health`：输出内核版本与就绪状态。
//!
//! gRPC 服务（tonic）在后续里程碑接入；本 CLI 保证「计算逻辑」先可被验证与复用。

use std::io::Read;

use spark_compute::gate::{coverage_gate, SlotHealth, SlotStatus};
use spark_compute::scalar::Scalar;
use spark_compute::{eval_formula, KERNEL_VERSION};

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let mode = args.get(1).map(|s| s.as_str()).unwrap_or("--health");

    match mode {
        "--health" => {
            println!(
                "{{\"kernel_version\":\"{}\",\"ready\":true}}",
                KERNEL_VERSION
            );
        }
        "--selftest" => {
            std::process::exit(run_selftest());
        }
        "--eval" => {
            let mut buf = String::new();
            if std::io::stdin().read_to_string(&mut buf).is_err() {
                eprintln!("failed to read stdin");
                std::process::exit(2);
            }
            match run_eval(&buf) {
                Ok(out) => println!("{out}"),
                Err(e) => {
                    eprintln!("{{\"error\":{}}}", json_str(&e));
                    std::process::exit(3);
                }
            }
        }
        other => {
            eprintln!("unknown mode {other:?}; use --health | --selftest | --eval");
            std::process::exit(1);
        }
    }
}

/// 自检：覆盖缺失语义 / 覆盖率门控 / 规则生效 / 银行家舍入等关键纪律。
/// 返回 0 = 全通过；非 0 = 失败数。
fn run_selftest() -> i32 {
    let mut failed = 0;
    let mut check = |name: &str, ok: bool| {
        if ok {
            println!("  ok   {name}");
        } else {
            println!("  FAIL {name}");
            failed += 1;
        }
    };

    println!("spark-compute selftest (kernel {KERNEL_VERSION})");

    // 1. GP 正常
    let gp = eval_formula("rev - cogs", &[("rev", Scalar::Num(100.0)), ("cogs", Scalar::Num(40.0))]).unwrap();
    check("gp = rev - cogs", gp.get() == Some(60.0));

    // 2. GP 依赖缺失 ⇒ 缺失（不补 0）
    let gp_missing = eval_formula("rev - cogs", &[("rev", Scalar::Num(100.0))]).unwrap();
    check("gp skips when cogs missing", gp_missing == Scalar::Missing);

    // 3. GMP 分母为 0 ⇒ null（不是 0）
    let gmp0 = eval_formula("rev == 0 ? null : gp / rev", &[("rev", Scalar::Num(0.0)), ("gp", Scalar::Num(30.0))]).unwrap();
    check("gmp(rev=0) = null not 0", gmp0 == Scalar::Missing);

    // 4. GMP 正常
    let gmp = eval_formula("rev == 0 ? null : gp / rev", &[("rev", Scalar::Num(200.0)), ("gp", Scalar::Num(50.0))]).unwrap();
    check("gmp = 0.25", gmp.get() == Some(0.25));

    // 5. 覆盖率门控：MISSING 硬淘汰
    let blocked = coverage_gate(SlotHealth { status: SlotStatus::Missing, coverage: 0.99, gate: 0.80 });
    check("coverage_gate rejects MISSING slot", blocked.is_err());

    // 6. 覆盖率门控：0.79 < 0.80 淘汰
    let below = coverage_gate(SlotHealth { status: SlotStatus::Active, coverage: 0.79, gate: 0.80 });
    check("coverage_gate rejects below gate", below.is_err());

    // 7. 覆盖率门控：0.80 == 0.80 通过
    let at = coverage_gate(SlotHealth { status: SlotStatus::Active, coverage: 0.80, gate: 0.80 });
    check("coverage_gate passes at gate", at.is_ok());

    // 8. 净贡献：affiliate 缺失 ⇒ 依赖失败
    let deps = spark_compute::gate::dependencies_ok(&[
        SlotHealth { status: SlotStatus::Active, coverage: 0.95, gate: 0.80 },
        SlotHealth { status: SlotStatus::Missing, coverage: 0.0, gate: 0.80 },
    ]);
    check("net_contrib blocked by missing affiliate", deps.is_err());

    // 9. 银行家舍入
    check("banker_round(2.5)=2", spark_compute::scalar::banker_round(2.5, 0) == 2.0);
    check("banker_round(3.5)=4", spark_compute::scalar::banker_round(3.5, 0) == 4.0);

    // 10. 缺失 != 0
    check("Missing != Num(0.0)", Scalar::Missing != Scalar::Num(0.0));

    if failed == 0 {
        println!("ALL PASS");
    } else {
        println!("{failed} FAILED");
    }
    failed
}

/// 从 JSON 请求求值。
/// 请求：{"formula":"rev - cogs","vars":{"rev":100,"cogs":null}}
/// null / 缺字段 ⇒ Missing。
fn run_eval(input: &str) -> Result<String, String> {
    let v: serde_json::Value = serde_json::from_str(input).map_err(|e| e.to_string())?;
    let formula = v.get("formula").and_then(|x| x.as_str()).ok_or("missing 'formula'")?;
    let vars = v.get("vars");

    let mut pairs: Vec<(String, Scalar)> = Vec::new();
    if let Some(obj) = vars.and_then(|x| x.as_object()) {
        for (k, val) in obj {
            let s = match val {
                serde_json::Value::Number(n) => Scalar::from_opt(n.as_f64()),
                serde_json::Value::Null => Scalar::Missing,
                _ => return Err(format!("var {k:?} must be number or null")),
            };
            pairs.push((k.clone(), s));
        }
    }
    let refs: Vec<(&str, Scalar)> = pairs.iter().map(|(k, v)| (k.as_str(), *v)).collect();
    let r = eval_formula(formula, &refs)?;

    let out = match r.get() {
        Some(x) => format!("{{\"result\":{x},\"skipped\":false}}"),
        None => "{\"result\":null,\"skipped\":true,\"reason\":\"missing dependency\"}".to_string(),
    };
    Ok(out)
}

fn json_str(s: &str) -> String {
    let mut o = String::from("\"");
    for c in s.chars() {
        match c {
            '"' => o.push_str("\\\""),
            '\\' => o.push_str("\\\\"),
            '\n' => o.push_str("\\n"),
            _ => o.push(c),
        }
    }
    o.push('"');
    o
}
