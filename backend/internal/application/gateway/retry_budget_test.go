package gateway

import (
	"context"
	"testing"
	"time"
)

func TestRetryBudgetKeepsDeadlineAndAcceptedDelivery(t *testing.T) {
	for _, tc := range []struct {
		budget time.Duration
		want   bool
	}{{0, true}, {time.Second, true}, {100 * time.Millisecond, false}} {
		a := newAdmission(context.Background(), time.Now(), tc.budget)
		if got := hasRetryAdmissionBudget(a); got != tc.want {
			t.Fatalf("budget=%s allowed=%v", tc.budget, got)
		}
		if err := a.commit(); err != nil {
			t.Fatal(err)
		}
		if remaining, bounded := a.Remaining(); bounded {
			t.Fatalf("accepted stream retained timer %s", remaining)
		}
		a.close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	a := newAdmission(ctx, time.Now(), time.Second)
	defer a.close()
	if hasRetryAdmissionBudget(a) {
		t.Fatal("ignored shorter parent deadline")
	}
}
