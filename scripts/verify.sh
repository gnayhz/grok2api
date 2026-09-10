#!/usr/bin/env bash
# verify.sh — one-shot verification matrix for grok2api backend.
# Consolidates the checks established across review rounds 1-11:
#   build, vet, staticcheck, race tests, fuzz seeds, govulncheck, flaky probe.
#
# Usage:
#   scripts/verify.sh check    # target preflight + architecture checks
#   scripts/verify.sh          # fast tier: preflight + architecture + build + fmt + vet + staticcheck + race
#   scripts/verify.sh full     # + fuzz seeds + govulncheck + flaky (count=3)
#   scripts/verify.sh fuzz     # run the fuzzing engines for 30s per target
#
# Third-party tools (staticcheck, govulncheck) degrade to SKIP with a warning
# when absent; the core go toolchain checks are always required. Fuzz engines
# discover every target in FUZZ_PACKAGES, including parser and partition tests.
#
# Backend integration tests SKIP without env; run them against ephemeral
# real backends. Publish to 127.0.0.1 ports (round 4 verified green this
# way): docker-network container names resolve through fake-IP DNS on the
# host and the TCP handshake fails with EOF, so --network + name addressing
# only works when the tests themselves run inside the same network.
#   docker run -d --rm --name redis-verify -p 127.0.0.1:16379:6379 redis:7-alpine
#   TEST_REDIS_ADDRESS=127.0.0.1:16379 go test ./internal/infra/runtime/redis/ -count=1
#   docker rm -f redis-verify
#   docker run -d --rm --name pg-verify -p 127.0.0.1:15432:5432 -e POSTGRES_PASSWORD=pgtest \
#     -e POSTGRES_DB=grok2api_test postgres:16-alpine
#   TEST_POSTGRES_DSN='postgres://postgres:pgtest@127.0.0.1:15432/grok2api_test?sslmode=disable' \
#     TEST_REDIS_ADDRESS=127.0.0.1:16379 go test ./internal/infra/persistence/relational/ -run Integration -count=1
#   docker rm -f pg-verify
#
# Crash recovery must be checked in an isolated instance against the current
# history and ledger contract. A truncated stream alone does not verify that
# accepted history or pending billing records can be recovered.

set -euo pipefail

VERIFY_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TIER="${1:-fast}"
FAILED=0
SKIPPED=()
declare -a STAGES=()
declare -a RESULTS=()
REPEAT_PACKAGES=(./internal/application/gateway ./internal/application/account
	./internal/infra/rsc ./internal/infra/persistence/relational ./internal/app
	./internal/transport/http/inference ./internal/pkg/jsonpeek)
FUZZ_PACKAGES=(./internal/application/gateway ./internal/pkg/jsonpeek
	./internal/infra/provider/searchresult ./internal/application/egress
	./internal/infra/provider/conversation ./internal/pkg/responseflow)
# SKIPPED_NOW 由 stage 函数体设置：非空时本阶段记 skipped 而非 ok（round 19）。

stage() {
	local name="$1" start
	start=$(date +%s)
	STAGES+=("$name")
	printf '\n=== %s ===\n' "$name"
	shift  # drop the stage name; execute the remaining arguments
	SKIPPED_NOW=""
	if "$@"; then
		if [[ -n "$SKIPPED_NOW" ]]; then
			RESULTS+=("skipped ($SKIPPED_NOW)")
		else
			RESULTS+=("ok ($(( $(date +%s) - start ))s)")
		fi
	else
		RESULTS+=("FAILED ($(( $(date +%s) - start ))s)")
		FAILED=1
	fi
}

# have 探测命令：PATH 之外也查 go env GOPATH/bin——go install 的工具
# 默认落在那里，容器/CI 环境常不把它加入 PATH（round 74 曾因 PATH
# 传播在脚本内 skip staticcheck/govulncheck，基线只得单独验证）。
have() {
	command -v "$1" >/dev/null 2>&1 && return 0
	local gopath_bin
	gopath_bin="$(go env GOPATH 2>/dev/null)/bin"
	[ -n "$gopath_bin" ] && [ -x "$gopath_bin/$1" ]
}
# resolve_bin 输出探测到的二进制路径（供 stage 直接调用）。
resolve_bin() {
	command -v "$1" >/dev/null 2>&1 && { command -v "$1"; return 0; }
	local gopath_bin
	gopath_bin="$(go env GOPATH 2>/dev/null)/bin"
	if [ -n "$gopath_bin" ] && [ -x "$gopath_bin/$1" ]; then
		printf '%s\n' "$gopath_bin/$1"
		return 0
	fi
	return 1
}

