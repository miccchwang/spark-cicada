//! 公式求值器（M-ALGO 核心）。
//!
//! 支持 `algorithms/*.yaml` 中出现的公式形态：
//!   - 标识符（引用数据槽 / 上游算法结果）：`rev`, `cogs`, `gp`, `overhead_alloc`
//!   - 数字字面量：`0`, `1`, `0.075`, `12`
//!   - 二元运算：`+ - * /`
//!   - 括号：`( )`
//!   - 条件表达式：`rev == 0 ? null : gp / rev`
//!   - 比较：`== != < <= > >=`
//!   - 字面量：`null` / `true` / `false`
//!
//! **纪律**：本求值器是**唯一**的数值法则执行点。Go 侧永不实现公式。
//! 缺失语义：任一操作数为 `Missing` ⇒ 结果 `Missing`（除法分母为 0 同）。
//! 条件表达式特例：条件为 `Missing` ⇒ 整体 `Missing`（不猜测分支）。

use crate::scalar::Scalar;

#[derive(Debug, Clone, PartialEq)]
enum Tok {
    Ident(String),
    Num(f64),
    Null,
    True,
    False,
    Plus,
    Minus,
    Star,
    Slash,
    LParen,
    RParen,
    Question,
    Colon,
    Eq,
    Ne,
    Lt,
    Le,
    Gt,
    Ge,
}

fn lex(src: &str) -> Result<Vec<Tok>, String> {
    let mut out = Vec::new();
    let cs: Vec<char> = src.chars().collect();
    let mut i = 0;
    while i < cs.len() {
        let c = cs[i];
        match c {
            ' ' | '\t' | '\n' | '\r' => i += 1,
            '+' => { out.push(Tok::Plus); i += 1; }
            '-' => { out.push(Tok::Minus); i += 1; }
            '*' => { out.push(Tok::Star); i += 1; }
            '/' => { out.push(Tok::Slash); i += 1; }
            '(' => { out.push(Tok::LParen); i += 1; }
            ')' => { out.push(Tok::RParen); i += 1; }
            '?' => { out.push(Tok::Question); i += 1; }
            ':' => { out.push(Tok::Colon); i += 1; }
            '=' => {
                if i + 1 < cs.len() && cs[i + 1] == '=' { out.push(Tok::Eq); i += 2; }
                else { return Err(format!("unexpected '=' at {i}")); }
            }
            '!' => {
                if i + 1 < cs.len() && cs[i + 1] == '=' { out.push(Tok::Ne); i += 2; }
                else { return Err(format!("unexpected '!' at {i}")); }
            }
            '<' => {
                if i + 1 < cs.len() && cs[i + 1] == '=' { out.push(Tok::Le); i += 2; }
                else { out.push(Tok::Lt); i += 1; }
            }
            '>' => {
                if i + 1 < cs.len() && cs[i + 1] == '=' { out.push(Tok::Ge); i += 2; }
                else { out.push(Tok::Gt); i += 1; }
            }
            d if d.is_ascii_digit() || d == '.' => {
                let start = i;
                while i < cs.len() && (cs[i].is_ascii_digit() || cs[i] == '.') { i += 1; }
                let s: String = cs[start..i].iter().collect();
                let v: f64 = s.parse().map_err(|_| format!("bad number {s:?}"))?;
                out.push(Tok::Num(v));
            }
            a if a.is_alphabetic() || a == '_' => {
                let start = i;
                while i < cs.len() && (cs[i].is_alphanumeric() || cs[i] == '_') { i += 1; }
                let s: String = cs[start..i].iter().collect();
                match s.as_str() {
                    "null" => out.push(Tok::Null),
                    "true" => out.push(Tok::True),
                    "false" => out.push(Tok::False),
                    _ => out.push(Tok::Ident(s)),
                }
            }
            other => return Err(format!("unexpected char {other:?}")),
        }
    }
    Ok(out)
}

