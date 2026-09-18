package events

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"reflect"
	"testing"
	"time"
)

type retryReceiptJournal struct {
	Journal
	calls  [][]model.Event
	err    error
	cancel context.CancelFunc
}

func (j *retryReceiptJournal) RecordMany(_ context.Context, v []model.Event) error {
	j.calls = append(j.calls, append([]model.Event(nil), v...))
	if j.cancel != nil {
		j.cancel()
	}
	if len(j.calls) == 1 {
		return j.err
	}
	return nil
}
func TestReceiptRetryRetainsExactFactAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		cancel bool
		calls  int
	}{
		{"lock", &repository.StoreFault{Kind: repository.StoreFaultLock}, false, 2},
		{"unknown", errors.New("synthetic error"), false, 1},
		{"conflict", &repository.StoreFault{Kind: repository.StoreFaultConstraint}, false, 1},
		{"cancel", &repository.StoreFault{Kind: repository.StoreFaultLock}, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			j := &retryReceiptJournal{err: tc.err}
			if tc.cancel {
				j.cancel = cancel
			}
			s := New(j, nil, nil)
			err := s.RecordQualityEvent(ctx, Receipt{Attempt: attemptmeta.Identity{ID: "synthetic/1", AccountID: 7}, Outcome: Degraded, At: time.Now().UTC()}, time.Minute)
			if len(j.calls) != tc.calls || tc.calls == 2 && err != nil || tc.calls == 1 && err == nil {
				t.Fatalf("calls=%d err=%v", len(j.calls), err)
			}
			if len(j.calls) == 2 && !reflect.DeepEqual(j.calls[0], j.calls[1]) {
				t.Fatal("retry changed receipt or restriction expiry")
			}
		})
	}
}
