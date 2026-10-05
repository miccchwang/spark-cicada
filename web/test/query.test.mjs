/**
 * test/query.test.mjs —— M-QUERY 客户端断言
 *
 * 重点（fail-closed）：
 *   * 后端 500 "store not configured" → 必须抛出 STORE_NOT_CONFIGURED，
 *     **绝不**返回空 DataContract（否则 UI 会把「没数据」当「数据为 0」）。
 *   * 桶 STALE → BUCKET_NOT_FRESH。
 *   * 403 → FORBIDDEN。401 → UNAUTHENTICATED。
 *   * 版本不符 → PROTOCOL_MISMATCH。
 *   * 网络异常 → NETWORK。
 *   * 成功路径缓存生效。
 */

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { existsSync, mkdirSync } from "node:fs";
import { pathToFileURL, fileURLToPath } from "node:url";

// 本文件位于 web/test/，故 ".." 即 web/。
const WEB = fileURLToPath(new URL("..", import.meta.url));
const OUT = join(WEB, ".test-build");

let Q;

before(async () => {
  const esbuild = await import("esbuild");
  if (!existsSync(OUT)) mkdirSync(OUT, { recursive: true });
  await esbuild.build({
    entryPoints: [join(WEB, "src", "query", "client.ts")],
    outfile: join(OUT, "query.mjs"),
    format: "esm",
    bundle: true,
    platform: "neutral",
  });
  Q = await import(pathToFileURL(join(OUT, "query.mjs")).href);
});

/** 构造一个可控 fetch 响应。 */
function stubFetch(status, body, { throws = false } = {}) {
  return async (_url, _init) => {
    if (throws) throw new Error("connection refused");
    return {
      ok: status >= 200 && status < 300,
      status,
      async text() {
        return body;
      },
    };
  };
}

const OK_DC = JSON.stringify({
  v: "1.0",
  queryHash: "h",
  rows: [],
  columns: [],
  aggregates: {},
  levels: [],
  algoTrace: [],
  gaps: [],
  precomputed: true,
  generatedAt: "2026-10-05T00:00:00Z",
});

const QS = { v: "1.0", time: { mode: "preset", preset: "mtd", grain: "month", timezone: "Asia/Bangkok" }, filters: [], dims: { level: "store" } };

test("降级模式：500 store not configured → STORE_NOT_CONFIGURED（不返回空数据）", async () => {
  const c = new Q.QueryClient({ fetchImpl: stubFetch(500, "query failed: query: store not configured") });
  await assert.rejects(
    () => c.run(QS),
    (e) => {
      assert.ok(e instanceof Q.QueryError);
      assert.equal(e.reason, "STORE_NOT_CONFIGURED");
      assert.equal(e.status, 500);
      return true;
    },
  );
});

test("桶非 FRESH：500 bucket state=STALE → BUCKET_NOT_FRESH", async () => {
  const c = new Q.QueryClient({ fetchImpl: stubFetch(500, "no usable precompute bucket: bucket pnl_month state=STALE") });
  await assert.rejects(() => c.run(QS), (e) => e.reason === "BUCKET_NOT_FRESH");
});

test("403 → FORBIDDEN", async () => {
  const c = new Q.QueryClient({ fetchImpl: stubFetch(403, "unknown account") });
  await assert.rejects(() => c.run(QS), (e) => e.reason === "FORBIDDEN");
});

test("401 → UNAUTHENTICATED", async () => {
  const c = new Q.QueryClient({ fetchImpl: stubFetch(401, "missing identity") });
  await assert.rejects(() => c.run(QS), (e) => e.reason === "UNAUTHENTICATED");
});

test("400 → MALFORMED_REQUEST", async () => {
  const c = new Q.QueryClient({ fetchImpl: stubFetch(400, "invalid QueryState") });
  await assert.rejects(() => c.run(QS), (e) => e.reason === "MALFORMED_REQUEST");
});

test("版本不符 → PROTOCOL_MISMATCH（拒绝猜测）", async () => {
  const bad = JSON.stringify({ v: "2.0", rows: [] });
  const c = new Q.QueryClient({ fetchImpl: stubFetch(200, bad) });
  await assert.rejects(() => c.run(QS), (e) => e.reason === "PROTOCOL_MISMATCH");
});

test("响应非 JSON → PROTOCOL_MISMATCH", async () => {
  const c = new Q.QueryClient({ fetchImpl: stubFetch(200, "<html>oops</html>") });
  await assert.rejects(() => c.run(QS), (e) => e.reason === "PROTOCOL_MISMATCH");
});

test("网络异常 → NETWORK（不吞异常）", async () => {
  const c = new Q.QueryClient({ fetchImpl: stubFetch(200, "", { throws: true }) });
  await assert.rejects(() => c.run(QS), (e) => e.reason === "NETWORK");
});

test("成功路径：返回 DataContract 且缓存生效", async () => {
  let calls = 0;
  const counting = async () => {
    calls++;
    return { ok: true, status: 200, async text() { return OK_DC; } };
  };
  const c = new Q.QueryClient({ fetchImpl: counting });
  const a = await c.run(QS, "key1");
  const b = await c.run(QS, "key1");
  assert.equal(a.v, "1.0");
  assert.equal(b, a, "相同 cacheKey 应命中缓存");
  assert.equal(calls, 1, "第二次不应再发请求");
  c.clearCache();
  await c.run(QS, "key1");
  assert.equal(calls, 2, "清缓存后应重新请求");
});

test("身份经请求头注入，不写入 URL（G11）", async () => {
  let seenUrl = "";
  let seenHeaders = {};
  const spy = async (url, init) => {
    seenUrl = String(url);
    seenHeaders = init?.headers ?? {};
    return { ok: true, status: 200, async text() { return OK_DC; } };
  };
  const c = new Q.QueryClient({ fetchImpl: spy, account: "lead.sea", baseUrl: "http://x" });
  await c.run(QS);
  assert.equal(seenHeaders["X-Spark-Account"], "lead.sea", "身份应走请求头");
  assert.ok(!seenUrl.includes("lead.sea"), "★ 身份不得出现在 URL 中");
});

test("toRequestBody 只做搬运（不含业务字段计算）", () => {
  const body = Q.QueryClient.toRequestBody(QS);
  const parsed = JSON.parse(body);
  assert.equal(parsed.v, "1.0");
  assert.deepEqual(parsed.dims, { level: "store" });
});
