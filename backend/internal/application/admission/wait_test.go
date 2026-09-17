package admission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitCommitDisarmsOnlyAdmissionTimer(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	a := NewWait(parent, time.Now(), 20*time.Millisecond)
	defer a.Close()
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Context().Done():
		t.Fatal("admitted stream canceled by admission")
	case <-time.After(45 * time.Millisecond):
	}
	cancel()
	<-a.Context().Done()
	if !errors.Is(a.Failure(), context.Canceled) {
		t.Fatal("parent cancellation lost")
	}
}

func TestWaitExpiryCannotRaceIntoDelivery(t *testing.T) {
	for i := 0; i < 100; i++ {
		a := NewWait(context.Background(), time.Now().Add(-time.Second), time.Millisecond)
		if err := a.Commit(); err == nil {
			t.Fatal("expired response committed")
		}
		a.Close()
	}
}

func TestBoundRetryRejectsWhenNoNextAccount(t *testing.T) {
	if BoundRetry(QualityActionRetry, false) != QualityActionReject {
		t.Fatal("retry without next account must reject")
	}
	if BoundRetry(QualityActionRetry, true) != QualityActionRetry {
		t.Fatal("retry with next account must stay retry")
	}
	commit := CommitHold(QualityWithhold, 1, 2, false)
	if commit.Action != QualityActionReject || commit.KeepBody {
		t.Fatalf("exhausted hold: %+v", commit)
	}
}
