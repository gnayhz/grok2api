package risk

import (
	"context"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestDisableModeCoversConsoleChannel closes the coverage gap flagged by the
// test-quality audit: disable-mode consequences were only asserted for Web +
// Build; the identity group also includes Console accounts, which must be
// disabled independently (channel failures must not skip each other).
func TestDisableModeCoversConsoleChannel(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso"
	accounts.linkedWeb[7] = 90
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{55, 56}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	cfg := baseTestConfig() // OnDenied: disable
	service := New(cfg, accounts, store, checker, nil)

	service.attribute(context.Background(), credentialBuild(7), 0)

	accounts.mu.Lock()
	defer accounts.mu.Unlock()
	if _, disabled := accounts.disabled[7]; !disabled {
		t.Fatal("degraded build 7 must be disabled")
	}
	for _, id := range []uint64{90, 55, 56} {
		if _, disabled := accounts.disabled[id]; disabled {
			t.Fatalf("identity-group member %d must not be cascaded in disable mode", id)
		}
	}
}

// TestPatrolCutoffsDeriveFromConfig pins the patrol freshness bounds fed into
// the due-identity query: patrol interval and error retry window derive from
// configuration (previously 0% covered).
func TestPatrolCutoffsDeriveFromConfig(t *testing.T) {
	cfg := baseTestConfig() // PatrolInterval 30d, ErrorRetry 1h
	service := New(cfg, nil, &fakeStore{verdicts: map[uint64]StoredVerdict{}}, &fakeChecker{}, nil)
	patrolDue, errorRetryDue := service.PatrolCutoffs()
	now := time.Now().UTC()
	if d := now.Sub(patrolDue); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("patrol cutoff = %v (~%v), want ~30d before now", patrolDue, d)
	}
	if d := now.Sub(errorRetryDue); d < 55*time.Minute || d > 65*time.Minute {
		t.Fatalf("error-retry cutoff = %v (~%v), want ~1h before now", errorRetryDue, d)
	}
}

func credentialBuild(id uint64) accountdomain.Credential {
	return accountdomain.Credential{ID: id, Provider: accountdomain.ProviderBuild}
}
