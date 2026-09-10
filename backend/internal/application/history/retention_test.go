package history

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type retentionResponseStore struct {
	repository.ResponseRepository
	remove func(context.Context, time.Time, int, int) (repository.ResponseCleanupResult, error)
}

func (s retentionResponseStore) DeleteExpired(ctx context.Context, now time.Time, owner, web int) (repository.ResponseCleanupResult, error) {
	return s.remove(ctx, now, owner, web)
}

type retentionJournal struct {
	repository.ConversationJournal
	prune func(context.Context, time.Time, int) (int64, error)
}

func (j retentionJournal) Prune(ctx context.Context, now time.Time, n int) (int64, error) {
	return j.prune(ctx, now, n)
}

type retentionLock struct {
	acquire func(context.Context, string, time.Duration) (func(), bool, error)
}

func (l retentionLock) Acquire(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	return l.acquire(ctx, key, ttl)
}

func TestHistoryRetentionBoundsFailuresAndMetrics(t *testing.T) {
	for _, scenario := range []string{"complete", "maximum", "deadline", "parent_cancel", "store_failure", "lock_busy", "lock_failure"} {
		t.Run(scenario, func(t *testing.T) {
			perfmetrics.Default.CollectAndReset()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var log bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&log, nil))
			cause := errors.New("retention unavailable")
			calls, releases := 0, 0
			now := time.Now().In(time.FixedZone("positive", 8*3600))
			lock := retentionLock{func(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
				if key != "response-ownership-cleanup" || ttl != 2*time.Minute {
					t.Fatal("lock policy changed")
				}
				if scenario == "lock_failure" {
					return nil, false, cause
				}
				if scenario == "lock_busy" {
					return nil, false, nil
				}
				return func() { releases++ }, true, nil
			}}
			store := retentionResponseStore{remove: func(callCtx context.Context, at time.Time, owner, web int) (repository.ResponseCleanupResult, error) {
				calls++
				deadline, ok := callCtx.Deadline()
				if !ok || time.Until(deadline) > 30*time.Second || time.Until(deadline) < 28*time.Second {
					t.Fatal("cleanup missing finite budget")
				}
				if owner != 1000 || web != 50 || !at.Equal(now) || at.Location() != time.UTC {
					t.Fatal("cleanup policy changed")
				}
				if calls == 2 {
					switch scenario {
					case "deadline":
						return repository.ResponseCleanupResult{}, context.DeadlineExceeded
					case "parent_cancel":
						cancel()
						return repository.ResponseCleanupResult{}, context.Canceled
					case "store_failure":
						return repository.ResponseCleanupResult{}, cause
					}
				}
				return repository.ResponseCleanupResult{OwnershipDeleted: 7, WebStateDeleted: 3, HasMore: scenario != "complete" || calls < 3}, nil
			}}
			err := NewRetention(store, nil, lock, logger).CleanupResponses(ctx, now)
			wantCalls, wantReleases, wantMetricCalls := 3, 1, 3
			switch scenario {
			case "maximum":
				wantCalls, wantMetricCalls = 100, 100
			case "deadline":
				wantCalls, wantMetricCalls = 2, 1
			case "parent_cancel", "store_failure":
				wantCalls, wantMetricCalls = 2, 0
			case "lock_busy", "lock_failure":
				wantCalls, wantReleases, wantMetricCalls = 0, 0, 0
			}
			switch scenario {
			case "parent_cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "store_failure", "lock_failure":
				if !errors.Is(err, cause) {
					t.Fatal(err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			if calls != wantCalls || releases != wantReleases {
				t.Fatalf("calls=%d release=%d", calls, releases)
			}
			samples := perfmetrics.Default.CollectAndReset()
			totals := map[string]int64{}
			for _, sample := range samples {
				if sample.Labels.Subsystem == "response" && sample.Labels.Operation == "cleanup" {
					totals[sample.Name] = sample.Total
					wantOutcome := "complete"
					if scenario == "maximum" || scenario == "deadline" {
						wantOutcome = "backlog"
					}
					if sample.Labels.Outcome != wantOutcome {
						t.Fatal(sample.Labels)
					}
				}
			}
			if totals["response_cleanup_ownership_rows"] != int64(7*wantMetricCalls) || totals["response_cleanup_web_state_rows"] != int64(3*wantMetricCalls) {
				t.Fatal(totals)
			}
			if strings.Contains(log.String(), "response_cleanup_backlog") != (scenario == "maximum" || scenario == "deadline") {
				t.Fatal(log.String())
			}
		})
	}
}
func TestHistoryRetentionSharedLockAndJournalCancel(t *testing.T) {
	lock := memory.NewLockStore()
	release, acquired, err := lock.Acquire(context.Background(), "response-ownership-cleanup", time.Minute)
	if err != nil || !acquired {
		t.Fatal(err)
	}
	calls := 0
	store := retentionResponseStore{remove: func(context.Context, time.Time, int, int) (repository.ResponseCleanupResult, error) {
		calls++
		return repository.ResponseCleanupResult{}, nil
	}}
	first, second := NewRetention(store, nil, lock, nil), NewRetention(store, nil, lock, nil)
	if err = first.CleanupResponses(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("ignored shared lock")
	}
	release()
	if err = second.CleanupResponses(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	_, acquired, err = lock.Acquire(context.Background(), "response-ownership-cleanup", time.Minute)
	if err != nil || !acquired {
		t.Fatal("cleanup retained lock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	journal := retentionJournal{prune: func(callCtx context.Context, now time.Time, limit int) (int64, error) {
		if limit != 100 || now.Location() != time.UTC {
			t.Fatal("journal policy changed")
		}
		if deadline, ok := callCtx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("journal missing deadline")
		}
		close(entered)
		<-callCtx.Done()
		return 0, callCtx.Err()
	}}
	done := make(chan error, 1)
	go func() { done <- NewRetention(store, journal, nil, nil).CleanupConversations(ctx, time.Now()) }()
	<-entered
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if ResponseCleanupInterval != 5*time.Minute || ConversationCleanupInterval != 10*time.Minute {
		t.Fatal("schedule policy changed")
	}
}
