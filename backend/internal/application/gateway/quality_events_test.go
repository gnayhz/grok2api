package gateway

import (
	"context"
	"errors"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type eventRecorderFunc func(context.Context, QualityObservation, time.Duration) error

func (f eventRecorderFunc) RecordQualityEvent(ctx context.Context, obs QualityObservation, ttl time.Duration) error {
	return f(ctx, obs, ttl)
}

func TestEveryRejectedAttemptReleasesCapacityBeforeDurableReceipt(t *testing.T) {
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	s, accounts, limiter := newGuardLoopServiceWithLimiter(t, adapter, "event-a", "event-b")
	for _, account := range accounts {
		adapter.responses[account.ID] = []scriptedBuildResponse{{status: http.StatusOK, body: "data: {\"choices\":[{\"delta\":{\"content\":\"bare\"}}]}\n\ndata: [DONE]\n\n"}}
	}
	ids := map[string]bool{}
	s.SetQualityEventRecorder(eventRecorderFunc(func(ctx context.Context, obs QualityObservation, ttl time.Duration) error {
		if obs.Outcome != QualityObservedDegraded {
			t.Fatalf("unexpected outcome %s", obs.Outcome)
		}
		if count, err := limiter.Current(ctx, repository.AccountConcurrencyKey(obs.AccountID)); err != nil || count != 0 {
			t.Fatalf("lease held while recording: count=%d err=%v", count, err)
		}
		if obs.Attempt.ID == "" || ids[obs.Attempt.ID] {
			t.Fatalf("attempt identity missing or duplicate: %+v", obs.Attempt)
		}
		ids[obs.Attempt.ID] = true
		return nil
	}))
	if result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("event-request", true)); result != nil || !errors.Is(err, errQualityDegraded) {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if len(ids) != 2 {
		t.Fatalf("only %d physical attempts recorded", len(ids))
	}
}

func TestDurableReceiptFailureStopsRetriesAndLeavesLocalProtection(t *testing.T) {
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	s, accounts := newGuardLoopService(t, adapter, "event-failure-a", "event-failure-b")
	adapter.responses[accounts[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: "data: {\"choices\":[{\"delta\":{\"content\":\"bare\"}}]}\n\ndata: [DONE]\n\n"}}
	s.SetQualityEventRecorder(eventRecorderFunc(func(context.Context, QualityObservation, time.Duration) error {
		return errors.New("injected storage outage")
	}))
	result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("receipt-failure", true))
	var failure *UpstreamFailure
	if result != nil || !errors.As(err, &failure) || failure.Code != "quality_event_unavailable" {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if len(adapter.Attempts()) != 1 {
		t.Fatal("retried without a durable receipt")
	}
	if s.selector.LocalQualityAllowed(accounts[0].ID, time.Now()) {
		t.Fatal("missing local outage protection")
	}
}

func TestBufferedPhysicalResponsePersistsDeliveryUsageAfterInternalFailure(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		base := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
		s, _ := newGuardLoopService(t, base, "physical-buffered")
		s.SetGuardSnapshotSource(StaticGuardSnapshotSource(QualityRetryRuntime{Enabled: guarded, GuardedModels: []string{"grok-4.6"}}))
		recorder := &physicalEventRecorder{}
		s.SetQualityEventRecorder(recorder)
		s.providers = providerimpl.NewRegistry(resourceTestAdapter{base, func(ctx context.Context, _ provider.ResponseResourceRequest) (*provider.Response, error) {
			// An internal failed exchange must persist before handoff even when
			// the final exchange has already been read and buffered by an adapter.
			failed := attemptmeta.Begin(ctx, attemptmeta.Path{})
			if err := infraegress.BeginDirectPhysicalCall(failed); err != nil {
				return nil, err
			}
			infraegress.RecordDirectPhysicalCall(failed, nil, errors.New("connection failed"))
			current := attemptmeta.Begin(ctx, attemptmeta.Path{})
			if err := infraegress.BeginDirectPhysicalCall(current); err != nil {
				return nil, err
			}
			raw := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"output":[{"type":"reasoning","summary":[{"text":"plan"}]}],"usage":{"input_tokens":20,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":3}}}`))}
			infraegress.RecordDirectPhysicalCall(current, raw, nil)
			data, err := io.ReadAll(raw.Body)
			_ = raw.Body.Close()
			return &provider.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Attempt: attemptmeta.FromContext(current), Body: io.NopCloser(strings.NewReader(string(data)))}, err
		}})
		result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("physical-buffered", false))
		if err != nil {
			t.Fatal(err)
		}
		if len(recorder.facts) != 1 || recorder.facts[0].Usage.Found || recorder.facts[0].HeaderOutcome != "transport_error" {
			t.Fatalf("premature physical delivery receipt: %+v", recorder.facts)
		}
		_, _ = io.Copy(io.Discard, result.Body)
		finishTestResult(t, result, Usage{Reported: true, InputTokens: 20, OutputTokens: 5}, "", "")
		_ = result.Body.Close()
		if len(recorder.facts) != 2 || !recorder.facts[1].Usage.Found || recorder.facts[1].Usage.Input != 20 || recorder.facts[1].BodyOutcome != "eof" {
			t.Fatalf("guard=%v missing final usage/lifecycle: %+v", guarded, recorder.facts)
		}
		if guarded && recorder.facts[1].Usage.Reasoning != 3 {
			t.Fatal("converted delivery lost canonical reasoning usage")
		}
	}
}
