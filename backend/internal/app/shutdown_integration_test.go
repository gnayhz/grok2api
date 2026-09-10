package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gorilla/websocket"
)

func startLifecycleApplication(t *testing.T, a *Application) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

func awaitRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("application did not finish shutdown")
	}
}

func TestCloseDrainsHTTPWithNetworkAvailableAndWaitsForConcurrentClosers(t *testing.T) {
	a := newLifecycleApplication(t)
	a.httpDrainTimeout = time.Second
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "complete") }))
	defer origin.Close()
	resume := make(chan struct{})
	result := make(chan error, 1)
	a.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-resume
		lease, err := a.egress.Acquire(r.Context(), egressdomain.ScopeBuild, "shutdown")
		if err != nil {
			result <- err
			return
		}
		defer lease.Release()
		request, _ := http.NewRequestWithContext(r.Context(), "GET", origin.URL, nil)
		response, err := lease.Do(request)
		if err == nil {
			_, err = io.Copy(w, response.Body)
			_ = response.Body.Close()
		}
		result <- err
	})
	_, runDone := startLifecycleApplication(t, a)
	response := lifecycleGET(t, a)
	defer response.Body.Close()
	closed := make(chan error, 2)
	for range 2 {
		go func() { closed <- a.Close() }()
	}
	// Wait until Run has actually closed admission before allowing the next
	// physical step of the already accepted request.
	deadline := time.Now().Add(time.Second)
	for {
		a.lifecycleMu.Lock()
		requests := a.requests
		a.lifecycleMu.Unlock()
		requests.mu.Lock()
		draining := requests.draining
		requests.mu.Unlock()
		if draining {
			break
		}
		if time.Now().After(deadline) {
			close(resume)
			t.Fatal("Close did not begin draining")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		close(resume)
		t.Fatalf("Close returned before request completion: %v", err)
	default:
	}
	close(resume)
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "complete" {
		t.Fatalf("graceful result=%q error=%v", body, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	awaitRun(t, runDone)
	for range 2 {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("closed application accepted a second Run")
	}
}

func TestRunWaitsForHijackedReadAndLateCompletion(t *testing.T) {
	a := newLifecycleApplication(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	finish := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	a.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		close(started)
		_, _, _ = conn.ReadMessage() // Deliberately needs the hijacked socket closed.
		if !errors.Is(r.Context().Err(), context.Canceled) {
			t.Error("request root was not canceled")
		}
		close(canceled)
		<-finish // Simulate completion work after network cancellation.
	})
	cancel, runDone := startLifecycleApplication(t, a)
	var conn *websocket.Conn
	deadline := time.Now().Add(time.Second)
	for {
		var err error
		conn, _, err = websocket.DefaultDialer.Dial("ws://"+a.server.Addr, nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	defer conn.Close()
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("hijacked connection was not canceled")
	}
	select {
	case err := <-runDone:
		t.Fatalf("Run returned before completion joined: %v", err)
	default:
	}
	if _, _, err := relational.NewAuditRepository(a.database).List(context.Background(), 0, 1); err != nil {
		t.Fatalf("storage stopped during completion: %v", err)
	}
	once.Do(func() { close(finish) })
	awaitRun(t, runDone)
}

func TestHTTPDrainRejectsNewHandlersAndJoinsPanic(t *testing.T) {
	for _, panicHandler := range []bool{false, true} {
		t.Run(fmt.Sprint(panicHandler), func(t *testing.T) {
			entered := make(chan struct{})
			finish := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				close(entered)
				<-finish
				if panicHandler {
					panic("injected handler panic")
				}
			})}
			requests := newHTTPRequests(server)
			defer requests.force()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = recover() }()
				server.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
			}()
			<-entered
			requests.beginDrain()
			refused := httptest.NewRecorder()
			server.Handler.ServeHTTP(refused, httptest.NewRequest("GET", "/", nil))
			if refused.Code != http.StatusServiceUnavailable {
				t.Fatal(refused.Code)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			if err := waitStopped(ctx, requests.done); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			cancel()
			close(finish)
			<-done
			if err := waitStopped(context.Background(), requests.done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnfinishedHTTPPreservesDependenciesUntilCloseRetry(t *testing.T) {
	a := newLifecycleApplication(t)
	a.shutdownJoinBudget = 30 * time.Millisecond
	finish := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	finished := make(chan error, 1)
	a.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-finish // An uncooperative boundary; cancellation alone cannot join it.
		finished <- a.audits.Create(context.Background(), audit.Record{EventID: "evt_late_shutdown", RequestID: "late-shutdown", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "none", StatusCode: 499})
	})
	cancel, runDone := startLifecycleApplication(t, a)
	response := lifecycleGET(t, a)
	defer response.Body.Close()
	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run hid unfinished completion: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run exceeded its join budget")
	}
	if err := a.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close hid unfinished completion: %v", err)
	}
	if _, _, err := relational.NewAuditRepository(a.database).List(context.Background(), 0, 1); err != nil {
		t.Fatalf("Close shut storage under an active handler: %v", err)
	}
	once.Do(func() { close(finish) })
	if err := <-finished; err != nil {
		t.Fatalf("late handler could not hand off audit: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close retry: %v", err)
	}
}

func BenchmarkHTTPHandlerLifecycle(b *testing.B) {
	for _, tracked := range []bool{false, true} {
		b.Run(fmt.Sprint(tracked), func(b *testing.B) {
			server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
			if tracked {
				requests := newHTTPRequests(server)
				defer requests.force()
			}
			request, response := httptest.NewRequest("GET", "/", nil), httptest.NewRecorder()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				server.Handler.ServeHTTP(response, request)
			}
		})
	}
}

func TestHijackedHandlerPanicClosesOwnedSocket(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ready")); err != nil {
			t.Error(err)
		}
		panic("injected panic after Upgrade")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	requests := newHTTPRequests(server.Config)
	defer requests.force()
	server.Start()
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+server.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, body, err := conn.ReadMessage(); err != nil || string(body) != "ready" {
		t.Fatalf("upgrade: %q %v", body, err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("panic left the hijacked socket open")
	} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
		t.Fatal("panic cleanup required a client timeout")
	}
	requests.beginDrain()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitStopped(ctx, requests.done); err != nil {
		t.Fatal(err)
	}
}
