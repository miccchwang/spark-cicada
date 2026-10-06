/**
 * test/contracts.test.mjs —— 契约镜像与唯一真源的一致性
 *
 * 动机：web/src/contracts/*.ts 是仓库根 contracts/*.ts 的镜像。
 * 若真源改了字段而镜像没跟，前端会静默错位 —— 这是最隐蔽的漂移。
 * 本测试逐字比对**接口字段名**，不一致即失败。
 */

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

// fileURLToPath 而非 .pathname：后者会把含空格的路径编码为 %20（本仓库路径含 "TK automatic"）。
const REPO = fileURLToPath(new URL("../..", import.meta.url));
const WEB = join(REPO, "web");

/** 提取 `export interface X { ... }` 体内的字段名。 */
function interfaceFields(src, name) {
  const re = new RegExp(`export interface ${name}\\s*(?:<[^>]*>)?\\s*\\{([\\s\\S]*?)\\n\\}`);
  const m = src.match(re);
  if (!m) return null;
  const body = m[1];
  const fields = [];
  // 顶层字段：行首（可能带缩进）的 `ident?:` 或 `ident:`
  for (const line of body.split("\n")) {
    const fm = line.match(/^\s{0,4}([A-Za-z_][A-Za-z0-9_]*)\??\s*:/);
    if (fm) fields.push(fm[1]);
  }
  return fields.sort();
}

const PAIRS = [
  { dir: "query-state", ifaces: ["TimeRange", "FilterClause", "DimSelection", "OrderClause", "PageClause", "PrecomputeHint", "QueryState"] },
  { dir: "data-contract", ifaces: ["ColumnDef", "LevelSummary", "AlgoTrace", "DataGap", "DataContract"] },
  { dir: "view-template", ifaces: ["ViewTemplate", "ColumnPref", "LayoutPref"] },
];

for (const { dir, ifaces } of PAIRS) {
  test(`契约镜像一致：${dir}`, () => {
    const truth = readFileSync(join(REPO, "contracts", `${dir}.ts`), "utf8");
    const mirror = readFileSync(join(WEB, "src", "contracts", `${dir}.ts`), "utf8");
    for (const name of ifaces) {
      const a = interfaceFields(truth, name);
      const b = interfaceFields(mirror, name);
      assert.ok(a, `真源 contracts/${dir}.ts 找不到接口 ${name}`);
      assert.ok(b, `镜像 web/src/contracts/${dir}.ts 找不到接口 ${name}`);
      assert.deepEqual(
        b,
        a,
        `接口 ${name} 字段不一致\n  真源: ${a.join(",")}\n  镜像: ${b.join(",")}`,
      );
    }
  });
}

test("契约版本号一致", () => {
  const qTruth = readFileSync(join(REPO, "contracts", "query-state.ts"), "utf8");
  const qMirror = readFileSync(join(WEB, "src", "contracts", "query-state.ts"), "utf8");
  const vTruth = qTruth.match(/QUERY_STATE_VERSION\s*=\s*"([^"]+)"/)?.[1];
  const vMirror = qMirror.match(/QUERY_STATE_VERSION\s*=\s*"([^"]+)"/)?.[1];
  assert.equal(vMirror, vTruth, "QueryState 版本号漂移");

  const dTruth = readFileSync(join(REPO, "contracts", "data-contract.ts"), "utf8");
  const dMirror = readFileSync(join(WEB, "src", "contracts", "data-contract.ts"), "utf8");
  const dvTruth = dTruth.match(/DATA_CONTRACT_VERSION\s*=\s*"([^"]+)"/)?.[1];
  const dvMirror = dMirror.match(/DATA_CONTRACT_VERSION\s*=\s*"([^"]+)"/)?.[1];
  assert.equal(dvMirror, dvTruth, "DataContract 版本号漂移");

  const tTruth = readFileSync(join(REPO, "contracts", "view-template.ts"), "utf8");
  const tMirror = readFileSync(join(WEB, "src", "contracts", "view-template.ts"), "utf8");
  const tvTruth = tTruth.match(/VIEW_TEMPLATE_VERSION\s*=\s*"([^"]+)"/)?.[1];
  const tvMirror = tMirror.match(/VIEW_TEMPLATE_VERSION\s*=\s*"([^"]+)"/)?.[1];
  assert.equal(tvMirror, tvTruth, "ViewTemplate 版本号漂移");
});
