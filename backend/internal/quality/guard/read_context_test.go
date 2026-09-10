package guard

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

type guardReadFailure struct{ err error }

func (s guardReadFailure) LoadGuard(context.Context) (Config, bool, error) {
	return Config{}, false, s.err
}
func (s guardReadFailure) SaveGuard(context.Context, Config) error { return s.err }

func TestGuardReadFailureBelongsToCallerOrAuthority(t *testing.T) {
	for _, mode := range []string{"caller_cancel", "caller_deadline", "store_cancel", "store_deadline", "store_failure", "caller_cancel_store_failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			cause := errors.New("authority unavailable")
			own := false
			switch mode {
			case "caller_cancel":
				child, cancel := context.WithCancel(ctx)
				cancel()
				ctx = child
				cause = ctx.Err()
				own = true
			case "caller_deadline":
				child, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				ctx = child
				cause = ctx.Err()
				own = true
			case "caller_cancel_store_failure":
				child, cancel := context.WithCancel(ctx)
				cancel()
				ctx = child
			case "store_cancel":
				cause = context.Canceled
			case "store_deadline":
				cause = context.DeadlineExceeded
			}
			service := New(DefaultConfig(), guardReadFailure{fmt.Errorf("load guard document: %w", cause)})
			before, err := service.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Read(ctx); !errors.Is(err, cause) {
				t.Fatalf("read=%v", err)
			}
			after, snapshotErr := service.Snapshot()
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed read changed installed policy")
			}
			if own && snapshotErr != nil {
				t.Fatalf("caller lifetime poisoned unrelated requests: %v", snapshotErr)
			}
			if !own && !errors.Is(snapshotErr, cause) {
				t.Fatalf("authority failure left policy ready: %v", snapshotErr)
			}
		})
	}
}

func TestCallerReadCancellationCannotHealUnavailablePolicy(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			broken := errors.New("durable guard unavailable")
			service := New(DefaultConfig(), guardReadFailure{broken})
			if err := service.LoadPersisted(context.Background()); !errors.Is(err, broken) {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			} else {
				cancel()
			}
			defer cancel()
			service.store = guardReadFailure{ctx.Err()}
			if _, err := service.Read(ctx); !errors.Is(err, ctx.Err()) {
				t.Fatal(err)
			}
			if _, err := service.Snapshot(); !errors.Is(err, broken) {
				t.Fatalf("caller departure healed failed policy: %v", err)
			}
			service.store = &fakeGuardStore{saved: map[string]Config{"only": DefaultConfig()}}
			if err := service.LoadPersisted(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Snapshot(); err != nil {
				t.Fatalf("healthy authority did not restore policy: %v", err)
			}
		})
	}
}