/// 表达式 AST。
#[derive(Debug, Clone)]
pub enum Expr {
    Ident(String),
    Num(f64),
    Null,
    Bool(bool),
    Bin(char, Box<Expr>, Box<Expr>),
    Cmp(&'static str, Box<Expr>, Box<Expr>),
    Cond(Box<Expr>, Box<Expr>, Box<Expr>),
    Neg(Box<Expr>),
}

struct Parser {
    toks: Vec<Tok>,
    pos: usize,
}

impl Parser {
    fn peek(&self) -> Option<&Tok> { self.toks.get(self.pos) }
    fn next(&mut self) -> Option<Tok> { let t = self.toks.get(self.pos).cloned(); self.pos += 1; t }
    fn eat(&mut self, t: &Tok) -> bool { if self.peek() == Some(t) { self.pos += 1; true } else { false } }

    fn parse_expr(&mut self) -> Result<Expr, String> {
        let cond = self.parse_cmp()?;
        if self.eat(&Tok::Question) {
            let a = self.parse_expr()?;
            if !self.eat(&Tok::Colon) { return Err("expected ':'".into()); }
            let b = self.parse_expr()?;
            return Ok(Expr::Cond(Box::new(cond), Box::new(a), Box::new(b)));
        }
        Ok(cond)
    }

    fn parse_cmp(&mut self) -> Result<Expr, String> {
        let mut lhs = self.parse_add()?;
        loop {
            let op = match self.peek() {
                Some(Tok::Eq) => "==", Some(Tok::Ne) => "!=",
                Some(Tok::Lt) => "<", Some(Tok::Le) => "<=",
                Some(Tok::Gt) => ">", Some(Tok::Ge) => ">=",
                _ => break,
            };
            self.pos += 1;
            let rhs = self.parse_add()?;
            lhs = Expr::Cmp(op, Box::new(lhs), Box::new(rhs));
        }
        Ok(lhs)
    }

    fn parse_add(&mut self) -> Result<Expr, String> {
        let mut lhs = self.parse_mul()?;
        loop {
            let op = match self.peek() { Some(Tok::Plus) => '+', Some(Tok::Minus) => '-', _ => break };
            self.pos += 1;
            let rhs = self.parse_mul()?;
            lhs = Expr::Bin(op, Box::new(lhs), Box::new(rhs));
        }
        Ok(lhs)
    }

    fn parse_mul(&mut self) -> Result<Expr, String> {
        let mut lhs = self.parse_unary()?;
        loop {
            let op = match self.peek() { Some(Tok::Star) => '*', Some(Tok::Slash) => '/', _ => break };
            self.pos += 1;
            let rhs = self.parse_unary()?;
            lhs = Expr::Bin(op, Box::new(lhs), Box::new(rhs));
        }
        Ok(lhs)
    }

    fn parse_unary(&mut self) -> Result<Expr, String> {
        if self.eat(&Tok::Minus) {
            return Ok(Expr::Neg(Box::new(self.parse_unary()?)));
        }
        self.parse_primary()
    }

