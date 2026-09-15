package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestAdmissionCoversHeadersAndAllRetries(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers", true: "shared_retry_budget"}[retry], func(t *testing.T) {
			adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
			s, accounts := newGuardLoopService(t, adapter, "deadline-first", "deadline-second")
			// Leave room for both real SQL selections under -race. The first
			// response consumes over half the budget, so restarting the timer
			// for its retry would exceed the assertion below.
			const budget = time.Second
			s.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, AdmissionTimeout: budget})
			for _, account := range accounts {
				adapter.responses[account.ID] = []scriptedBuildResponse{{status: 200, headerDelay: 5 * budget}}
			}
			if retry {
				adapter.responses[accounts[0].ID] = []scriptedBuildResponse{{status: 200, headerDelay: 550 * time.Millisecond,
					body: "data: {\"choices\":[{\"delta\":{\"content\":\"bare\"}}]}\n\n"}}
			}
			started := time.Now()
			result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("deadline", true))
			var failure *UpstreamFailure
			if result != nil || !errors.As(err, &failure) || failure.Code != "quality_admission_timeout" {
				t.Fatalf("result=%v err=%v", result, err)
			}
			if elapsed := time.Since(started); elapsed > budget+300*time.Millisecond {
				t.Fatalf("admission budget was multiplied or cleanup blocked: %v", elapsed)
			}
			wantAttempts := 1
			if retry {
				wantAttempts = 2
			}
			if len(adapter.Attempts()) != wantAttempts {
				t.Fatalf("attempts=%v", adapter.Attempts())
			}
			for _, account := range accounts {
				if n, _ := s.selector.concurrency.Current(context.Background(), accountConcurrencyKey(account.ID)); n != 0 {
					t.Fatalf("timeout leaked lease: %d", n)
				}
			}
		})
	}
}

type blockingAdmissionAuthority struct{}

func (blockingAdmissionAuthority) AccountSchedulable(uint64) bool { return true }
func (blockingAdmissionAuthority) CheckAccountAdmission(ctx context.Context, _ uint64) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func TestAdmissionCoversAccountSelection(t *testing.T) {
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	s, _ := newGuardLoopService(t, adapter, "selection-deadline")
	s.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, AdmissionTimeout: 40 * time.Millisecond})
	s.selector.SetQualityEligibility(blockingAdmissionAuthority{})
	result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("selection", true))
	var failure *UpstreamFailure
	if result != nil || !errors.As(err, &failure) || failure.Code != "quality_admission_timeout" || len(adapter.Attempts()) != 0 {
		t.Fatalf("result=%v err=%v attempts=%v", result, err, adapter.Attempts())
	}
}

func TestAdmissionCommitDisarmsOnlyAdmissionTimer(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	a := newAdmission(parent, time.Now(), 20*time.Millisecond)
	defer a.close()
	if err := a.commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.ctx.Done():
		t.Fatal("admitted stream canceled by admission")
	case <-time.After(45 * time.Millisecond):
	}
	cancel()
	<-a.ctx.Done()
	if !errors.Is(a.failure(), context.Canceled) {
		t.Fatal("parent cancellation lost")
	}
}

func TestAdmissionExpiryCannotRaceIntoDelivery(t *testing.T) {
	for i := 0; i < 100; i++ {
		a := newAdmission(context.Background(), time.Now().Add(-time.Second), time.Millisecond)
		if err := a.commit(); err == nil {
			t.Fatal("expired response committed")
		}
		a.close()
	}
}

type resourceTestAdapter struct {
	*scriptedBuildAdapter
	forward func(context.Context, provider.ResponseResourceRequest) (*provider.Response, error)
}

func (a resourceTestAdapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	return a.forward(ctx, request)
}

type countedBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *countedBody) Close() error { b.closed.Add(1); return nil }

func TestAttemptOwnsBodyReturnedAlongsideError(t *testing.T) {
	base := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	s, _ := newGuardLoopService(t, base, "body-error")
	body := &countedBody{Reader: strings.NewReader("diagnostic")}
	var physical context.Context
	s.providers = provider.NewRegistry(resourceTestAdapter{base, func(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
		physical = ctx
		return &provider.Response{StatusCode: 502, Header: make(http.Header), Body: body}, errors.New("transport failure with response")
	}})
	result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("body-error", true))
	if result != nil || err == nil || body.closed.Load() != 1 || physical.Err() == nil {
		t.Fatalf("result=%v err=%v closes=%d physical=%v", result, err, body.closed.Load(), physical.Err())
	}
}

