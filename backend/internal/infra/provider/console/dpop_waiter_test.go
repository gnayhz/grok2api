package console

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestDPoPWaiterDeadlineDoesNotWaitForOwner(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var tokenCalls, responseCalls atomic.Int32
	var once sync.Once
	defer once.Do(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/dpop/token" {
			if tokenCalls.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			serveTestDPoPToken(t, w, r)
			return
		}
		responseCalls.Add(1)
		verifyTestDPoPProof(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_waiter","object":"response","status":"completed","output":[]}`)
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	forward := func(ctx context.Context) error {
		response, err := adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3", Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.3","input":"hello"}`)})
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, response.Body)
		if closeErr := response.Body.Close(); err == nil {
			err = closeErr
		}
		return err
	}
	owner := make(chan error, 1)
	go func() { owner <- forward(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("owner never reached token endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	waiter := make(chan error, 1)
	start := time.Now()
	go func() { waiter <- forward(ctx) }()
	timely := false
	var waiterErr error
	select {
	case waiterErr = <-waiter:
		timely = true
	case <-time.After(500 * time.Millisecond):
	}
	elapsed := time.Since(start)
	once.Do(func() { close(release) })
	if !timely {
		waiterErr = <-waiter
	}
	if err := <-owner; err != nil {
		t.Errorf("owner failed: %v", err)
	}
	if !timely || !errors.Is(waiterErr, context.DeadlineExceeded) || tokenCalls.Load() != 1 || responseCalls.Load() != 1 {
		t.Fatalf("waiter respected own deadline=%v elapsed=%v err=%v token=%d response=%d", timely, elapsed, waiterErr, tokenCalls.Load(), responseCalls.Load())
	}
}
