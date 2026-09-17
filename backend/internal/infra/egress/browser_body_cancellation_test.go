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
)

// Exercise the actual fhttp adapter after cancellation has closed its body,
// and while a read is already waiting for the remainder of a document.
func TestBrowserBodyPreservesRequestCancellation(t *testing.T) {
	for _, mode := range []string{"cancel_before_read", "deadline_before_read", "cancel_during_read", "deadline_during_read"} {
		t.Run(mode, func(t *testing.T) {
			serverDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"value":`)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(serverDone)
			}))
			defer server.Close()
			client, err := newBrowserClientWithBudget("", DefaultUserAgent, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			var ctx context.Context
			var cancel context.CancelFunc
			deadline := strings.HasPrefix(mode, "deadline")
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			result := make(chan error, 1)
			if strings.HasSuffix(mode, "during_read") {
				// Consume the prefix so the following read must wait on the live stream.
				prefix := make([]byte, len(`{"value":`))
				if _, err := io.ReadFull(response.Body, prefix); err != nil {
					t.Fatal(err)
				}
				go func() { _, err := io.ReadAll(response.Body); result <- err }()
				if !deadline {
					cancel()
				}
			} else {
				if !deadline {
					cancel()
				} else {
					<-ctx.Done()
				}
				select {
				case <-serverDone:
				case <-time.After(5 * time.Second):
					t.Fatal("server did not observe cancellation")
				}
				_, err := io.ReadAll(response.Body)
				result <- err
			}
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			select {
			case err := <-result:
				if !errors.Is(err, want) {
					t.Fatalf("body lost request cause: %v want %v", err, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("body cancellation blocked")
			}
		})
	}
}

func BenchmarkBrowserBodyCompletion(b *testing.B) {
	payload := strings.Repeat("normal-response ", 1024)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, payload) }))
	defer server.Close()
	client, err := newBrowserClientWithBudget("", DefaultUserAgent, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer client.CloseIdleConnections()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		req, err := http.NewRequest("GET", server.URL, nil)
		if err != nil {
			b.Fatal(err)
		}
		res, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, res.Body)
		closeErr := res.Body.Close()
		if err != nil || closeErr != nil || n != int64(len(payload)) {
			b.Fatalf("normal body: %d %v %v", n, err, closeErr)
		}
	}
	b.StopTimer()
	if calls.Load() != int64(b.N) {
		b.Fatal("request count changed")
	}
	b.ReportMetric(1, "requests/op")
}
