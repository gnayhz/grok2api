package quotarecovery

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRunDueClaimsBoundedBatchAndProcessesConcurrently(t *testing.T) {
	now := time.Now().UTC()
	events := make([]accountdomain.QuotaRecoveryEvent, defaultRecoveryWorkers)
	for index := range events {
		events[index] = accountdomain.QuotaRecoveryEvent{AccountID: uint64(index + 1), Mode: "fast", DueAt: now}
	}
	queue := &quotaQueueStub{claimed: events}
	syncer := &quotaSyncStub{}
	service := NewService(testLogger(), queue, syncer, time.Second, time.Minute)

	service.runDue(context.Background(), now)

	if queue.claimLimit != defaultRecoveryWorkers || queue.claimLease != recoveryClaimLease {
		t.Fatalf("claim limit = %d, lease = %s", queue.claimLimit, queue.claimLease)
	}
	// 并发高水位是调度依赖量：满载机器上 25 个 worker 未必全部同时在飞
	//（规模轮 130 verify-full 实测 20/25 触发误报）。语义断言取两侧：
	// 确实并发（>1）且被池上界约束（<= workers）。
	if syncer.maxConcurrent <= 1 || syncer.maxConcurrent > defaultRecoveryWorkers {
		t.Fatalf("max concurrent probes = %d (want (1, %d])", syncer.maxConcurrent, defaultRecoveryWorkers)
	}
	if queue.acked != defaultRecoveryWorkers {
		t.Fatalf("acked events = %d", queue.acked)
	}
}

func TestReconcileDueRestoresMissingQueueEvents(t *testing.T) {
	now := time.Now().UTC()
	queue := &quotaQueueStub{}
	syncer := &quotaSyncStub{due: []accountdomain.QuotaWindow{{AccountID: 7, Mode: "expert", Remaining: 0, ResetAt: &now}}}
	service := NewService(testLogger(), queue, syncer, time.Second, time.Minute)

	service.reconcileDue(context.Background(), now)

	if len(queue.scheduled) != 1 || queue.scheduled[0].AccountID != 7 || queue.scheduled[0].Mode != "expert" {
		t.Fatalf("scheduled = %#v", queue.scheduled)
	}
}

// TestReconcileDueSkipsWindowsOutsideRoutingControl 锁定巡检 ticker 也遵守
// domain owner 的资格规则：存储层的"已耗尽且已到期"只是粗略到期上界，不再
// 是资格判定。此前旧 Console 账单窗口(billing/unknown，不参与路由控制)能借
// 这条路径排入恢复队列，绕过启动回放与刷新路径共用的规则。
func TestReconcileDueSkipsWindowsOutsideRoutingControl(t *testing.T) {
	now := time.Now().UTC()
	queue := &quotaQueueStub{}
	syncer := &quotaSyncStub{due: []accountdomain.QuotaWindow{
		{AccountID: 1, Provider: accountdomain.ProviderWeb, Mode: "expert", Remaining: 0, ResetAt: &now},
		{AccountID: 2, Provider: accountdomain.ProviderConsole, Mode: "console", Remaining: 0, ResetAt: &now},
		{AccountID: 3, Provider: accountdomain.ProviderConsole, Mode: "console_image", Remaining: 0, ResetAt: &now},
		{AccountID: 4, Provider: accountdomain.ProviderConsole, Mode: "console_video", Remaining: 0, ResetAt: &now},
		// 不参与路由控制的 Console 非用量窗口：不得产生恢复事件。
		{AccountID: 5, Provider: accountdomain.ProviderConsole, Mode: "billing", Remaining: 0, ResetAt: &now},
		{AccountID: 6, Provider: accountdomain.ProviderConsole, Mode: "unknown", Remaining: 0, ResetAt: &now},
	}}
	service := NewService(testLogger(), queue, syncer, time.Second, time.Minute)

	service.reconcileDue(context.Background(), now)

	got := make([]uint64, 0, len(queue.scheduled))
	for _, event := range queue.scheduled {
		got = append(got, event.AccountID)
	}
	want := []uint64{1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("scheduled account ids = %v, want %v", got, want)
	}
	for index, id := range want {
		if got[index] != id {
			t.Fatalf("scheduled account ids = %v, want %v", got, want)
		}
	}
}

