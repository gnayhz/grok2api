#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root/backend"
go test ./... -count=1
go test -race ./internal/infra/egress ./internal/pkg/browsertransport ./internal/pkg/netbudget ./internal/pkg/proxydial ./internal/pkg/tunnelproxy ./internal/infra/provider/cli ./internal/infra/provider/console ./internal/infra/provider/web ./internal/application/egress ./internal/app ./internal/infra/persistence/relational ./internal/transport/http/egress -count=1
go test ./internal/infra/egress -run 'TestRuntimeEndToEndLatencyAndResourceStability|TestEgressSoakResourceStability|TestAcquireRefreshLatencyDistributionOnPostgres' -v -count=1
go test ./internal/app -run TestRuntimeEndToEndAuthoritativeAdmissionCost -v -count=1
go test -tags egress_final_review ./internal/infra/egress -run '^TestFinalReviewSustainedLoadOverloadAndHotUpdate$' -v -count=1
if [[ "${EGRESS_BENCH:-0}" == "1" ]]; then
  go test ./internal/infra/egress -run '^$' -bench 'BenchmarkAcquire|BenchmarkPoolAccountingScale' -benchmem -count=3
fi
