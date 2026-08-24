#!/usr/bin/env bash
# 出口重构验证矩阵一键复跑(EGRESS-REVIEW-STATUS.md 证据表的入口)。
# 覆盖: 三包 -race 全量、全链轮换 e2e(本地替身)、资源 soak、探活/轮换/订阅/
# 审计/热更新等全部出口测试, 以及 Acquire 基准 + 回归护栏抽样。
# 迁移矩阵(需容器)另见 scripts/verify-postgres-migrations.sh。
# 用法: scripts/verify-egress-matrix.sh [--with-postgres]
set -euo pipefail

cd "$(dirname "$0")/../backend"

echo "== [1/5] 出口三包 -race 全量"
go test ./internal/infra/egress/ ./internal/application/egress/ ./internal/application/gateway/ \
  -count=1 -race -timeout 10m

echo "== [2/5] 持久层出口套件(SQLite 全量)"
go test ./internal/infra/persistence/relational/ -count=1 -timeout 10m

if [[ "${1:-}" == "--with-postgres" ]]; then
  echo "== [2b] 持久层出口套件(真实 PostgreSQL)"
  if [[ -z "${TEST_POSTGRES_ADMIN_DSN:-}" ]]; then
    echo "TEST_POSTGRES_ADMIN_DSN 未设置, 使用临时容器" >&2
    "$0/../verify-postgres-migrations.sh"
  else
    go test ./internal/infra/persistence/relational/ -count=1 -timeout 10m
  fi
fi

echo "== [3/5] 出口关键回归族(显式点名, 防未来误删/改名静默漏检)"
# 每组断言最少通过数:go test -run 对零匹配静默退出 0,改名/误删的测试会无声
# 消失——数量护栏让"点名族少跑了一个"变成显式失败。
run_named_tests() { # pkg pattern min_pass label
  local output
  output=$(go test "$1" -count=1 -race -run "$2" -v 2>&1)
  local passed
  passed=$(printf '%s' "$output" | grep -c '^--- PASS')
  printf '%s' "$output" | grep -E '^(--- |=== RUN)' | sed 's/^/   /'
  if [ "$passed" -lt "$3" ]; then
    echo "FATAL: $4 只通过 $passed < $3 个测试(改名/误删?)" >&2
    exit 1
  fi
}
run_named_tests ./internal/infra/egress \
 'TestE2EPoolRouteRealProxyRoundTrip|TestAcquirePoolRoutedFallbackChain|TestPoolFallbackChainConflictMatrix|TestGuardStateSoftCooldownReplicaLocal|TestRotationCursorContinuityAcrossReplicas|TestStickyAccountTemplatePoolMemberLifecycle|TestInflightCountersSweptForDeletedNodes|TestClientCacheLifecycleUnderProxyURLEditAndHotUpdates|TestRefreshDueClearancesViaSnapshotCacheSemantics|TestRotationWorkerSurvivesProcessingPanic|TestStickyAndRotationOrderRuleConsistency|TestQualityReleaseAndCooldownManagerContracts|TestBrowserClientDoRoundTrip|TestBrowserClientUncompressedTranslation|TestRecordDirectPhysicalCallDelegation|TestLeaseLifecycleUnderCancellationStorm|TestDoPinnedHTTPSSSRFGuards|TestDialContextCancellationBridgesAndReaps|TestNodeSoftCooledExpiredEntryEviction|TestTraceSelectionOnlyAdvancesOnSuccessfulAcquisition|TestRoutingStatsSnapshotOrderedDeterministic' \
 20 'infra/egress 点名族'
run_named_tests ./internal/application/egress \
 'TestRotationRateGuardsPreventStorm|TestConcurrentRotationTriggersDeduplicate|TestRotationRecoversPendingQuarantineAfterRestart|TestSubscriptionSyncIdempotencyUnderFaultInjection|TestImportTextInputBoundaries|TestEgressRotationMetricVocabulary|TestOnEgressDegradedRequiresDistinctAccounts|TestDisabledCreateStaysDisabled|TestNodeCRUDServicePath|TestPoolCRUDServiceInvariants|TestPoolFallbackChainValidationOnServicePath|TestOperationsConfigServicePathInvariants|TestSourceListAndRevealServicePath|TestQualityObserverFacadeWiring|TestRotationConfigDisableDropsQueue|TestRotationSkipPaths|TestRotationMinIntervalDefersViaRequeue|TestRotateNodeServicePath|TestQualityGuardConfigNormalization' \
 19 'application/egress 点名族'
run_named_tests ./internal/infra/persistence/relational \
 'TestUpdateEgressSourcePreservesConcurrentSyncState|TestUpdateEgressSourceConfigChangeReArmsSchedule|TestEgressOperationsCleanupDeletesOnlyDualStackUnhealthyNodes|TestGetEgressNodePoolProjectionMatchesList|TestListEgressNodesLazyEnrichment|TestUpdateEgressNodeConfigResetPreservesInFlightQuarantine|TestUpsertEgressNodesFromSourceResetsObservationsOnProxyChange' \
 7 'relational 点名族'
run_named_tests ./internal/application/gateway \
 'TestE2ERotationFullChainLocalDoubles|TestEgressAuditTrailMatchesActualExit|TestRoutingConfigHotUpdatePropagation|TestWebAssetEgressDoesNotOverwriteInferenceTrace' \
 4 'gateway 点名族'

echo "== [4/5] 资源稳定性 soak(真实代理 3200 次出站)"
go test ./internal/infra/egress/ -count=1 -run TestEgressSoakResourceStability -v 2>&1 | grep -E '^(--- |soak:)' | sed 's/^/   /'

echo "== [5/5] Acquire/轮换游标/缓存命中基准 + 回归护栏抽样"
go test ./internal/infra/egress/ -run xxx -bench 'BenchmarkAcquire(PoolTarget|FixedNodeTarget|AutoSchedule|Concurrent64)|BenchmarkRotationCursorAdvance|BenchmarkManagerAcquireCachedBuild' \
  -benchtime 1s -count=1 2>&1 | grep -E '^Benchmark' | sed 's/^/   /'

echo ">> egress verification matrix passed"
