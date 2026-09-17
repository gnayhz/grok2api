package account

import (
	"testing"
	"time"
)

func TestQuotaWindowDeservesRecoveryRequiresRoutingExhaustionAndDeadline(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	past := now.Add(-time.Minute)
	cases := []struct {
		name     string
		provider Provider
		window   QuotaWindow
		want     bool
	}{
		{
			name: "console usage without reset timestamp keeps the prediction window", provider: ProviderConsole,
			window: QuotaWindow{Mode: QuotaModeConsole, Remaining: 0, Source: QuotaSourceUpstream}, want: true,
		},
		{
			name: "console image without reset timestamp keeps the prediction window", provider: ProviderConsole,
			window: QuotaWindow{Mode: QuotaModeConsoleImage, Remaining: 0, Source: QuotaSourceUpstream}, want: true,
		},
		{
			name: "console video without reset timestamp keeps the prediction window", provider: ProviderConsole,
			window: QuotaWindow{Mode: QuotaModeConsoleVideo, Remaining: 0, Source: QuotaSourceUpstream}, want: true,
		},
		{
			name: "expired console usage deadline falls back to the prediction window", provider: ProviderConsole,
			window: QuotaWindow{Mode: QuotaModeConsoleImage, Remaining: 0, ResetAt: &past, Source: QuotaSourceUpstream}, want: true,
		},
		{
			name: "console billing snapshot never owns a recovery event", provider: ProviderConsole,
			window: QuotaWindow{Mode: "billing", Remaining: 0, ResetAt: &future, Source: QuotaSourceUpstream}, want: false,
		},
		{
			name: "unknown console mode never owns a recovery event", provider: ProviderConsole,
			window: QuotaWindow{Mode: "unknown", Remaining: 0, ResetAt: &past, Source: QuotaSourceDefault}, want: false,
		},
		{
			name: "web window with explicit deadline", provider: ProviderWeb,
			window: QuotaWindow{Mode: QuotaModeWebFast, Remaining: 0, ResetAt: &future, Source: QuotaSourceUpstream}, want: true,
		},
		{
			name: "remote window without deadline keeps the generic fallback", provider: ProviderWeb,
			window: QuotaWindow{Mode: QuotaModeWebFast, Remaining: 0, Source: QuotaSourceUpstream}, want: true,
		},
		{
			name: "expired remote deadline keeps the generic fallback", provider: ProviderWeb,
			window: QuotaWindow{Mode: QuotaModeWebFast, Remaining: 0, ResetAt: &past, Source: QuotaSourceUpstream}, want: true,
		},
		{
			name: "local window without deadline has nothing to wait for", provider: ProviderWeb,
			window: QuotaWindow{Mode: QuotaModeWebFast, Remaining: 0, Source: QuotaSourceDefault}, want: false,
		},
		{
			name: "expired local deadline has nothing to wait for", provider: ProviderWeb,
			window: QuotaWindow{Mode: QuotaModeWebFast, Remaining: 0, ResetAt: &past, Source: QuotaSourceDefault}, want: false,
		},
		{
			name: "available window is not exhausted", provider: ProviderWeb,
			window: QuotaWindow{Mode: QuotaModeWebFast, Remaining: 1, ResetAt: &future, Source: QuotaSourceUpstream}, want: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := QuotaWindowDeservesRecovery(testCase.provider, testCase.window, now); got != testCase.want {
				t.Fatalf("QuotaWindowDeservesRecovery = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestQuotaWindowProbeAtOwnsTheBoundedRetryDeadline(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	explicit := now.Add(90 * time.Minute)
	if deadline, ok := QuotaWindowProbeAt(QuotaWindow{Mode: QuotaModeWebFast, Remaining: 0, ResetAt: &explicit}, now); !ok || !deadline.Equal(explicit) {
		t.Fatalf("explicit deadline = %v, ok = %v", deadline, ok)
	}
	// Console usage windows have no upstream reset timestamp: the fixed 24-hour
	// prediction window is the only deadline they can offer.
	for _, mode := range []string{QuotaModeConsole, QuotaModeConsoleImage, QuotaModeConsoleVideo} {
		deadline, ok := QuotaWindowProbeAt(QuotaWindow{Mode: mode, Remaining: 0, Source: QuotaSourceUpstream}, now)
		if !ok || !deadline.Equal(now.Add(24*time.Hour)) {
			t.Fatalf("%s deadline = %v, ok = %v", mode, deadline, ok)
		}
		expired := now.Add(-time.Hour)
		deadline, ok = QuotaWindowProbeAt(QuotaWindow{Mode: mode, Remaining: 0, ResetAt: &expired, Source: QuotaSourceUpstream}, now)
		if !ok || !deadline.Equal(now.Add(24*time.Hour)) {
			t.Fatalf("%s expired deadline = %v, ok = %v", mode, deadline, ok)
		}
	}
	deadline, ok := QuotaWindowProbeAt(QuotaWindow{Mode: QuotaModeWebHeavy, Remaining: 0, Source: QuotaSourceUpstream}, now)
	if !ok || !deadline.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("remote fallback = %v, ok = %v", deadline, ok)
	}
	if _, ok := QuotaWindowProbeAt(QuotaWindow{Mode: QuotaModeWebHeavy, Remaining: 0, Source: QuotaSourceDefault}, now); ok {
		t.Fatal("local window without deadline must not report a probe time")
	}
	if ConsolePredictedQuotaProbeDelay != 24*time.Hour {
		t.Fatalf("ConsolePredictedQuotaProbeDelay = %s", ConsolePredictedQuotaProbeDelay)
	}
}
