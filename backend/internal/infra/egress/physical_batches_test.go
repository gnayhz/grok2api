package egress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

type physicalRequestBody struct {
	io.Reader
	closed bool
}

func (b *physicalRequestBody) Close() error { b.closed = true; return nil }

func TestPhysicalBatchGateStopsBeforeSubmissionAndClosesInput(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer upstream.Close()
	ctx := WithPhysicalCallBatches(ledgerContext(), time.Now().Add(time.Minute), func(context.Context) error { return errors.New("proxyconnect receipt database unavailable") })
	lease := &Lease{client: upstream.Client(), proxyPool: true}
	body := &physicalRequestBody{Reader: strings.NewReader("request")}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, body)
	_, err := lease.Do(request)
	if !neterror.RejectedBeforeSubmission(err) || calls.Load() != 0 || !body.closed || PhysicalCallCount(ctx) != 0 {
		t.Fatalf("err=%v calls=%d bodyClosed=%v entries=%d", err, calls.Load(), body.closed, PhysicalCallCount(ctx))
	}
	if _, ok := feedbackKind(domainegress.ScopeBuild, 0, err); ok {
		t.Fatal("local receipt failure entered network health")
	}
}

func TestPhysicalBatchesNeverPruneUnacknowledgedBodies(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = w.Write([]byte("body")) }))
	defer upstream.Close()
	ctx := WithPhysicalCallBatches(ledgerContext(), time.Now().Add(time.Minute), func(context.Context) error { return nil })
	lease := &Lease{client: upstream.Client()}
	var last io.ReadCloser
	for i := 0; i < MaxPhysicalCalls; i++ {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
		response, err := lease.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		last = response.Body
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if _, err := lease.Do(request); !errors.Is(err, ErrPhysicalCallLimit) {
		t.Fatalf("unacknowledged ceiling bypassed: %v", err)
	}
	if calls.Load() != MaxPhysicalCalls || len(PhysicalFacts(ctx)) != MaxPhysicalCalls {
		t.Fatal("lost unacknowledged physical fact")
	}
	ConfirmPhysicalFacts(ctx, PhysicalFacts(ctx))
	if PhysicalCallCount(ctx) != MaxPhysicalCalls || len(PhysicalObservations(ctx)) != 0 {
		t.Fatal("acknowledgement lost cumulative count or retained payload entries")
	}
	_, _ = last.Read(make([]byte, 1)) // a late read of a closed, pruned body must be safe
	response, err := lease.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	facts := PhysicalFacts(ctx)
	if calls.Load() != MaxPhysicalCalls+1 || len(facts) != 1 || facts[0].BodyBytes != 4 || PhysicalCallCount(ctx) != MaxPhysicalCalls+1 {
		t.Fatalf("new batch accounting differs: calls=%d facts=%+v", calls.Load(), facts)
	}
}

func TestPhysicalBatchOwnerCannotBeReplaced(t *testing.T) {
	rejected := errors.New("original owner refused")
	ctx := WithPhysicalCallBatches(ledgerContext(), time.Now().Add(time.Minute), func(context.Context) error { return rejected })
	ctx = WithPhysicalCallBatches(ctx, time.Now().Add(time.Hour), func(context.Context) error { return nil })
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unused.invalid", nil)
	client := &scriptedRequestClient{do: func(int, *http.Request) (*http.Response, error) { t.Fatal("replaced owner submitted"); return nil, nil }}
	_, err := (&Lease{client: client}).Do(request)
	if !errors.Is(err, rejected) || client.calls != 0 {
		t.Fatalf("original owner replaced: %v", err)
	}
}
