package account

import (
	"context"
	"reflect"
	"strconv"
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
	if due, ok := accountdomain.QuotaWindowProbeAt(predicted, observed); !ok || !due.Equal(*predicted.ResetAt) {
		t.Fatal("queue differs from persisted prediction")
	}
	if due, ok := accountdomain.QuotaWindowProbeAt(actual, observed); !ok || !due.Equal(explicit) {
		t.Fatal("explicit reset not honored")
	}
}

// TestReconcileQuotaRecoveryWindowSchedulesExactlyEligibleWindows 证明对账入口与
// domain owner 的恢复资格规则一致：同一窗口集合无论从启动重放还是从对账进入,
// 都被安排或取消同样的恢复事件。
func TestReconcileQuotaRecoveryWindowSchedulesExactlyEligibleWindows(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	past := now.Add(-time.Minute)
	cases := []struct {
		name     string
		provider accountdomain.Provider
		window   accountdomain.QuotaWindow
	}{
		{"console chat prediction", accountdomain.ProviderConsole, accountdomain.QuotaWindow{Mode: accountdomain.QuotaModeConsole, Remaining: 0, Source: accountdomain.QuotaSourceUpstream}},
		{"console image prediction", accountdomain.ProviderConsole, accountdomain.QuotaWindow{Mode: accountdomain.QuotaModeConsoleImage, Remaining: 0, Source: accountdomain.QuotaSourceUpstream}},
		{"console video prediction", accountdomain.ProviderConsole, accountdomain.QuotaWindow{Mode: accountdomain.QuotaModeConsoleVideo, Remaining: 0, Source: accountdomain.QuotaSourceUpstream}},
		{"console billing snapshot", accountdomain.ProviderConsole, accountdomain.QuotaWindow{Mode: "billing", Remaining: 0, ResetAt: &future, Source: accountdomain.QuotaSourceUpstream}},
		{"web explicit deadline", accountdomain.ProviderWeb, accountdomain.QuotaWindow{Mode: accountdomain.QuotaModeWebFast, Remaining: 0, ResetAt: &future, Source: accountdomain.QuotaSourceUpstream}},
		{"web remote fallback", accountdomain.ProviderWeb, accountdomain.QuotaWindow{Mode: accountdomain.QuotaModeWebFast, Remaining: 0, Source: accountdomain.QuotaSourceUpstream}},
		{"web expired remote deadline", accountdomain.ProviderWeb, accountdomain.QuotaWindow{Mode: accountdomain.QuotaModeWebFast, Remaining: 0, ResetAt: &past, Source: accountdomain.QuotaSourceUpstream}},
		{"web local without deadline", accountdomain.ProviderWeb, accountdomain.QuotaWindow{Mode: accountdomain.QuotaModeWebFast, Remaining: 0, Source: accountdomain.QuotaSourceDefault}},
		{"available web window", accountdomain.ProviderWeb, accountdomain.QuotaWindow{Mode: accountdomain.QuotaModeWebFast, Remaining: 3, ResetAt: &future, Source: accountdomain.QuotaSourceUpstream}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service := NewService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			service.now = func() time.Time { return now }
			queue := &reconcileQueueStub{}
			service.SetQuotaRecoveryQueue(queue)
			if err := service.reconcileQuotaRecoveryWindow(context.Background(), testCase.provider, 11, testCase.window); err != nil {
				t.Fatal(err)
			}
			eligible := accountdomain.QuotaWindowDeservesRecovery(testCase.provider, testCase.window, now)
			if scheduled := len(queue.scheduled) == 1; scheduled != eligible {
				t.Fatalf("scheduled = %v, eligible = %v (%#v)", scheduled, eligible, queue.scheduled)
			}
			if !eligible {
				if len(queue.cancelled) != 1 || queue.cancelled[0] != "11:"+testCase.window.Mode {
					t.Fatalf("cancelled = %#v", queue.cancelled)
				}
				return
			}
			want, ok := accountdomain.QuotaWindowProbeAt(testCase.window, now)
			if !ok {
				t.Fatal("eligible window must own a bounded probe deadline")
			}
			event := queue.scheduled[0]
			if event.AccountID != 11 || event.Mode != testCase.window.Mode || !event.DueAt.Equal(want) {
				t.Fatalf("scheduled = %#v, want due %v", event, want)
			}
		})
	}
}

type reconcileQueueStub struct {
	scheduled []accountdomain.QuotaRecoveryEvent
	cancelled []string
}

func (q *reconcileQueueStub) ScheduleQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.scheduled = append(q.scheduled, value)
	return nil
}

func (q *reconcileQueueStub) EnsureQuotaRecovery(context.Context, accountdomain.QuotaRecoveryEvent) error {
	return nil
}

func (q *reconcileQueueStub) CancelQuotaRecovery(_ context.Context, accountID uint64, mode string) error {
	q.cancelled = append(q.cancelled, strconv.FormatUint(accountID, 10)+":"+mode)
	return nil
}

func (q *reconcileQueueStub) ClaimDueQuotaRecoveries(context.Context, time.Time, int, time.Duration) ([]accountdomain.QuotaRecoveryEvent, error) {
	return nil, nil
}

func (q *reconcileQueueStub) AckQuotaRecovery(context.Context, accountdomain.QuotaRecoveryEvent) error {
	return nil
}

func (q *reconcileQueueStub) RescheduleQuotaRecovery(context.Context, accountdomain.QuotaRecoveryEvent) error {
	return nil
}
