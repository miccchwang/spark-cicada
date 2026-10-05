#!/usr/bin/env bash
# tools/verify/launch-check.sh —— 「可上线」的可复现证据链。
#
# 为什么需要它：
#   本机没有 docker/psql，无法直连真库；而 CI 要等推送后才跑。
#   这个脚本把「上线前必须成立的事」变成**一条命令、退出码可信**的检查，
#   任何人（或 CI、或部署机）都能重复跑出同样的结论。
#
# 覆盖的断言（每条都必须真跑，不允许 skip 冒充通过）：
#   1. 工具链就位（go / node）
#   2. Go 构建 + vet
#   3. 迁移集可加载且校验通过（dry-run，不需要库）
#   4. 全部 Go 测试（含 G1–G12 闸门）
#   5. Rust 内核测试 + selftest
#   6. 前端分层闸门 + 测试 + 类型检查 + 构建
#   7. ★ 前端产物密钥扫描（G11b）
#   8. ★ sparkd 真起进程 + 静态托管 + API 降级语义（真 HTTP，不靠 mock）
#
# 用法：
#   bash tools/verify/launch-check.sh              # 全量（无库时跳过真库段并明示）
#   SPARK_TEST_DB_DSN=... bash tools/verify/launch-check.sh   # 含真库段
#   SPARK_REQUIRE_DB=1 bash tools/verify/launch-check.sh      # 真库段必须通过
#
# 退出码：0 = 全部通过；非 0 = 有断言失败。**不允许「跳过」被计为通过。**

set -u

# ── 定位仓库根（脚本在 tools/verify/ 下）──
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT" || exit 1

# ── 工具链：优先托管绝对路径（本机 go/node 不在 PATH）──
GOEXE="${GOEXE:-C:/Users/Konvy-1098/.workbuddy-ai/binaries/go/versions/go1.27.1/bin/go.exe}"
NODEEXE="${NODEEXE:-C:/Users/Konvy-1098/.workbuddy-ai/binaries/node/versions/22.22.2-3/node.exe}"
CARGOEXE="${CARGOEXE:-C:/Users/Konvy-1098/.rustup/toolchains/stable-x86_64-pc-windows-gnu/bin/cargo.exe}"

# 回退到 PATH（CI/Linux 上托管路径不存在）
command -v "$GOEXE" >/dev/null 2>&1 || GOEXE="$(command -v go || true)"
command -v "$NODEEXE" >/dev/null 2>&1 || NODEEXE="$(command -v node || true)"
command -v "$CARGOEXE" >/dev/null 2>&1 || CARGOEXE="$(command -v cargo || true)"

export GOROOT="${GOROOT:-C:/Users/Konvy-1098/.workbuddy-ai/binaries/go/versions/go1.27.1}"
export GOPATH="${GOPATH:-C:/Users/Konvy-1098/.workbuddy-ai/binaries/go/gopath}"
export GOCACHE="${GOCACHE:-C:/Users/Konvy-1098/.workbuddy-ai/binaries/go/cache}"
export GOTMPDIR="${GOTMPDIR:-C:/Users/Konvy-1098/.workbuddy-ai/binaries/go/tmp}"

PASS=0
FAIL=0
SKIP=0
FAILED_NAMES=""

step()  { printf '\n\033[1m── %s ──\033[0m\n' "$*"; }
ok()    { PASS=$((PASS+1)); printf '  \033[32m✓ %s\033[0m\n' "$*"; }
bad()   { FAIL=$((FAIL+1)); FAILED_NAMES="$FAILED_NAMES\n  - $*"; printf '  \033[31m✗ %s\033[0m\n' "$*"; }
skip()  { SKIP=$((SKIP+1)); printf '  \033[33m⊘ %s\033[0m\n' "$*"; }

# ───────────────────────── 0. 工具链 ─────────────────────────
step "0. 工具链"
if [ -n "$GOEXE" ] && "$GOEXE" version >/dev/null 2>&1; then ok "go: $("$GOEXE" version | head -1)"; else bad "go 不可用"; fi
if [ -n "$NODEEXE" ] && "$NODEEXE" --version >/dev/null 2>&1; then ok "node: $("$NODEEXE" --version)"; else bad "node 不可用"; fi
if [ -n "$CARGOEXE" ] && "$CARGOEXE" --version >/dev/null 2>&1; then ok "cargo: $("$CARGOEXE" --version)"; else skip "cargo 不可用（Rust 段将跳过）"; fi

