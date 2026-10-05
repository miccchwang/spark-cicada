#!/usr/bin/env node
/**
 * scripts/build.mjs —— 用 esbuild 打包 web 前端。
 *
 * 产物：dist/app.js（ESM bundle）+ dist/index.html（最小宿主页）。
 *
 * 纪律：本脚本不写任何密钥；身份由宿主页面/网关注入。
 */

import * as esbuild from "esbuild";
import { mkdirSync, writeFileSync, cpSync, existsSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = fileURLToPath(new URL("..", import.meta.url));
const DIST = join(ROOT, "dist");

mkdirSync(DIST, { recursive: true });

await esbuild.build({
  entryPoints: [join(ROOT, "src", "main.ts")],
  outfile: join(DIST, "app.js"),
  bundle: true,
  format: "esm",
  platform: "browser",
  target: ["es2022"],
  sourcemap: true,
  minify: false, // 内部工具优先可读性
  logLevel: "info",
});

// 样式（如存在）
const css = join(ROOT, "src", "styles.css");
if (existsSync(css)) cpSync(css, join(DIST, "styles.css"));

// 最小宿主页
const html = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>spark-cicada · 经营数据平台</title>
${existsSync(css) ? '<link rel="stylesheet" href="./styles.css" />' : ""}
</head>
<body>
<div id="app"></div>
<script type="module" src="./app.js"></script>
</body>
</html>
`;
writeFileSync(join(DIST, "index.html"), html, "utf8");

console.log("✓ 构建完成 → web/dist/");
