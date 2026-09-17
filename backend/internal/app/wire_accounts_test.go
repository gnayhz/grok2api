package app

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestReplayQuotaRecoveryWindowsSchedulesExactlyEligibleWindows 锁定启动重放与
// domain owner 资格规则的一致性：同一候选窗口集合无论从启动重放还是从
// application/account 的对账入口进入，都安排同样的恢复事件。
func TestReplayQuotaRecoveryWindowsSchedulesExactlyEligibleWindows(t *testing.T) {
	now := time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC)
	future := now.Add(3 * time.Hour)
	windows := []accountdomain.QuotaWindow{
		{AccountID: 1, Provider: accountdomain.ProviderWeb, Mode: accountdomain.QuotaModeWebFast, Remaining: 0, ResetAt: &future, Source: accountdomain.QuotaSourceUpstream},
		// 旧规则会跳过没有 reset 时间戳的 Console 用量窗口,owner 规则给出固定预测期限。
		{AccountID: 2, Provider: accountdomain.ProviderConsole, Mode: accountdomain.QuotaModeConsoleImage, Remaining: 0, Source: accountdomain.QuotaSourceUpstream},
		// 旧规则会重放 Console 计费快照,owner 规则将其排除在恢复事件之外。
		{AccountID: 3, Provider: accountdomain.ProviderConsole, Mode: "billing", Remaining: 0, ResetAt: &future, Source: accountdomain.QuotaSourceUpstream},
		{AccountID: 4, Provider: accountdomain.ProviderWeb, Mode: accountdomain.QuotaModeWebHeavy, Remaining: 0, Source: accountdomain.QuotaSourceDefault},
		{AccountID: 5, Provider: accountdomain.ProviderWeb, Mode: accountdomain.QuotaModeWebAuto, Remaining: 4, ResetAt: &future, Source: accountdomain.QuotaSourceUpstream},
		{AccountID: 6, Provider: accountdomain.ProviderWeb, Mode: accountdomain.QuotaModeWebExpert, Remaining: 0, Source: accountdomain.QuotaSourceUpstream},
	}
	queue := &replayQueueStub{}
	restored, err := replayQuotaRecoveryWindows(context.Background(), queue, windows, now)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]time.Time)
	for _, window := range windows {
		if !accountdomain.QuotaWindowDeservesRecovery(window.Provider, window, now) {
			continue
		}
		deadline, ok := accountdomain.QuotaWindowProbeAt(window, now)
		if !ok {
			t.Fatalf("eligible window %d:%s must own a bounded probe deadline", window.AccountID, window.Mode)
		}
		want[fmt.Sprintf("%d:%s", window.AccountID, window.Mode)] = deadline
	}
	if len(want) != 3 {
		t.Fatalf("fixture must exercise both eligible and ineligible windows: %#v", want)
	}
	if restored != len(want) {
		t.Fatalf("restored = %d, want %d", restored, len(want))
	}
	got := make(map[string]time.Time)
	for _, event := range queue.scheduled {
		got[fmt.Sprintf("%d:%s", event.AccountID, event.Mode)] = event.DueAt
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed = %#v, want %#v", got, want)
	}
	if got["2:console_image"] != now.Add(24*time.Hour) {
		t.Fatalf("Console image prediction = %v", got["2:console_image"])
	}
	if _, exists := got["3:billing"]; exists {
		t.Fatal("Console billing snapshot must not be replayed")
	}
}

type replayQueueStub struct {
	scheduled []accountdomain.QuotaRecoveryEvent
}

func (q *replayQueueStub) ScheduleQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.scheduled = append(q.scheduled, value)
	return nil
}

func (q *replayQueueStub) EnsureQuotaRecovery(context.Context, accountdomain.QuotaRecoveryEvent) error {
	return nil
}

func (q *replayQueueStub) CancelQuotaRecovery(context.Context, uint64, string) error {
	return nil
}

func (q *replayQueueStub) ClaimDueQuotaRecoveries(context.Context, time.Time, int, time.Duration) ([]accountdomain.QuotaRecoveryEvent, error) {
	return nil, nil
}

func (q *replayQueueStub) AckQuotaRecovery(context.Context, accountdomain.QuotaRecoveryEvent) error {
	return nil
}

func (q *replayQueueStub) RescheduleQuotaRecovery(context.Context, accountdomain.QuotaRecoveryEvent) error {
	return nil
}
