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
  { dir: "pnl", ifaces: ["CaliberMeta", "PnlLineDef", "PnlLine", "PnlStatement"] },
  { dir: "strategy-choice", ifaces: ["StrategyChoice", "ChoiceOption", "ImpactPreview", "StrategyHistoryEntry"] },
  { dir: "tenant", ifaces: ["Tenant", "TenantQuota", "TenantContext", "TenantHint"] },
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

  const pTruth = readFileSync(join(REPO, "contracts", "pnl.ts"), "utf8");
  const pMirror = readFileSync(join(WEB, "src", "contracts", "pnl.ts"), "utf8");
  const pvTruth = pTruth.match(/PNL_CONTRACT_VERSION\s*=\s*"([^"]+)"/)?.[1];
  const pvMirror = pMirror.match(/PNL_CONTRACT_VERSION\s*=\s*"([^"]+)"/)?.[1];
  assert.equal(pvMirror, pvTruth, "PnL 契约版本号漂移");

  const sTruth = readFileSync(join(REPO, "contracts", "strategy-choice.ts"), "utf8");
  const sMirror = readFileSync(join(WEB, "src", "contracts", "strategy-choice.ts"), "utf8");
  const svTruth = sTruth.match(/STRATEGY_CHOICE_VERSION\s*=\s*"([^"]+)"/)?.[1];
  const svMirror = sMirror.match(/STRATEGY_CHOICE_VERSION\s*=\s*"([^"]+)"/)?.[1];
  assert.equal(svMirror, svTruth, "StrategyChoice 契约版本号漂移");

  const tnTruth = readFileSync(join(REPO, "contracts", "tenant.ts"), "utf8");
  const tnMirror = readFileSync(join(WEB, "src", "contracts", "tenant.ts"), "utf8");
  const tenTruth = tnTruth.match(/TENANT_CONTRACT_VERSION\s*=\s*"([^"]+)"/)?.[1];
  const tenMirror = tnMirror.match(/TENANT_CONTRACT_VERSION\s*=\s*"([^"]+)"/)?.[1];
  assert.equal(tenMirror, tenTruth, "Tenant 契约版本号漂移");
});

/**
 * ★★ 多租户的 fail-closed 纪律必须在**契约层**锁死。
 *
 * 越权最常见的样子不是「绕过了权限判断」，而是有人写了个新 handler
 * 忘了带租户条件，或者查询失败时走了兜底分支 —— 而兜底分支查的是全表。
 * 若这条纪律只活在某个解析器的实现里，下一代维护者会「顺手」加一个
 * 「回退到默认租户」的便利分支。那正是本契约要禁止的。
 */