stage_build() { go build ./...; }
# gofmt 漂移检查：交付门此前不含 fmt，17 个文件带着未格式化内容入库
# （round 37 清零）。非空输出即失败。
stage_fmt() {
	# A missing or broken formatter must fail a required toolchain check.
	local drift
	# gofmt is part of the required Go toolchain; capture its failure before
	# inspecting output, rather than treating a broken command as no drift.
	drift=$(gofmt -l .) || return
	if [ -n "$drift" ]; then
		echo "gofmt drift detected:"
		echo "$drift"
		return 1
	fi
}
stage_vet() { go vet ./...; }
stage_staticcheck() {
	if have staticcheck; then
		# ST1005 (capitalized error strings) is waived project-wide in staticcheck.conf.
		"$(resolve_bin staticcheck)" ./...
	else
		echo "staticcheck not installed — skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"
		SKIPPED+=("staticcheck: tool missing")
		SKIPPED_NOW="tool missing"
	fi
}
stage_race() {
	# 一次性竞态失败若不留痕将无从排查（verify-full 一次失败、
	# 五次复跑全绿的无头绪案例）——把完整输出落盘再按需透传。
	# The full inference suite takes over 10 minutes under -race (E06/G02).
	# Keep a finite suite deadline without truncating healthy integration runs.
	if ! go test -race -timeout 30m -count=1 ./... >".race-output.log" 2>&1; then
		cat ".race-output.log"
		echo "race suite 完整输出已保存到 .race-output.log"
		return 1
	fi
	rm -f ".race-output.log"
}
stage_fuzz_seeds() {
	# One command preserves any package failure under stage's conditional call.
	go test -count=1 -run '^Fuzz' "${FUZZ_PACKAGES[@]}"
}
stage_govulncheck() {
	if have govulncheck; then
		# 已知残留（2026-08 轮审记录）：GO-2026-5932 指 x/crypto/openpgp 上游弃维，
		# Fixed: N/A 无法通过升级消除。本项目仅用 bcrypt/hkdf/chacha20poly1305，
		# 符号级 0 可达；x/crypto 被 15+ 传递依赖共享、不可移除——按接受处理。
		"$(resolve_bin govulncheck)" ./...
	else
		echo "govulncheck not installed — skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"
		SKIPPED+=("govulncheck: tool missing")
		SKIPPED_NOW="tool missing"
	fi
}
stage_flaky() {
	# Repeat core packages three times to surface scheduling-dependent flakes.
	# transport/http/inference 自 round 25/33 起承载守卫检查器与 copyStream 的
	# 流式测试（含并发泵），与 gateway 同属时序敏感核心，补入重复探测。
	# Three complete runs also share one package deadline; the inference suite
	# takes about five minutes per run without -race.
	go test -timeout 30m -count=3 "${REPEAT_PACKAGES[@]}"
}
stage_fuzz_engines() {
	local package targets target found
	for package in "${FUZZ_PACKAGES[@]}"; do
		targets=$(go test -list '^Fuzz' "$package") || return
		found=0
		while IFS= read -r target; do
			[[ "$target" =~ ^Fuzz[A-Za-z0-9_]+$ ]] || continue
			found=1
			go test -fuzz "^${target}$" -fuzztime 30s -run '^$' "$package" || return
		done <<< "$targets"
		if [[ "$found" == 0 ]]; then
			echo "No fuzz targets in configured package: $package" >&2
			return 1
		fi
	done
}

stage_targets() {
	go list ./internal/architecture "${REPEAT_PACKAGES[@]}" "${FUZZ_PACKAGES[@]}" >/dev/null
}
stage_architecture() { go test ./internal/architecture -count=1; }

main() {
	case "$TIER" in check|fast|full|fuzz) ;; *) echo "Unknown tier: $TIER" >&2; return 2;; esac
	cd "$VERIFY_ROOT/backend" || return
	stage "target preflight" stage_targets
	if [[ "$FAILED" != 0 ]]; then return 1; fi
	stage "architecture" stage_architecture
	if [[ "$TIER" != check ]]; then
		if [[ -z "${TEST_POSTGRES_DSN:-}" && -z "${TEST_POSTGRES_ADMIN_DSN:-}" ]]; then
			SKIPPED+=("PostgreSQL integration: test DSN absent")
		fi
		if [[ -z "${TEST_REDIS_ADDRESS:-}" ]]; then
			SKIPPED+=("Redis integration: TEST_REDIS_ADDRESS absent")
		fi
		stage "build" stage_build
		stage "gofmt" stage_fmt
		stage "vet" stage_vet
		stage "staticcheck" stage_staticcheck
		stage "race suite" stage_race
	fi

	if [ "$TIER" = "full" ]; then
		stage "fuzz seeds" stage_fuzz_seeds
		stage "govulncheck" stage_govulncheck
		stage "flaky probe (count=3)" stage_flaky
	fi

	if [ "$TIER" = "fuzz" ]; then
		stage "fuzz engines (30s each)" stage_fuzz_engines
	fi

	printf '\n==================== SUMMARY ====================\n'
	for i in "${!STAGES[@]}"; do
		printf '%-32s %s\n' "${STAGES[$i]}" "${RESULTS[$i]}"
	done
	if [ "${#SKIPPED[@]}" -gt 0 ]; then
		printf 'skipped: %s\n' "${SKIPPED[@]}"
	fi
	if [ "$FAILED" -ne 0 ]; then
		printf 'RESULT: FAILED\n'
		exit 1
	fi
	printf 'RESULT: PASS (%s tier)\n' "$TIER"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	main
fi
