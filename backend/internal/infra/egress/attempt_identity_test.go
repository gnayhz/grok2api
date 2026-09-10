package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

type changingPathVersion struct{ epoch atomic.Uint64 }

func (p *changingPathVersion) PathVersion(uint64) (uint64, bool) { return p.epoch.Load(), true }

func TestPhysicalResponseRetainsSubmissionEpochDuringRotation(t *testing.T) {
	received, finish := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(received)
		<-finish
		_, _ = io.WriteString(w, "response")
	}))
	defer server.Close()
	version := &changingPathVersion{}
	version.epoch.Store(11)
	ctx := attemptmeta.WithRequest(context.Background(), "synthetic-request", 4, "test-rule", version)
	ctx = attemptmeta.WithAccount(ctx, 17, "grok_build", "synthetic-model")
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
	lease := &Lease{NodeID: 23, client: server.Client()}
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() { response, err := lease.Do(request); done <- result{response, err} }()
	<-received
	version.epoch.Store(12)
	// A parallel auxiliary call has the same request scope but a different ID.
	aux := attemptmeta.FromContext(attemptmeta.Begin(ctx, attemptmeta.Path{NodeID: 99}))
	close(finish)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.response.Body.Close()
	id := attemptmeta.FromResponse(got.response)
	if id.Path.NodeID != 23 || id.Path.Epoch != 11 || id.Path.Status != attemptmeta.PathRegistered || id.ID == aux.ID || id.AccountID != 17 || id.Revision != 4 {
		t.Fatalf("response identity changed in flight: %+v", id)
	}
}

func TestRotatingProxyRegistrationIsNotPathVerification(t *testing.T) {
	version := &changingPathVersion{}
	version.epoch.Store(8)
	ctx := attemptmeta.WithRequest(context.Background(), "synthetic", 2, "test", version)
	id := attemptmeta.FromContext(attemptmeta.Begin(ctx, attemptmeta.Path{NodeID: 3, Rotating: true}))
	if id.Path.Status != attemptmeta.PathUnknown || id.Path.Epoch != 8 {
		t.Fatalf("rotating endpoint claimed verified path: %+v", id.Path)
	}
}
