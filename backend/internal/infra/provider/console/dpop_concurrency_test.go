package console

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func TestDPoPOwnerCancellationLetsLiveWaitersContinue(t *testing.T) {
	entered, released := make(chan struct{}), make(chan struct{})
	var tokenCalls, responseCalls atomic.Int32
	var once sync.Once
	defer once.Do(func() { close(released) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/dpop/token" {
			if tokenCalls.Add(1) == 1 {
				close(entered)
				select {
				case <-r.Context().Done():
					return
				case <-released:
				}
			}
			serveTestDPoPToken(t, w, r)
			return
		}
		responseCalls.Add(1)
		verifyTestDPoPProof(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_dpop","object":"response","status":"completed","output":[]}`)
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	forward := func(ctx context.Context) error {
		r, err := adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3", Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.3","input":"hello"}`)})
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, r.Body)
		if e := r.Body.Close(); err == nil {
			err = e
		}
		return err
	}
	ownerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := make(chan error, 1)
	go func() { owner <- forward(ownerCtx) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not reach mint")
	}
	const workers = 8
	waiters := make(chan error, workers)
	var started sync.WaitGroup
	started.Add(workers)
	for range workers {
		go func() { started.Done(); waiters <- forward(context.Background()) }()
	}
	started.Wait()
	time.Sleep(100 * time.Millisecond)
	// Actual HTTP remains blocked while the new callers begin. The assertion is
	// on their outcome and the total mint count, not internal group membership.
	cancel()
	if err := <-owner; !errors.Is(err, context.Canceled) {
		t.Errorf("owner error=%v", err)
	}
	for range workers {
		select {
		case err := <-waiters:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("live waiter stuck")
		}
	}
	if err := forward(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tokenCalls.Load() != 2 || responseCalls.Load() != workers+1 {
		t.Fatalf("mint=%d response=%d", tokenCalls.Load(), responseCalls.Load())
	}
}

func TestDPoPInterruptedOwnerAlwaysReleasesRefresh(t *testing.T) {
	for _, mode := range []string{"panic", "goexit"} {
		t.Run(mode, func(t *testing.T) {
			m := newDPoPSessionManager()
			m.store("key", dpopSession{accessToken: "synthetic", expiresAt: time.Now().Add(time.Hour)})
			pending := &dpopSessionRefresh{done: make(chan struct{})}
			m.refreshes = map[string]*dpopSessionRefresh{"key": pending}
			m.now = func() time.Time {
				if mode == "panic" {
					panic("synthetic interruption")
				}
				runtime.Goexit()
				return time.Time{}
			}
			ownerDone := make(chan struct{})
			var recovered any
			returned := false
			go func() {
				defer close(ownerDone)
				defer func() { recovered = recover() }()
				_, _ = m.runRefresh(context.Background(), "key", pending, nil, "", nil)
				returned = true
			}()
			select {
			case <-pending.done:
			case <-time.After(time.Second):
				t.Fatal("refresh notification stuck")
			}
			<-ownerDone
			if pending.err == nil || pending.ownerCanceled || returned || (mode == "panic" && recovered == nil) || (mode == "goexit" && recovered != nil) {
				t.Fatalf("owner behavior lost: err=%v returned=%v recovered=%v", pending.err, returned, recovered)
			}
			m.mu.Lock()
			remaining := len(m.refreshes)
			m.mu.Unlock()
			if remaining != 0 {
				t.Fatal("interrupted refresh retained")
			}
			m.now = time.Now
			if _, ok := m.cached("key"); !ok {
				t.Fatal("interruption corrupted cached state")
			}
		})
	}
}