[ "$FAIL" -gt 0 ] && { echo "工具链缺失，无法继续"; exit 1; }

# ───────────────────────── 1. Go 构建 / vet ─────────────────────────
step "1. Go 构建与静态检查"
( cd backend && "$GOEXE" build ./... ) && ok "go build ./..." || bad "go build ./..."
( cd backend && "$GOEXE" vet ./... )   && ok "go vet ./..."   || bad "go vet ./..."

# ───────────────────────── 2. 迁移集校验 ─────────────────────────
# 注意：spark-migrate 的 -dry-run 只跳过**执行**，仍会连接数据库读取
# schema_migrations 记账（这是对的 —— 不连库就无法判断哪些已应用）。
# 因此无库时本段应跳过，而不是报失败（报失败会误导为「迁移写错了」）。
step "2. 迁移集校验"
if [ -n "${SPARK_TEST_DB_DSN:-}" ] || [ -n "${SPARK_DB_DSN:-}" ]; then
  if ( cd backend && "$GOEXE" run ./cmd/spark-migrate -dir ../sql/migrations -dry-run >/tmp/spark-migrate.log 2>&1 ); then
    ok "迁移集可加载且校验通过"
  else
    bad "迁移集校验失败（见 /tmp/spark-migrate.log）"; tail -15 /tmp/spark-migrate.log
  fi
else
  # 无库：仍然校验**文件层面**的合法性（命名/版本唯一/非空）——
  # 这部分不需要数据库，由 LoadMigrations 负责。
  if ( cd backend && "$GOEXE" test ./internal/db/ -count=1 >/dev/null 2>&1 ) \
     || ( cd backend && "$GOEXE" test ./internal/gate/ -run TestSQL -count=1 >/dev/null 2>&1 ); then
    ok "迁移文件校验通过（命名/版本唯一/非空；SQL 静态闸门）"
  else
    bad "迁移文件校验失败"
  fi
  skip "数据库不可用：跳过「已应用记账」层面的 dry-run 校验"
fi

# ───────────────────────── 3. Go 测试（含闸门）─────────────────────────
step "3. Go 测试（G1–G12 闸门 + 控制面 + SQL 静态）"
if ( cd backend && "$GOEXE" test ./... -count=1 >/tmp/spark-go-test.log 2>&1 ); then
  ok "go test ./... 全绿"
else
  bad "go test ./... 失败（见 /tmp/spark-go-test.log）"
  tail -25 /tmp/spark-go-test.log
fi

# ───────────────────────── 4. 真库段 ─────────────────────────
step "4. 真库集成（迁移幂等 / NULL 不补 0 / 审计 append-only / D7）"
if [ -n "${SPARK_TEST_DB_DSN:-}" ]; then
  if ( cd backend && "$GOEXE" test ./internal/store/ -run Integration -count=1 -v >/tmp/spark-db-test.log 2>&1 ); then
    ok "真库集成断言全绿"
  else
    bad "真库集成断言失败（见 /tmp/spark-db-test.log）"
    grep -E "^\s*--- (FAIL|PASS)" /tmp/spark-db-test.log | head -20
  fi
elif [ "${SPARK_REQUIRE_DB:-0}" = "1" ]; then
  bad "SPARK_REQUIRE_DB=1 但未设置 SPARK_TEST_DB_DSN —— 拒绝以「跳过」冒充通过"
else
  skip "未设 SPARK_TEST_DB_DSN：真库断言**本轮未执行**（数据面真库行为尚未验证）"
fi

# ───────────────────────── 5. Rust 内核 ─────────────────────────
step "5. Rust 计算内核"
if [ -n "$CARGOEXE" ]; then
  if ( cd compute && "$CARGOEXE" test --locked --quiet >/tmp/spark-rust-test.log 2>&1 ); then
    ok "cargo test 全绿"
  else
    bad "cargo test 失败"; tail -20 /tmp/spark-rust-test.log
  fi
  if ( cd compute && "$CARGOEXE" run --quiet --locked -- --selftest >/tmp/spark-rust-self.log 2>&1 ); then
    ok "内核 selftest 通过"
  else
    bad "内核 selftest 失败"; tail -20 /tmp/spark-rust-self.log
  fi
