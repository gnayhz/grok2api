#!/usr/bin/env bash
# verify.sh — one-shot verification matrix for grok2api backend.
# Consolidates the checks established across review rounds 1-11:
#   build, vet, staticcheck, race tests, fuzz seeds, govulncheck, flaky probe.
#
# Usage:
#   scripts/verify.sh          # fast tier: build + vet + staticcheck + race
#   scripts/verify.sh full     # + fuzz seeds + govulncheck + flaky (count=3)
#   scripts/verify.sh fuzz     # run the fuzzing engines for 30s per target
#
# Third-party tools (staticcheck, govulncheck) degrade to SKIP with a warning
# when absent; the core go toolchain checks are always required.
#
# Backend integration tests SKIP without env; run them against ephemeral
# real backends (optimize round 13 verified both green this way):
#   docker run -d --rm --name redis-verify --network grok2api redis:7-alpine
#   TEST_REDIS_ADDRESS=redis-verify:6379 go test ./internal/infra/runtime/redis/ -count=1
#   docker stop redis-verify
#   docker run -d --rm --name pg-verify --network grok2api -e POSTGRES_PASSWORD=pgtest #     -e POSTGRES_DB=grok2api_test postgres:16-alpine
#   TEST_POSTGRES_DSN='postgres://postgres:pgtest@pg-verify:5432/grok2api_test?sslmode=disable' #     go test ./internal/infra/persistence/relational/ -run Integration -count=1
#   docker stop pg-verify
#
# Crash-recovery spot check (optimize round 14 verified this way): kill the
# running instance mid-stream (docker kill, SIGKILL), restart, then confirm
# pragma integrity_check=ok, zero orphan attempts, zero null-duration rows,
# and a normal follow-up request. In-flight requests lose their buffered
# audit rows to SIGKILL (expected ledger-mode loss, not corruption); clients
# see a truncated stream with no [DONE] terminator.

set -euo pipefail

cd "$(dirname "$0")/../backend"

TIER="${1:-fast}"
FAILED=0
SKIPPED=()
declare -a STAGES=()
declare -a RESULTS=()
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
stage_vet() { go vet ./...; }
stage_staticcheck() {
	if have staticcheck; then
		# ST1005 (capitalized error strings) is waived project-wide in staticcheck.conf.
		"$(resolve_bin staticcheck)" ./...
	else
		echo "staticcheck not installed — skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"
		SKIPPED+=(staticcheck)
		SKIPPED_NOW="tool missing"
	fi
}
stage_race() { go test -race -count=1 ./...; }
stage_fuzz_seeds() {
	go test -count=1 -run 'FuzzParseRisk|FuzzObserveQualityChunk|FuzzPeekQualityBody' ./internal/infra/rsc/ ./internal/application/gateway/
}
stage_govulncheck() {
	if have govulncheck; then
		"$(resolve_bin govulncheck)" ./...
	else
		echo "govulncheck not installed — skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"
		SKIPPED+=(govulncheck)
		SKIPPED_NOW="tool missing"
	fi
}
stage_flaky() {
	# Repeat core packages three times to surface scheduling-dependent flakes.
	go test -count=3 ./internal/application/gateway/ ./internal/application/account/risk/ ./internal/infra/rsc/ ./internal/infra/persistence/relational/ ./internal/app/
}
stage_fuzz_engines() {
	go test -fuzz FuzzParseRisk -fuzztime 30s -run xxx ./internal/infra/rsc/
	go test -fuzz FuzzPeekQualityBody -fuzztime 30s -run xxx ./internal/application/gateway/
	go test -fuzz FuzzObserveQualityChunk -fuzztime 30s -run xxx ./internal/application/gateway/
}

stage "build" stage_build
stage "vet" stage_vet
stage "staticcheck" stage_staticcheck
stage "race suite" stage_race

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
	printf 'skipped (tool missing): %s\n' "${SKIPPED[*]}"
fi
if [ "$FAILED" -ne 0 ]; then
	printf 'RESULT: FAILED\n'
	exit 1
fi
printf 'RESULT: PASS (%s tier)\n' "$TIER"
