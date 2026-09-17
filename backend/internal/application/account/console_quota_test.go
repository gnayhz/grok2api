package account

import (
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestPreserveActiveLocalQuotaWindowsUntilReset(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-time.Second)
	incoming := []accountdomain.QuotaWindow{{Mode: "fast", Remaining: 20, Total: 20}}

	active := preserveActiveQuotaWindows([]accountdomain.QuotaWindow{{Mode: "fast", Remaining: 7, Total: 20, ResetAt: &future}}, incoming, now)
	if len(active) != 1 || active[0].Remaining != 7 {
		t.Fatalf("active window = %#v", active)
	}

	expired := preserveActiveQuotaWindows([]accountdomain.QuotaWindow{{Mode: "fast", Remaining: 0, Total: 20, ResetAt: &past}}, incoming, now)
	if len(expired) != 1 || expired[0].Remaining != 20 {
		t.Fatalf("expired window = %#v", expired)
	}
}

func TestCompleteConsoleUsageSnapshotRejectsLegacyAndPartialWindows(t *testing.T) {
	now := time.Now().UTC()
	legacy := []accountdomain.QuotaWindow{{Mode: "console", Source: accountdomain.QuotaSourceDefault, SyncedAt: &now}}
	if completeConsoleUsageSnapshot(legacy) {
		t.Fatal("legacy synthetic Console window was accepted")
	}
	partial := []accountdomain.QuotaWindow{
		{Mode: "console", Source: accountdomain.QuotaSourceUpstream, SyncedAt: &now},
		{Mode: "console_image", Source: accountdomain.QuotaSourceUpstream, SyncedAt: &now},
	}
	if completeConsoleUsageSnapshot(partial) {
		t.Fatal("partial Console usage snapshot was accepted")
	}
	complete := append(partial, accountdomain.QuotaWindow{Mode: "console_video", Source: accountdomain.QuotaSourceUpstream, SyncedAt: &now})
	if !completeConsoleUsageSnapshot(complete) {
		t.Fatal("complete Console usage snapshot was rejected")
	}
}

func TestConsoleQuotaWindowsControlTheirMatchingRoutes(t *testing.T) {
	now := time.Now().UTC()
	for _, mode := range []string{"console", "console_image", "console_video"} {
		if !quotaWindowControlsRouting(accountdomain.ProviderConsole, mode) {
			t.Fatalf("%s must control its matching Console route", mode)
		}
		window := accountdomain.QuotaWindow{Mode: mode, Remaining: 0, Source: accountdomain.QuotaSourceUpstream}
		if !accountdomain.QuotaWindowDeservesRecovery(accountdomain.ProviderConsole, window, now) {
			t.Fatalf("%s must schedule a predicted recovery probe", mode)
		}
		deadline, ok := accountdomain.QuotaWindowProbeAt(window, now)
		if !ok || !deadline.Equal(now.Add(accountdomain.ConsolePredictedQuotaProbeDelay)) {
			t.Fatalf("%s deadline = %v, ok = %v", mode, deadline, ok)
		}
	}
	if quotaWindowControlsRouting(accountdomain.ProviderConsole, "unknown") {
		t.Fatal("unknown Console quota mode must not control routing")
	}
	if !quotaWindowControlsRouting(accountdomain.ProviderWeb, "weekly") {
		t.Fatal("Web quota behavior must remain unchanged")
	}
}