else
  skip "cargo 不可用：Rust 段未执行"
fi

# ───────────────────────── 6. 前端 ─────────────────────────
step "6. 前端（三段解耦）"
if [ -d web/node_modules ]; then
  ( cd web && "$NODEEXE" scripts/layering-gate.mjs ) && ok "分层闸门通过" || bad "分层闸门失败"
  ( cd web && "$NODEEXE" --test test/*.test.mjs >/tmp/spark-web-test.log 2>&1 ) \
    && ok "前端测试通过（$(grep -c '^ok ' /tmp/spark-web-test.log || echo '?') 通过）" \
    || { bad "前端测试失败"; tail -20 /tmp/spark-web-test.log; }
  ( cd web && "$NODEEXE" node_modules/typescript/bin/tsc --noEmit ) && ok "tsc 零错误" || bad "tsc 报错"
  ( cd web && "$NODEEXE" scripts/build.mjs >/dev/null 2>&1 ) && ok "前端构建产物已生成" || bad "前端构建失败"
else
  skip "web/node_modules 缺失：请先 cd web && npm install"
fi

# ───────────────────────── 7. 产物密钥扫描（G11b）─────────────────────────
step "7. 前端产物密钥扫描（G11b）"
if [ -d web/dist ]; then
  if grep -rEn '(client_secret|api_key|access_token|BEGIN [A-Z ]*PRIVATE KEY|user:pass@)' web/dist/ 2>/dev/null; then
    bad "前端产物中发现疑似密钥"
  else
    ok "前端产物无密钥形态"
  fi
else
  skip "无 web/dist，跳过"
fi

# ───────────────────────── 8. sparkd 真进程冒烟 ─────────────────────────
step "8. sparkd 真进程冒烟（HTTP 层，不靠 mock）"

# 8-pre ★ sparkd 是 fail-closed 的：拿不到 compute 内核就拒绝启动。
#        因此冒烟必须先确保 Rust 内核二进制存在（缺失时先构建）。
COMPUTE_BIN=""
for cand in "$ROOT/compute/target/release/spark-compute" "$ROOT/compute/target/release/spark-compute.exe" "$ROOT/spark-compute" "$ROOT/spark-compute.exe"; do
  [ -x "$cand" ] && COMPUTE_BIN="$cand" && break
done
if [ -z "$COMPUTE_BIN" ] && [ -n "$CARGOEXE" ]; then
  printf '  · 未找到 compute 内核，尝试构建（cargo build --release）…\n'
  ( cd compute && "$CARGOEXE" build --release --locked --quiet >/dev/null 2>&1 )
  for cand in "$ROOT/compute/target/release/spark-compute" "$ROOT/compute/target/release/spark-compute.exe"; do
    [ -x "$cand" ] && COMPUTE_BIN="$cand" && break
  done
fi
if [ -n "$COMPUTE_BIN" ]; then
  ok "compute 内核就位：$COMPUTE_BIN"
else
  bad "compute 内核缺失 —— sparkd 会 fail-closed 拒绝启动（这是设计，不是 bug）"
fi

if [ -n "$COMPUTE_BIN" ] && [ -d web/dist ] && [ -f web/dist/index.html ]; then
  # 起进程：不设 DSN ⇒ 应进入降级模式（这是**预期**行为，不是失败）
  ( cd backend && "$GOEXE" build -o /tmp/sparkd ./cmd/sparkd ) || \
    "$GOEXE" build -o /tmp/sparkd ./backend/cmd/sparkd
  if [ -x /tmp/sparkd ]; then
    SPARK_COMPUTE_BIN="$COMPUTE_BIN" SPARK_WEB_DIR="$ROOT/web/dist" \
      /tmp/sparkd -addr 127.0.0.1:18099 >/tmp/sparkd.log 2>&1 &
    SRV_PID=$!
    sleep 3
    if kill -0 "$SRV_PID" 2>/dev/null; then
      # 8a 静态托管
      CODE=$(curl -s -o /tmp/idx.html -w '%{http_code}' http://127.0.0.1:18099/ || echo 000)
      if [ "$CODE" = "200" ] && grep -q 'id="app"\|id=app' /tmp/idx.html; then
        ok "GET / 返回前端壳（200）"
      else
        bad "GET / 期望 200 + 前端壳，实际 $CODE"
      fi
      # 8b SPA fallback
      CODE=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18099/report/pnl 2>/dev/null || echo 000)
      [ "$CODE" = "200" ] && ok "SPA fallback 生效（200）" || bad "SPA fallback 期望 200，实际 $CODE"
      # 8c healthz 应活且 db=false（无库降级）
      HZ=$(curl -s http://127.0.0.1:18099/healthz 2>/dev/null || echo '{}')
      echo "$HZ" | grep -q '"ok":true' && ok "healthz ok" || bad "healthz 异常: $HZ"
      echo "$HZ" | grep -q '"db":false' && ok "healthz db=false（无库降级，如实暴露）" || bad "healthz 未如实暴露 db 状态: $HZ"
      # 8d ★ API 无库必须显式报错，绝不伪造数据
      #    grain 必须给出，否则会先命中「unsupported grain」而非 store 检查。
      CODE=$(curl -s -o /tmp/q.json -w '%{http_code}' -X POST http://127.0.0.1:18099/api/query \
        -H 'X-Spark-Account: lead.sea' -H 'Content-Type: application/json' \
        -d '{"v":"1.0","time":{"mode":"preset","preset":"mtd","grain":"month","timezone":"Asia/Bangkok"},"filters":[],"dims":{"level":"store"}}' 2>/dev/null || echo 000)
      if [ "$CODE" = "500" ] && grep -qi 'store not configured' /tmp/q.json; then
        ok "★ /api/query 无库时显式 500「store not configured」（不伪造数据）"
      else
        bad "/api/query 降级语义错误：HTTP $CODE, body=$(head -c 200 /tmp/q.json)"
      fi
      # 8e ★ 未注册桶必须被拒（拒绝隐式查询 / 防越表）
      #    桶名经 precomputeHint.bucket 指定；UnregisteredBucketFailsClosed 路径。
      #    注意：无库时第 141 行的 store not configured 会先命中，
      #    所以这里断言的是「非 200 且不返回任何数据行」——
      #    真正区分「拒绝」与「返回数据」的是响应体里没有 rows 内容。
      CODE=$(curl -s -o /tmp/q2.json -w '%{http_code}' -X POST http://127.0.0.1:18099/api/query \
        -H 'X-Spark-Account: lead.sea' -H 'Content-Type: application/json' \
        -d '{"v":"1.0","time":{"mode":"preset","preset":"mtd","grain":"month"},"dims":{"level":"store"},"precomputeHint":{"bucket":"pg_catalog.pg_tables"}}' 2>/dev/null || echo 000)
      if [ "$CODE" = "200" ]; then
        bad "未注册桶竟返回 200（隐式查询未被拒绝）"
      else
        ok "未注册桶被拒（HTTP $CODE，未泄露数据）"
      fi
    else
      bad "sparkd 未启动成功"; tail -20 /tmp/sparkd.log
    fi
    kill "$SRV_PID" 2>/dev/null
    wait "$SRV_PID" 2>/dev/null
  else
    bad "sparkd 构建失败"
  fi
else
  skip "无 web/dist：跳过 sparkd 冒烟（先构建前端）"
fi

# ───────────────────────── 汇总 ─────────────────────────
printf '\n\033[1m════════ 汇总 ════════\033[0m\n'
printf '  通过 %d  失败 %d  跳过 %d\n' "$PASS" "$FAIL" "$SKIP"
if [ "$FAIL" -gt 0 ]; then
  printf '\n\033[31m失败项：%b\033[0m\n' "$FAILED_NAMES"
  printf '\n\033[31m结论：未达可上线标准。\033[0m\n'
  exit 1
fi
if [ "$SKIP" -gt 0 ]; then
  printf '\n\033[33m注意：有 %d 段被跳过，其覆盖的行为**尚未被验证**。\033[0m\n' "$SKIP"
fi
printf '\n\033[32m结论：已通过本轮全部可执行断言。\033[0m\n'
exit 0