func TestUndeliveredResultExpiresAndReleasesResources(t *testing.T) {
	base := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	s, accounts := newGuardLoopService(t, base, "unclaimed-result")
	// The admission budget spans the whole governed pipeline: selection, the
	// physical send and the quality peek must all finish before delivery, and
	// the same absolute deadline later expires the unclaimed result. An 80ms
	// budget lost the pre-delivery race on a shared CI runner (observed while
	// go test kept the long-running inference package busy): the peek never
	// read the first SSE chunk before the deadline canceled it, so
	// CreateChatCompletion surfaced quality_admission_timeout. Stay in the
	// seconds-scale regime the other hold tests use and derive every
	// post-delivery wait from the budget: expiry fires at start+budget, which
	// is at most one budget after CreateChatCompletion returns.
	const admissionBudget = 5 * time.Second
	s.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, AdmissionTimeout: admissionBudget})
	completed := make(chan QualityObservation, 1)
	s.SetQualityEventRecorder(eventRecorderFunc(func(_ context.Context, obs QualityObservation, _ time.Duration) error {
		if obs.Outcome != QualityObservedAdmitted {
			completed <- obs
		}
		return nil
	}))
	body := &countedBody{Reader: strings.NewReader("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"plan\"}}]}\n\ndata: [DONE]\n\n")}
	var physical context.Context
	s.providers = provider.NewRegistry(resourceTestAdapter{base, func(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
		physical = ctx
		return &provider.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	}})
	result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("unclaimed", true))
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	select {
	case <-physical.Done():
	case <-time.After(2 * admissionBudget):
		t.Fatal("unused result retained physical request")
	}
	if err := result.CommitDelivery(); err == nil {
		t.Fatal("expired result can publish headers")
	}
	if _, err := result.Body.Read(make([]byte, 10)); err == nil {
		t.Fatal("expired result leaked held bytes")
	}
	deadline := time.Now().Add(2 * admissionBudget)
	for {
		n, _ := s.selector.concurrency.Current(context.Background(), accountConcurrencyKey(accounts[0].ID))
		if n == 0 && body.closed.Load() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unused result: lease=%d closes=%d", n, body.closed.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case obs := <-completed:
		if obs.Outcome != QualityObservedInterrupted || obs.ErrorCode != "quality_admission_timeout" {
			t.Fatalf("unused result lost completion reason: %+v", obs)
		}
	case <-time.After(2 * admissionBudget):
		t.Fatal("unused result did not record completion")
	}
}

func TestAttemptResourceCloseRacesReplacement(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, resources := newAttemptResources(context.Background())
		first := &countedBody{Reader: strings.NewReader("one")}
		inner := resources.own(first)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); resources.close() }()
		go func() { defer wg.Done(); resources.own(inner) }()
		wg.Wait()
		resources.close()
		if first.closed.Load() != 1 || ctx.Err() == nil {
			t.Fatalf("closes=%d ctx=%v", first.closed.Load(), ctx.Err())
		}
	}
}

func TestOutputAcceptanceRequiresSuccessfulDelivery(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, code := range []string{"", "upstream_stream_interrupted", "client_disconnected", "response_too_large"} {
			adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
			s, accounts := newGuardLoopService(t, adapter, "output-commit")
			s.UpdateQualityRetry(QualityRetryRuntime{Enabled: enabled})
			var accepted atomic.Int32
			adapter.responses[accounts[0].ID] = []scriptedBuildResponse{{status: 200,
				body: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"plan\"}}]}\n\ndata: [DONE]\n\n", acceptOutput: func() { accepted.Add(1) }}}
			adapter.checkRequest = func(request provider.ResponseResourceRequest) {
				if !request.DeferOutputCommit {
					t.Error("output effects were not deferred")
				}
			}
			result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("cache-delivery", true))
			if err != nil {
				t.Fatal(err)
			}
			if accepted.Load() != 0 {
				t.Fatal("cache accepted at handoff")
			}
			_, _ = io.Copy(io.Discard, result.Body)
			if accepted.Load() != 0 {
				t.Fatal("cache accepted before delivery result")
			}
			finishTestResult(t, result, Usage{}, "", code)
			_ = result.Body.Close()
			want := int32(0)
			if code == "" {
				want = 1
			}
			if accepted.Load() != want {
				t.Fatalf("guard=%v code=%s accepted=%d", enabled, code, accepted.Load())
			}
		}
	}
}

// The request entry creates admission unarmed: model and candidate lookups
// run before jurisdiction and exemptions are settled, so an exempt request
// must not be cancelled mid-lookup. Arming afterwards still measures from the
// original request start.
func TestAdmissionArmsAfterExemptionDecision(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	unarmed := newAdmission(parent, started, 0)
	time.Sleep(2 * time.Millisecond)
	if err := unarmed.failure(); err != nil {
		t.Fatalf("unarmed admission failed during pre-work: %v", err)
	}
	unarmed.disable()
	if err := unarmed.failure(); err != nil {
		t.Fatalf("disabled admission failed: %v", err)
	}
	expired := newAdmission(parent, started.Add(-time.Minute), 0)
	expired.setBudget(30 * time.Second)
	if err := expired.failure(); err == nil {
		t.Fatal("armed budget must still measure from request start")
	}
}