test("★ 多租户契约：解析结果必须显式区分 resolved/rejected（不得回退默认租户）", () => {
  const truth = readFileSync(join(REPO, "contracts", "tenant.ts"), "utf8");
  const mirror = readFileSync(join(WEB, "src", "contracts", "tenant.ts"), "utf8");
  for (const [name, src] of [["真源", truth], ["镜像", mirror]]) {
    // 解析结果必须是判别联合，且两种形态都在
    assert.match(
      src,
      /export type TenantResolution\s*=[\s\S]*?kind:\s*"resolved"[\s\S]*?kind:\s*"rejected"/,
      `${name} TenantResolution 必须同时含 resolved / rejected 两种形态`,
    );
    // 拒绝原因必须齐全（缺哪一个都会让某类失败变成静默放行）
    for (const reason of ["missing", "malformed", "not_found", "not_active", "quota_exceeded", "internal"]) {
      assert.match(
        src,
        new RegExp(`"${reason}"`),
        `${name} TenantRejectReason 缺少 "${reason}"`,
      );
    }
    // 提示优先级：claim 必须先于 host（host 是用户可控输入）
    const prec = src.match(/TENANT_HINT_PRECEDENCE[^=]*=\s*\[([\s\S]*?)\]/)?.[1] ?? "";
    const order = [...prec.matchAll(/"(claim|header|host|envDefault)"/g)].map((m) => m[1]);
    assert.deepEqual(
      order,
      ["claim", "header", "host", "envDefault"],
      `${name} 提示优先级必须为 claim > header > host > envDefault`,
    );
    // 两档隔离必须都在（少一档会让某类客户无法合规上线）
    assert.match(src, /"shared"/, `${name} 缺少 shared 档`);
    assert.match(src, /"dedicated"/, `${name} 缺少 dedicated 档`);
    // ★ 缓存键必须强制带租户 —— 漏租户的缓存是最隐蔽的串租
    assert.match(
      src,
      /tenantCacheKey\(\s*tenantId/,
      `${name} tenantCacheKey 必须以 tenantId 为第一参数`,
    );
    // ★ 不得出现「默认租户」类兜底。
    //   注意：这里只禁**默认租户 ID**，不禁 "DEFAULT_TENANT_QUOTA"
    //   （配额有合理默认值；租户身份没有）。
    for (const forbidden of ["defaultTenantId", "fallbackTenant", "DEFAULT_TENANT_ID"]) {
      assert.doesNotMatch(
        src,
        new RegExp(forbidden, "i"),
        `${name} 不应存在默认租户兜底 "${forbidden}" —— 解析失败必须拒绝服务`,
      );
    }
  }
});

/**
 * ★ 口径不变量必须**在契约层**就写死，而不是散在实现里。
 *
 * 「net_revenue 两口径相同」这条如果只存在于某个函数的注释里，
 * 下一代维护者会在别处复制一份「自己的口径表」——然后两处漂移。
 * 这里直接盯住三张表：CALIBER_INVARIANT_LINES / DISCOUNT_TARGET_LINE / 默认口径。
 */
test("★ P&L 口径契约：不变量行 / 折扣归属 / 模块默认（D10）", () => {
  const truth = readFileSync(join(REPO, "contracts", "pnl.ts"), "utf8");
  const mirror = readFileSync(join(WEB, "src", "contracts", "pnl.ts"), "utf8");
  for (const [name, src] of [["真源", truth], ["镜像", mirror]]) {
    assert.match(src, /CALIBER_INVARIANT_LINES[^=]*=\s*\[[^\]]*"net_revenue"/, `${name} 未把 net_revenue 列为口径不变量`);
    assert.match(src, /A:\s*"marketing"/, `${name} 口径 A 的折扣归属应为 marketing（计入营销费用）`);
    assert.match(src, /B:\s*"seller_discount"/, `${name} 口径 B 的折扣归属应为 seller_discount（收入抵减）`);
    assert.match(src, /"module\.report":\s*"A"/, `${name} 经营报表默认口径应为 A（运营口径）`);
    assert.match(src, /"module\.pnl":\s*"B"/, `${name} P&L 默认口径应为 B（财务口径，D10 分模块各自默认）`);
  }
});

/**
 * ★ 策略实验室的「不许自由输入」必须在**契约层**就锁死。
 *
 * 若这条只存在于某个 handler 的校验里，下一代维护者会觉得
 * 「加个自定义输入更灵活」——那正是需求明确禁止的（"以选型方式提供"）。
 * 这里直接盯住 DecisionAction 的联合类型：只能有 choose / keep_current。
 */
test("★ 策略契约：决策动作只有选型，绝无自由输入", () => {
  const truth = readFileSync(join(REPO, "contracts", "strategy-choice.ts"), "utf8");
  const mirror = readFileSync(join(WEB, "src", "contracts", "strategy-choice.ts"), "utf8");
  for (const [name, src] of [["真源", truth], ["镜像", mirror]]) {
    // 动作联合类型必须只含这两种
    assert.match(
      src,
      /export type DecisionAction\s*=[\s\S]*?kind:\s*"choose"[\s\S]*?kind:\s*"keep_current"/,
      `${name} DecisionAction 必须且只能包含 choose / keep_current`,
    );
    // 不得出现「填公式 / 任意数值」类动作
    for (const forbidden of ["formula", "expression", "set_value", "setValue", "rawValue"]) {
      assert.doesNotMatch(
        src,
        new RegExp(`kind:\\s*"${forbidden}"`, "i"),
        `${name} 不应存在自由输入类动作 kind: "${forbidden}"`,
      );
    }
    // 选项必须自带风险与可逆性（选之前就能看到代价）
    assert.match(src, /risk:\s*"low"\s*\|\s*"medium"\s*\|\s*"high"/, `${name} ChoiceOption 必须有 risk`);
    assert.match(src, /reversible:\s*boolean/, `${name} ChoiceOption 必须有 reversible`);
    // 候选数量契约：2–4
    assert.match(src, /候选选项 2–4 个/, `${name} 应声明候选数量为 2–4 个`);
  }
});
