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

set -euo pipefail

cd "$(dirname "$0")/../backend"

TIER="${1:-fast}"
FAILED=0
SKIPPED=()
declare -a STAGES=()
declare -a RESULTS=()

stage() {
	local name="$1" start
	start=$(date +%s)
	STAGES+=("$name")
	printf '\n=== %s ===\n' "$name"
	shift  # drop the stage name; execute the remaining arguments
	if "$@"; then
		RESULTS+=("ok ($(( $(date +%s) - start ))s)")
	else
		RESULTS+=("FAILED ($(( $(date +%s) - start ))s)")
		FAILED=1
	fi
}

have() { command -v "$1" >/dev/null 2>&1; }

stage_build() { go build ./...; }
stage_vet() { go vet ./...; }
stage_staticcheck() {
	if have staticcheck; then
		# ST1005 (capitalized error strings) is waived project-wide in staticcheck.conf.
		staticcheck ./...
	else
		echo "staticcheck not installed — skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"
		SKIPPED+=(staticcheck)
	fi
}
stage_race() { go test -race -count=1 ./...; }
stage_fuzz_seeds() {
	go test -count=1 -run 'FuzzParseRisk|FuzzObserveQualityChunk' ./internal/infra/rsc/ ./internal/application/gateway/
}
stage_govulncheck() {
	if have govulncheck; then
		govulncheck ./...
	else
		echo "govulncheck not installed — skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"
		SKIPPED+=(govulncheck)
	fi
}
stage_flaky() {
	# Repeat core packages three times to surface scheduling-dependent flakes.
	go test -count=3 ./internal/application/gateway/ ./internal/application/account/risk/ ./internal/infra/rsc/ ./internal/infra/persistence/relational/ ./internal/app/
}
stage_fuzz_engines() {
	go test -fuzz FuzzParseRisk -fuzztime 30s -run xxx ./internal/infra/rsc/
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
	printf '%-32s %s\n' "${STATES[$i]:-${STAGES[$i]}}" "${RESULTS[$i]}"
done
if [ "${#SKIPPED[@]}" -gt 0 ]; then
	printf 'skipped (tool missing): %s\n' "${SKIPPED[*]}"
fi
if [ "$FAILED" -ne 0 ]; then
	printf 'RESULT: FAILED\n'
	exit 1
fi
printf 'RESULT: PASS (%s tier)\n' "$TIER"
