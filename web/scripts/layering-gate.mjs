#!/usr/bin/env node
/**
 * scripts/layering-gate.mjs —— 前端 G1/G3 静态闸门
 *
 * 把「三段解耦」从文档承诺变成**会失败的断言**（与后端 internal/gate/g1_layering.go 同思想）。
 *
 * 断言：
 *   G1-a  M-FILTER（src/filter/**）不得有任何网络调用
 *   G1-b  M-RENDER（src/render/**）不得引入 M-FILTER / QueryState
 *   G1-c  M-QUERY（src/query/**）不得含业务公式（gp/cogs/gmp/net_contrib 算术）
 *   G1-d  无反向依赖：contracts/ 不得引入 filter|render|query
 *   G3    缺失文案唯一真源：除 contracts/data-contract.ts 外不得硬编码 "待接入"
 *
 * 退出码非 0 ⇒ 阻断合并。
 */

import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";

// 必须用 fileURLToPath：URL 的 .pathname 会把路径里的空格编码成 %20
// （本仓库路径含 "TK automatic"），导致 scandir ENOENT 且退出码被误判为 0。
const ROOT = fileURLToPath(new URL("..", import.meta.url));
const SRC = join(ROOT, "src");

/** 递归收集 .ts 文件。 */
function walk(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    const st = statSync(p);
    if (st.isDirectory()) out.push(...walk(p));
    else if (name.endsWith(".ts")) out.push(p);
  }
  return out;
}

/** 去掉注释，避免注释里的词触发误报。 */
function stripComments(code) {
  return code
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/(^|[^:])\/\/.*$/gm, "$1");
}

/** 提取 import 说明符。 */
function importsOf(code) {
  const out = [];
  const re = /(?:^|\n)\s*import\s+(?:type\s+)?[\s\S]*?from\s+["']([^"']+)["']/g;
  let m;
  while ((m = re.exec(code)) !== null) out.push(m[1]);
  return out;
}

const files = walk(SRC);
const violations = [];

const rel = (p) => relative(ROOT, p).split(sep).join("/");

/** 路径分类（只按 POSIX 相对路径判断，避免平台差异）。 */
const isFilter = (r) => r.startsWith("src/filter/");
const isRender = (r) => r.startsWith("src/render/");
const isQuery = (r) => r.startsWith("src/query/");
const isContracts = (r) => r.startsWith("src/contracts/");

// 网络调用特征
const NETWORK_PATTERNS = [
  [/\bfetch\s*\(/, "fetch("],
  [/\bXMLHttpRequest\b/, "XMLHttpRequest"],
  [/\bWebSocket\b/, "WebSocket"],
  [/https?:\/\//, "http(s):// URL"],
  [/\bnavigator\.sendBeacon\b/, "navigator.sendBeacon"],
];

// 业务公式特征：这些词的算术运算
const FORMULA_WORDS = ["gp", "cogs", "gmp", "net_contrib", "rev", "revenue"];

for (const f of files) {
  const r = rel(f);
  const raw = readFileSync(f, "utf8");
  const code = stripComments(raw);
  const imps = importsOf(code);

  // ── G1-a：M-FILTER 无网络 ──
  if (isFilter(r)) {
    for (const [re, label] of NETWORK_PATTERNS) {
      if (re.test(code)) {
        violations.push(`G1-a ${r}: M-FILTER 出现网络调用「${label}」——筛选层只产出 QueryState`);
      }
    }
    if (imps.some((i) => i.includes("/query/") || i.includes("/render/"))) {
      violations.push(`G1-a ${r}: M-FILTER 引入了 query/render —— 违反单向依赖`);
    }
  }

  // ── G1-b：M-RENDER 不读 QueryState ──
  if (isRender(r)) {
    for (const i of imps) {
      if (i.includes("query-state")) {
        violations.push(`G1-b ${r}: M-RENDER 引入了 QueryState「${i}」——渲染层唯一入参是 DataContract`);
      }
      if (i.includes("/filter/")) {
        violations.push(`G1-b ${r}: M-RENDER 引入了 M-FILTER「${i}」——禁止反向依赖`);
      }
    }
    // 网络调用也不该出现在纯渲染层
    for (const [re, label] of NETWORK_PATTERNS) {
      if (re.test(code)) {
        violations.push(`G1-b ${r}: M-RENDER 出现网络调用「${label}」——渲染层不做 I/O`);
      }
    }
  }

  // ── G1-c：M-QUERY 无业务公式 ──
  if (isQuery(r)) {
    for (const w of FORMULA_WORDS) {
      // 形如 `gp = ...`、`a - b` 里出现这些词参与算术 / 或形如 `calcGp(` 的本地实现
      const assignRe = new RegExp(`\\b${w}\\b\\s*[-+*/=]`, "i");
      const funcRe = new RegExp(`function\\s+\\w*${w}\\w*\\s*\\(`, "i");
      if (assignRe.test(code) || funcRe.test(code)) {
        violations.push(`G1-c ${r}: M-QUERY 疑似实现业务公式「${w}」——数值必须由后端内核产出`);
      }
    }
    if (imps.some((i) => i.includes("/render/"))) {
      violations.push(`G1-c ${r}: M-QUERY 引入了 render —— 违反单向依赖`);
    }
  }

  // ── G1-d：contracts 不依赖上层 ──
  if (isContracts(r)) {
    for (const i of imps) {
      if (/\/?(filter|render|query)\//.test(i)) {
        violations.push(`G1-d ${r}: contracts 反向依赖了「${i}」`);
      }
    }
  }

  // ── G3：缺失文案唯一真源 ──
  if (!isContracts(r) && /["'`]待接入["'`]/.test(code)) {
    violations.push(`G3 ${r}: 硬编码了缺失文案「待接入」——应引用 contracts 的 MISSING_TEXT`);
  }
}

// ── 输出 ──
if (violations.length > 0) {
  console.error(`\n✗ 前端分层闸门失败（${violations.length} 项）：\n`);
  for (const v of violations) console.error(`  ${v}`);
  console.error("\n修改分层约束前请先改 docs/05 与 docs/01。\n");
  process.exit(1);
}

console.log(`✓ 前端分层闸门通过（检查 ${files.length} 个文件）`);
console.log("  G1-a M-FILTER 无网络   ✓");
console.log("  G1-b M-RENDER 不读 QueryState ✓");
console.log("  G1-c M-QUERY 无业务公式 ✓");
console.log("  G1-d contracts 无反向依赖 ✓");
console.log("  G3   缺失文案唯一真源   ✓");
