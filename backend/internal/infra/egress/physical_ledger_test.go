package egress

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

func ledgerContext() context.Context {
	ctx := attemptmeta.WithRequest(context.Background(), "physical-request", 7, "rules", nil)
	ctx = attemptmeta.WithAccount(ctx, 42, "grok_build", "grok-4.6")
	return physical.WithPhysicalCallTrace(ctx, testsupport.NewPhysicalJournalFactory().NewPhysicalJournal(), "grok_build", "responses")
}

func TestPhysicalLedgerRetainsIndependentFailuresUsageAndIdentity(t *testing.T) {
	ctx := ledgerContext()
	client := &scriptedRequestClient{do: func(call int, r *http.Request) (*http.Response, error) {
		if call == 1 {
			return nil, errors.New("proxyconnect tcp: connection refused")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("output"))}, nil
	}}
	lease := &Lease{client: client, NodeID: 9, proxyPool: true}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/responses", http.NoBody)
	response, err := lease.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	id := attemptmeta.FromResponse(response)
	ObservePhysicalPayload(ctx, id.ID, []byte(`{"response":{"usage":{"input_tokens":13,"output_tokens":7,"total_tokens":20}}}`))
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	facts := physical.PhysicalFacts(ctx)
	if len(facts) != 2 {
		t.Fatalf("physical facts=%+v", facts)
	}
	if facts[0].Attempt.ID == facts[1].Attempt.ID || facts[0].HeaderOutcome != "transport_error" || facts[1].Stage != "connection_retry" {
		t.Fatalf("merged retries: %+v", facts)
	}
	if facts[0].Usage.Found || !facts[1].Usage.Found || facts[1].Usage.Total != 20 || facts[1].BodyBytes != 6 || facts[1].BodyOutcome != "eof" {
		t.Fatalf("usage/lifecycle=%+v", facts)
	}
	if facts[1].Attempt.Path.NodeID != 9 || !facts[1].Attempt.Path.Rotating {
		t.Fatal("lost actual path")
	}
	physical.ConfirmPhysicalFacts(ctx, facts)
	if len(physical.PhysicalFacts(ctx)) != 0 {
		t.Fatal("confirmed facts emitted twice")
	}
}

func TestPhysicalLedgerLimitsBeforeTransportSubmission(t *testing.T) {
	ctx := ledgerContext()
	client := &scriptedRequestClient{do: func(int, *http.Request) (*http.Response, error) { return nil, errors.New("dial failed") }}
	lease := &Lease{client: client}
	for i := 0; i <= physical.MaxPhysicalCalls; i++ {
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/responses", http.NoBody)
		_, err := lease.Do(request)
		if (i == physical.MaxPhysicalCalls) != errors.Is(err, ErrPhysicalCallLimit) {
			t.Fatalf("call %d err=%v", i, err)
		}
	}
	if client.calls != physical.MaxPhysicalCalls || len(physical.PhysicalFacts(ctx)) != physical.MaxPhysicalCalls {
		t.Fatalf("calls=%d facts=%d", client.calls, len(physical.PhysicalFacts(ctx)))
	}
}

func TestPhysicalUsageIgnoresNestedUserFieldsAndRecordsReportedZero(t *testing.T) {
	ctx := attemptmeta.Begin(ledgerContext(), attemptmeta.Path{})
	if err := beginPhysicalCall(ctx); err != nil {
		t.Fatal(err)
	}
	id := attemptmeta.FromContext(ctx).ID
	recordPhysicalCall(ctx, nil, errors.New("failed"))
	ObservePhysicalPayload(ctx, id, []byte(`{"output":[{"usage":{"output_tokens":999}}]}`))
	if physical.PhysicalFacts(ctx)[0].Usage.Found {
		t.Fatal("nested tool/user usage treated as billing")
	}
	ObservePhysicalPayload(ctx, id, []byte(`{"usage":null}`))
	if physical.PhysicalFacts(ctx)[0].Usage.Found {
		t.Fatal("absent usage treated as reported zero")
	}
	ObservePhysicalPayload(ctx, id, []byte(`{"usage":{"input_tokens":0,"output_tokens":0}}`))
	if !physical.PhysicalFacts(ctx)[0].Usage.Found {
		t.Fatal("reported zero conflated with unknown")
	}
}

func TestPhysicalLedgerDefersBufferedDeliveryAndPreservesCanonicalUsage(t *testing.T) {
	ctx := attemptmeta.Begin(ledgerContext(), attemptmeta.Path{})
	if err := beginPhysicalCall(ctx); err != nil {
		t.Fatal(err)
	}
	id := attemptmeta.FromContext(ctx).ID
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("buffered"))}
	recordPhysicalCall(ctx, response, nil)
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if len(physical.PhysicalFacts(ctx, id)) != 0 {
		t.Fatal("buffered delivery persisted before its usage became available")
	}
	physical.ObservePhysicalUsage(ctx, id, jsonpeek.TokenUsage{Found: true, Input: 20, Output: 5})
	if facts := physical.PhysicalFacts(ctx); len(facts) != 1 || facts[0].Usage.Input != 20 {
		t.Fatalf("lost late usage: %+v", facts)
	}
	ObservePhysicalPayload(ctx, id, []byte(`{"usage":{"input_tokens":20,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":3}}}`))
	physical.ObservePhysicalUsage(ctx, id, jsonpeek.TokenUsage{Found: true, Input: 20, Output: 5})
	facts := physical.PhysicalFacts(ctx)
	if facts[0].Usage.Reasoning != 3 {
		t.Fatal("converted usage overwrote the physical counters")
	}
	physical.ConfirmPhysicalFacts(ctx, facts)
	ObservePhysicalPayload(ctx, id, []byte(`{"usage":{"input_tokens":999}}`))
	if len(physical.PhysicalFacts(ctx)) != 0 {
		t.Fatal("confirmed facts changed after persistence")
	}
	observed := physical.PhysicalObservations(ctx)
	if len(observed) != 1 || observed[0].Usage.Input != 20 || observed[0].Usage.Reasoning != 3 {
		t.Fatalf("receipt acknowledgment erased generation usage: %+v", observed)
	}
}
