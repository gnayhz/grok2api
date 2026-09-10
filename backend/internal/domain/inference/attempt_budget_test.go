package inference

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAttemptBudgetConcurrentReservationsAndCancellation(t *testing.T) {
	budget := NewAttemptBudget(7)
	var submitted atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			permit, err := budget.Reserve(t.Context())
			if errors.Is(err, ErrAttemptBudget) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			ok, err := permit.Consume(budget)
			if err != nil || !ok {
				t.Errorf("consume=%t err=%v", ok, err)
				return
			}
			submitted.Add(1)
			permit.Release()
		})
	}
	wg.Wait()
	if submitted.Load() != 7 || budget.Remaining() != 0 {
		t.Fatalf("submissions=%d remaining=%d", submitted.Load(), budget.Remaining())
	}
	spare := NewAttemptBudget(1)
	permit, err := spare.Reserve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	permit.Release()
	permit.Release()
	if spare.Remaining() != 1 {
		t.Fatal("cancel did not release exactly once")
	}
	if _, err := permit.Consume(spare); !errors.Is(err, ErrAttemptBudget) {
		t.Fatalf("released permit err=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := spare.Reserve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	spare.Close()
	if _, err := spare.Reserve(t.Context()); !errors.Is(err, ErrAttemptBudget) {
		t.Fatalf("closed execution restarted: %v", err)
	}
}