type pagedQuotaSync struct {
	quotaSyncStub
	windows []accountdomain.QuotaWindow
}

func (s *pagedQuotaSync) ListDueQuotaWindows(_ context.Context, _ time.Time, limit int, after *repository.QuotaWindowCursor) ([]accountdomain.QuotaWindow, error) {
	var out []accountdomain.QuotaWindow
	for _, window := range s.windows {
		if after != nil && window.AccountID <= after.AccountID {
			continue
		}
		out = append(out, window)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func TestReconcileDueReachesEligibleAccountsPastFullIneligiblePage(t *testing.T) {
	now := time.Now().UTC()
	syncer := &pagedQuotaSync{}
	for i := 1; i <= recoveryReconcileLimit; i++ {
		syncer.windows = append(syncer.windows, accountdomain.QuotaWindow{
			AccountID: uint64(i), Provider: accountdomain.ProviderConsole, Mode: "billing", ResetAt: &now,
		})
	}
	syncer.windows = append(syncer.windows, accountdomain.QuotaWindow{
		AccountID: recoveryReconcileLimit + 1, Provider: accountdomain.ProviderWeb, Mode: "fast", ResetAt: &now,
	})
	queue := &quotaQueueStub{}
	service := NewService(testLogger(), queue, syncer, time.Second, time.Minute)
	service.reconcileDue(context.Background(), now)
	if len(queue.scheduled) != 0 {
		t.Fatalf("ineligible windows scheduled: %#v", queue.scheduled)
	}
	service.reconcileDue(context.Background(), now)
	if len(queue.scheduled) != 1 || queue.scheduled[0].AccountID != recoveryReconcileLimit+1 {
		t.Fatalf("eligible tail starved behind ineligible windows: %#v", queue.scheduled)
	}
	// A newly due low-ID window must be visited when the scan wraps.
	syncer.windows[0].Mode = "console"
	service.reconcileDue(context.Background(), now)
	if len(queue.scheduled) != 2 || queue.scheduled[1].AccountID != 1 {
		t.Fatalf("scan did not wrap to newly eligible windows: %#v", queue.scheduled)
	}
}

func TestRunOneKeepsPredictedRecoveryWindow(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	queue := &quotaQueueStub{}
	syncer := &quotaSyncStub{window: accountdomain.QuotaWindow{Mode: "console", Remaining: 0}}
	service := NewService(testLogger(), queue, syncer, 30*time.Second, 30*time.Minute)

	service.runOne(context.Background(), now, accountdomain.QuotaRecoveryEvent{AccountID: 7, Mode: "console", DueAt: now, ClaimToken: "claim"})

	if len(queue.rescheduled) != 1 || !queue.rescheduled[0].DueAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("rescheduled = %#v", queue.rescheduled)
	}
}

func TestRunOnePreservesNonConsoleWindowDuration(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	queue := &quotaQueueStub{}
	syncer := &quotaSyncStub{window: accountdomain.QuotaWindow{Mode: "fast", Remaining: 0, WindowSeconds: 3600}}
	service := NewService(testLogger(), queue, syncer, 30*time.Second, 30*time.Minute)

	service.runOne(context.Background(), now, accountdomain.QuotaRecoveryEvent{AccountID: 8, Mode: "fast", DueAt: now, ClaimToken: "claim"})

	if len(queue.rescheduled) != 1 || !queue.rescheduled[0].DueAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("rescheduled = %#v", queue.rescheduled)
	}
}

