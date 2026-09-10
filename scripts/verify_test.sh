#!/usr/bin/env bash
# Exercise failure propagation without invoking compilers, fuzz engines or services.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/verify.sh"

check_failure() (
	local scenario="$1"
	case "$scenario" in
		fuzz_engine)
			go() { if [[ "$*" == *"-list"* ]]; then printf 'FuzzFirst\nFuzzSecond\n'; elif [[ "$*" == *FuzzFirst* ]]; then return 9; else return 0; fi; }
			stage "injected first fuzz failure" stage_fuzz_engines
			;;
		fuzz_discovery)
			go() { if [[ "$*" == *"-list"* ]]; then return 8; fi; return 0; }
			stage "injected fuzz discovery failure" stage_fuzz_engines
			;;
		fuzz_missing)
			go() { printf 'ok example/package (no fuzz targets)\n'; }
			stage "missing fuzz target" stage_fuzz_engines
			;;
		seeds)
			go() { return 7; }
			stage "injected seed failure" stage_fuzz_seeds
			;;
		format)
			gofmt() { return 6; }
			stage "injected format failure" stage_fmt
			;;
		preflight)
			go() { return 5; }
			stage "missing package" stage_targets
			;;
	esac
	if [[ "$FAILED" != 1 || "${RESULTS[0]}" != FAILED* ]]; then
		echo "FAIL: $scenario was reported as successful" >&2
		return 1
	fi
)

for scenario in fuzz_engine fuzz_discovery fuzz_missing seeds format preflight; do
	check_failure "$scenario"
done
(
	go() { return 0; }
	stage "successful seeds" stage_fuzz_seeds
	[[ "$FAILED" == 0 && "${RESULTS[0]}" == ok* ]]
)
(
	unset TEST_POSTGRES_DSN TEST_POSTGRES_ADMIN_DSN TEST_REDIS_ADDRESS
	TIER=fast
	stage_targets() { :; }
	stage_architecture() { :; }
	stage_build() { :; }
	stage_fmt() { :; }
	stage_vet() { :; }
	stage_staticcheck() { :; }
	stage_race() { :; }
	output=$(main)
	[[ "$output" == *"skipped: PostgreSQL integration: test DSN absent"* ]]
	[[ "$output" == *"skipped: Redis integration: TEST_REDIS_ADDRESS absent"* ]]
)
printf 'PASS: verifier preserves failures and distinguishes successful stages\n'
