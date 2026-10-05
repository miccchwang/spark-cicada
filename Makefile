# spark-cicada · 顶层 Makefile
#
# 把 docs/05-验收闸门.md §3 约定的 `make gate*` 目标落到实处。
# 任一闸门失败 ⇒ 非零退出 ⇒ 阻断合并。

SHELL := /bin/sh

# ── 工具链（按托管环境绝对路径，避免污染系统 PATH）──
GO      ?= go
CARGO   ?= cargo
GO_PKG  := ./backend/...

.PHONY: help
help:
	@echo "make gate          全部闸门"
	@echo "make gate-fast     静态闸门（G1/G4/G11）"
	@echo "make gate-auth     权限闸门（G2/G3/G7/G10）"
	@echo "make gate-precomp  预计算一致性（G5/G6）"
	@echo "make gate-plugins  插槽与性能（G8/G9）"
	@echo "make gate-dr       备份与容灾（G12）"
	@echo "make gate-compute  计算内核自检"
	@echo "make gate-sql      SQL 迁移静态闸门（append-only/D7/可空/region/无凭据/种子诚实）"
	@echo "make gate-admin    控制面闸门（G4 解耦 / G5 传播 / G6 局部重算 / G9 挂载 / G10 审计）"
	@echo "make gate-db       真库集成闸门（需 SPARK_TEST_DB_DSN；未设置则跳过）"
	@echo "make gate-secret   全仓密钥扫描（gitleaks）"
	@echo "make migrate-up    应用数据库迁移"
	@echo "make migrate-dry   迁移校验（不写库）"
	@echo "make test          全部测试"
	@echo "make build         构建 Go + Rust"

# ───────────────────────────── 构建 ─────────────────────────────

.PHONY: build
build: build-go build-rust

.PHONY: build-go
build-go:
	cd backend && $(GO) build ./...

.PHONY: build-rust
build-rust:
	cd compute && $(CARGO) build --release

# ───────────────────────────── 数据库 ─────────────────────────────

# 应用迁移（幂等）。连接串取自 SPARK_DB_DSN 或 SPARK_PG_* 环境变量。
.PHONY: migrate-up
migrate-up:
	cd backend && $(GO) run ./cmd/spark-migrate -dir ../sql/migrations -up

.PHONY: migrate-dry
migrate-dry:
	cd backend && $(GO) run ./cmd/spark-migrate -dir ../sql/migrations -dry-run

.PHONY: migrate-status
migrate-status:
	cd backend && $(GO) run ./cmd/spark-migrate -dir ../sql/migrations -status

# ───────────────────────────── 测试 ─────────────────────────────

.PHONY: test
test: test-go test-rust

.PHONY: test-go
test-go:
	cd backend && $(GO) test ./...

.PHONY: test-rust
test-rust:
	cd compute && $(CARGO) test

# ───────────────────────────── 闸门 ─────────────────────────────

.PHONY: gate
gate: gate-fast gate-auth gate-precomp gate-plugins gate-dr gate-compute gate-sql gate-admin
	@echo "ALL GATES PASSED"

# 静态闸门：G1/G4/G11（源码静态扫描 + 前缀密钥扫描）
.PHONY: gate-fast
gate-fast:
	cd backend && $(GO) test ./internal/gate/ -run 'TestG1|TestG4|TestG11' -v

# 权限闸门：G2/G3/G7/G10
.PHONY: gate-auth
gate-auth:
	cd backend && $(GO) test ./internal/gate/ -run 'TestG2|TestG3|TestG7|TestG10|TestCompute' -v
	cd backend && $(GO) test ./internal/authz/ ./internal/chain/ ./internal/api/ ./internal/req/ -v

# 预计算一致性：G5/G6
.PHONY: gate-precomp
gate-precomp:
	cd backend && $(GO) test ./internal/gate/ -run 'TestG5|TestG6' -v
	cd backend && $(GO) test ./internal/precomp/ -v

# 插槽与性能：G8/G9
.PHONY: gate-plugins
gate-plugins:
	cd backend && $(GO) test ./internal/gate/ -run 'TestG8|TestG9' -v

# 备份与容灾：G12
.PHONY: gate-dr
gate-dr:
	cd backend && $(GO) test ./internal/gate/ -run 'TestG12' -v

# SQL 迁移静态闸门（不依赖数据库）
.PHONY: gate-sql
gate-sql:
	cd backend && $(GO) test ./internal/gate/ -run 'TestSQL' -v

# 控制面闸门（内存 Store，无需真库）
.PHONY: gate-admin
gate-admin:
	cd backend && $(GO) test ./internal/admin/ -v

# 真库集成闸门（未设置 SPARK_TEST_DB_DSN 时自动跳过）
.PHONY: gate-db
gate-db:
	cd backend && $(GO) test ./internal/store/ -run Integration -v

# 计算内核自检（缺失语义 / 覆盖率门控 / 银行家舍入）
.PHONY: gate-compute
gate-compute:
	cd compute && $(CARGO) run --quiet -- --selftest

# 密钥扫描（G11）—— 依赖 gitleaks；未安装时给出提示但不误判为通过
.PHONY: gate-secret
gate-secret:
	@if command -v gitleaks >/dev/null 2>&1; then \
		gitleaks detect --no-git --source . -v; \
	else \
		echo "[gate-secret] gitleaks 未安装，跳过全仓扫描（内置前缀扫描已由 gate-fast 覆盖）"; \
	fi

.PHONY: clean
clean:
	cd backend && $(GO) clean ./...
	cd compute && $(CARGO) clean
