package gateway

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// probeSession builds a minimal session: candidate 0 (ID 7) entered via the
// quota-probe lane, candidate 1 (ID 8) via the normal lane.
func probeSession() *selectionSession {
	return &selectionSession{
		values: []accountdomain.RoutingCandidate{
			{Credential: accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}},
			{Credential: accountdomain.Credential{ID: 8, Provider: accountdomain.ProviderBuild}},
		},
		normalCandidates: []int{1},
		probeCandidates:  []int{0},
	}
}

// TestProbeOriginExcludedFromSameAccountRetry locks the session-level contract
// the withhold guard depends on (service.go probeOrigin check): an account that
// entered via the quota-probe lane must never be re-queued by RetryAccount,
// even after paid-probe promotion flips lease.QuotaProbe to false —
// wasQuotaProbeCandidate is the surviving evidence (external review 2.md).
func TestProbeOriginExcludedFromSameAccountRetry(t *testing.T) {
	t.Parallel()
	session := probeSession()

	if !session.wasQuotaProbeCandidate(7) {
		t.Fatal("probe-lane account must be recognized as probe-origin")
	}
	if session.wasQuotaProbeCandidate(8) {
		t.Fatal("normal-lane account must not be probe-origin")
	}

	// RetryAccount re-queues only normal candidates: the probe account cannot
	// masquerade as a same-account retry target.
	session.RetryAccount(7)
	if session.retryAccountID == 7 {
		t.Fatal("RetryAccount must never re-queue a probe-lane account")
	}
	session.RetryAccount(8)
	if session.retryAccountID != 8 {
		t.Fatal("RetryAccount must re-queue a normal-lane account")
	}

	// Zero ID is a no-op.
	session.RetryAccount(0)
	if session.retryAccountID != 8 {
		t.Fatal("RetryAccount(0) must be a no-op")
	}
}

// TestWasQuotaProbeCandidateNilSessionSafe pins the nil-receiver guard: the
// service call site may hold a nil selection under early failure paths.
func TestWasQuotaProbeCandidateNilSessionSafe(t *testing.T) {
	t.Parallel()
	var session *selectionSession
	if session.wasQuotaProbeCandidate(7) {
		t.Fatal("nil session must report not-probe")
	}
}
