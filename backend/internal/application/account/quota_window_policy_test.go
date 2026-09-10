package account

import (
	"reflect"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestQuotaTimingPreservesObservedAndExplicitFacts(t *testing.T) {
	observed := time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)
	explicit := observed.Add(3 * time.Hour)
	base := accountdomain.QuotaWindow{AccountID: 7, Mode: "console", Remaining: 0, Total: 10, Source: accountdomain.QuotaSourceUpstream, SyncedAt: &observed}
	predicted := quotaWindowTiming(accountdomain.ProviderConsole, base, observed)
	if predicted.ResetAt == nil || !predicted.ResetAt.Equal(observed.Add(24*time.Hour)) || predicted.WindowSeconds != 86400 {
		t.Fatalf("prediction=%+v", predicted)
	}
	if base.ResetAt != nil || base.WindowSeconds != 0 {
		t.Fatal("input facts mutated")
	}
	if repeated := quotaWindowTiming(accountdomain.ProviderConsole, predicted, observed.Add(time.Hour)); !reflect.DeepEqual(repeated, predicted) {
		t.Fatal("repeat application moved the observation's prediction")
	}
	actual := base
	actual.ResetAt = &explicit
	actual.WindowSeconds = 10800
	if got := quotaWindowTiming(accountdomain.ProviderConsole, actual, observed); !reflect.DeepEqual(got, actual) {
		t.Fatal("explicit timing replaced by prediction")
	}
	available := base
	available.Remaining = 9
	if got := quotaWindowTiming(accountdomain.ProviderConsole, available, observed); got.ResetAt != nil || got.WindowSeconds != 86400 {
		t.Fatalf("available=%+v", got)
	}
	for _, mode := range []string{"console_image", "console_video", "unknown"} {
		window := base
		window.Mode = mode
		if got := quotaWindowTiming(accountdomain.ProviderConsole, window, observed); !reflect.DeepEqual(got, window) {
			t.Fatalf("nonchat metadata changed: %s", mode)
		}
	}
	for _, provider := range []accountdomain.Provider{accountdomain.ProviderBuild, accountdomain.ProviderWeb} {
		if got := quotaWindowTiming(provider, base, observed); !reflect.DeepEqual(got, base) {
			t.Fatalf("other provider changed: %s", provider)
		}
	}
	if due := quotaRecoveryDueAt(predicted, observed, true); due == nil || !due.Equal(*predicted.ResetAt) {
		t.Fatal("queue differs from persisted prediction")
	}
	if due := quotaRecoveryDueAt(actual, observed, true); due == nil || !due.Equal(explicit) {
		t.Fatal("explicit reset not honored")
	}
}
