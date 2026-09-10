package court

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestCaseOperationsCancelWhileEvaluationIsBusy(t *testing.T) {
	for _, operation := range []string{"report", "evaluate", "review"} {
		t.Run(operation, func(t *testing.T) {
			s := &Service{}
			if err := s.evaluateMu.Lock(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer s.evaluateMu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, result := make(chan struct{}), make(chan error, 1)
			go func() {
				close(started)
				switch operation {
				case "report":
					result <- s.ReportDegradedAt(ctx, 7, model.EpochKey{}, time.Now())
				case "evaluate":
					_, err := s.Evaluate(ctx, time.Now())
					result <- err
				case "review":
					result <- s.ReleaseAfterReview(ctx, 1, "reviewed")
				}
			}()
			<-started
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("queued operation returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled operation remained blocked behind evaluation")
			}
		})
	}
}

func TestCanceledCaseOperationDoesNotRetainEvaluationLock(t *testing.T) {
	s := &Service{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ReportDegradedAt(ctx, 7, model.EpochKey{}, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled report=%v", err)
	}
	ctx, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	if err := s.ReportDegradedAt(ctx, 7, model.EpochKey{}, time.Now()); err != nil {
		t.Fatalf("subsequent report could not acquire evaluation lock: %v", err)
	}
}
