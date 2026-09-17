package gateway

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func assertRecoveryDelay(t *testing.T, recovery account.QuotaRecovery, started time.Time, delay time.Duration) {
	t.Helper()
	if recovery.NextProbeAt == nil {
		t.Fatalf("recovery has no next probe: %#v", recovery)
	}
	assertTimeDelay(t, *recovery.NextProbeAt, started, delay)
}

func assertTimeDelay(t *testing.T, actual, started time.Time, delay time.Duration) {
	t.Helper()
	want := started.Add(delay)
	if actual.Before(want.Add(-time.Second)) || actual.After(want.Add(2*time.Second)) {
		t.Fatalf("time = %s, want around %s", actual, want)
	}
}