    fn parse_primary(&mut self) -> Result<Expr, String> {
        match self.next() {
            Some(Tok::Num(v)) => Ok(Expr::Num(v)),
            Some(Tok::Null) => Ok(Expr::Null),
            Some(Tok::True) => Ok(Expr::Bool(true)),
            Some(Tok::False) => Ok(Expr::Bool(false)),
            Some(Tok::Ident(s)) => Ok(Expr::Ident(s)),
            Some(Tok::LParen) => {
                let e = self.parse_expr()?;
                if !self.eat(&Tok::RParen) { return Err("expected ')'".into()); }
                Ok(e)
            }
            other => Err(format!("unexpected token {other:?}")),
        }
    }
}

/// 编译公式为 AST。
pub fn compile(formula: &str) -> Result<Expr, String> {
    let toks = lex(formula)?;
    let mut p = Parser { toks, pos: 0 };
    let e = p.parse_expr()?;
    if p.pos != p.toks.len() {
        return Err(format!("trailing tokens at {}", p.pos));
    }
    Ok(e)
}

/// 变量解析器：把标识符映射为标量。
pub trait Env {
    fn lookup(&self, name: &str) -> Scalar;
}

impl<F> Env for F where F: Fn(&str) -> Scalar {
    fn lookup(&self, name: &str) -> Scalar { self(name) }
}

/// 求值：缺失沿依赖链传播（fail-closed）。
pub fn eval(e: &Expr, env: &dyn Env) -> Scalar {
    match e {
        Expr::Num(v) => Scalar::Num(*v),
        Expr::Null => Scalar::Missing,
        Expr::Bool(b) => Scalar::Num(if *b { 1.0 } else { 0.0 }),
        Expr::Ident(n) => env.lookup(n),
        Expr::Neg(x) => match eval(x, env) {
            Scalar::Num(v) => Scalar::Num(-v),
            Scalar::Missing => Scalar::Missing,
        },
        Expr::Cond(c, a, b) => match eval(c, env) {
            Scalar::Missing => Scalar::Missing, // 条件未知 ⇒ 不猜分支
            Scalar::Num(v) => if v != 0.0 { eval(a, env) } else { eval(b, env) },
        },
        Expr::Cmp(op, l, r) => {
            let lv = eval(l, env);
            let rv = eval(r, env);
            match (lv.get(), rv.get()) {
                (Some(x), Some(y)) => {
                    let t = match *op {
                        "==" => x == y, "!=" => x != y,
                        "<" => x < y, "<=" => x <= y,
                        ">" => x > y, ">=" => x >= y,
                        _ => false,
                    };
                    Scalar::Num(if t { 1.0 } else { 0.0 })
                }
                _ => Scalar::Missing,
            }
        }
        Expr::Bin(op, l, r) => {
            let lv = eval(l, env);
            let rv = eval(r, env);
            match *op {
                '+' => lv.map2(rv, |a, b| Scalar::Num(a + b)),
                '-' => lv.map2(rv, |a, b| Scalar::Num(a - b)),
                '*' => lv.map2(rv, |a, b| Scalar::Num(a * b)),
                '/' => lv.safe_div(rv), // 分母 0 ⇒ Missing
                _ => Scalar::Missing,
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::scalar::Scalar;

    fn env_from(pairs: Vec<(&'static str, Scalar)>) -> impl Fn(&str) -> Scalar {
        move |n: &str| pairs.iter().find(|(k, _)| *k == n).map(|(_, v)| *v).unwrap_or(Scalar::Missing)
    }

    #[test]
    fn gp_formula() {
        let e = compile("rev - cogs").unwrap();
        let env = env_from(vec![("rev", Scalar::Num(100.0)), ("cogs", Scalar::Num(40.0))]);
        assert_eq!(eval(&e, &env).get(), Some(60.0));
    }

    #[test]
    fn gp_skips_when_cogs_missing() {
        // algo.gp 的 missing_policy=skip：cogs 缺失 ⇒ 结果缺失（不补 0）
        let e = compile("rev - cogs").unwrap();
        let env = env_from(vec![("rev", Scalar::Num(100.0)), ("cogs", Scalar::Missing)]);
        assert_eq!(eval(&e, &env), Scalar::Missing);
    }

    #[test]
    fn gmp_conditional_zero_denominator_is_null() {
        // algorithms/gmp.yaml: "rev == 0 ? null : gp / rev"
        let e = compile("rev == 0 ? null : gp / rev").unwrap();
        let env = env_from(vec![
            ("rev", Scalar::Num(0.0)),
            ("gp", Scalar::Num(30.0)),
        ]);
        // rev==0 ⇒ 命中 null 分支 ⇒ Missing（不是 0）
        assert_eq!(eval(&e, &env), Scalar::Missing);
    }

    #[test]
    fn gmp_normal() {
        let e = compile("rev == 0 ? null : gp / rev").unwrap();
        let env = env_from(vec![("rev", Scalar::Num(200.0)), ("gp", Scalar::Num(50.0))]);
        assert_eq!(eval(&e, &env).get(), Some(0.25));
    }

    #[test]
    fn cogs_formula_with_multiplication() {
        let e = compile("sum(qty * cost_unit)").unwrap_err();
        // sum() 不在本内核支持范围（聚合由 SQL 承担）—— 应显式失败而非静默
        assert!(e.contains("unexpected") || e.contains("trailing"));
    }

    #[test]
    fn net_contrib_skips_when_affiliate_missing() {
        // algo.net_contrib: "cm2 - overhead_alloc"，但依赖 slot.affiliate(MISSING)
        // 由上层在缺失槽时直接跳过；此处验证公式本身缺失传播
        let e = compile("cm2 - overhead_alloc").unwrap();
        let env = env_from(vec![("cm2", Scalar::Num(80.0)), ("overhead_alloc", Scalar::Missing)]);
        assert_eq!(eval(&e, &env), Scalar::Missing);
    }
}