// TestRunOneUsesFixedPredictionWindowForConsoleUsageModes 固定 24 小时预测窗口属于
// 全部 Console 用量模式(chat/image/video)；非用量模式不得落到该分支。
func TestRunOneUsesFixedPredictionWindowForConsoleUsageModes(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	for _, mode := range []string{"console", "console_image", "console_video"} {
		t.Run(mode, func(t *testing.T) {
			queue := &quotaQueueStub{}
			syncer := &quotaSyncStub{window: accountdomain.QuotaWindow{Mode: mode, Remaining: 0}}
			service := NewService(testLogger(), queue, syncer, 30*time.Second, 30*time.Minute)

			service.runOne(context.Background(), now, accountdomain.QuotaRecoveryEvent{AccountID: 7, Mode: mode, DueAt: now, ClaimToken: "claim"})

			if len(queue.rescheduled) != 1 || !queue.rescheduled[0].DueAt.Equal(now.Add(24*time.Hour)) {
				t.Fatalf("%s rescheduled = %#v", mode, queue.rescheduled)
			}
		})
	}
	for _, mode := range []string{"fast", "billing"} {
		t.Run(mode, func(t *testing.T) {
			queue := &quotaQueueStub{}
			syncer := &quotaSyncStub{window: accountdomain.QuotaWindow{Mode: mode, Remaining: 0}}
			service := NewService(testLogger(), queue, syncer, 30*time.Second, 30*time.Minute)

			service.runOne(context.Background(), now, accountdomain.QuotaRecoveryEvent{AccountID: 8, Mode: mode, DueAt: now, ClaimToken: "claim"})

			if len(queue.rescheduled) != 1 || !queue.rescheduled[0].DueAt.Equal(now.Add(30*time.Second)) {
				t.Fatalf("%s rescheduled = %#v", mode, queue.rescheduled)
			}
			if queue.rescheduled[0].DueAt.Equal(now.Add(24 * time.Hour)) {
				t.Fatalf("%s must not use the fixed Console prediction window", mode)
			}
		})
	}
}

type quotaQueueStub struct {
	mu          sync.Mutex
	claimed     []accountdomain.QuotaRecoveryEvent
	claimLimit  int
	claimLease  time.Duration
	acked       int
	scheduled   []accountdomain.QuotaRecoveryEvent
	ensured     []accountdomain.QuotaRecoveryEvent
	rescheduled []accountdomain.QuotaRecoveryEvent
	cancelled   []accountdomain.QuotaRecoveryEvent
}

func (q *quotaQueueStub) EnsureQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.mu.Lock()
	q.ensured = append(q.ensured, value)
	q.scheduled = append(q.scheduled, value)
	q.mu.Unlock()
	return nil
}

func (q *quotaQueueStub) ScheduleQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.mu.Lock()
	q.scheduled = append(q.scheduled, value)
	q.mu.Unlock()
	return nil
}

func (q *quotaQueueStub) CancelQuotaRecovery(_ context.Context, accountID uint64, mode string) error {
	q.mu.Lock()
	q.cancelled = append(q.cancelled, accountdomain.QuotaRecoveryEvent{AccountID: accountID, Mode: mode})
	q.mu.Unlock()
	return nil
}

func (q *quotaQueueStub) ClaimDueQuotaRecoveries(_ context.Context, _ time.Time, limit int, lease time.Duration) ([]accountdomain.QuotaRecoveryEvent, error) {
	q.claimLimit, q.claimLease = limit, lease
	return q.claimed, nil
}

func (q *quotaQueueStub) AckQuotaRecovery(_ context.Context, _ accountdomain.QuotaRecoveryEvent) error {
	q.mu.Lock()
	q.acked++
	q.mu.Unlock()
	return nil
}

func (q *quotaQueueStub) RescheduleQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.mu.Lock()
	q.rescheduled = append(q.rescheduled, value)
	q.mu.Unlock()
	return nil
}

type quotaSyncStub struct {
	mu            sync.Mutex
	current       int
	maxConcurrent int
	due           []accountdomain.QuotaWindow
	window        accountdomain.QuotaWindow
}

func (s *quotaSyncStub) ProbeQuotaMode(_ context.Context, accountID uint64, mode string) (accountdomain.QuotaWindow, error) {
	s.mu.Lock()
	s.current++
	if s.current > s.maxConcurrent {
		s.maxConcurrent = s.current
	}
	s.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	s.mu.Lock()
	s.current--
	s.mu.Unlock()
	if s.window.Mode != "" {
		window := s.window
		window.AccountID = accountID
		return window, nil
	}
	return accountdomain.QuotaWindow{AccountID: accountID, Mode: mode, Remaining: 1}, nil
}

func (s *quotaSyncStub) ListDueQuotaWindows(context.Context, time.Time, int, *repository.QuotaWindowCursor) ([]accountdomain.QuotaWindow, error) {
	return s.due, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
