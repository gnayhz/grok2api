package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponsePipelineCloseInterruptsBlockedSource(t *testing.T) {
	for _, layer := range []string{"compatibility", "search", "both"} {
		t.Run(layer, func(t *testing.T) {
			source := &blockedPipelineSource{waiting: make(chan struct{}), closed: make(chan struct{})}
			var stream io.ReadCloser = source
			if layer == "search" || layer == "both" {
				stream = newBuildXSearchResponseFilter(buildPromptCacheRoute{}).stream(stream)
			}
			if layer == "compatibility" || layer == "both" {
				stream = (*responsesToolCompatibility)(nil).normalizeResponseStream(stream)
			}
			defer source.Close()
			defer stream.Close()
			finished := make(chan struct{})
			go func() {
				_, _ = io.Copy(io.Discard, stream)
				close(finished)
			}()
			select {
			case <-source.waiting:
			case <-time.After(time.Second):
				t.Fatal("pipeline did not start its upstream read")
			}
			_ = stream.Close()
			select {
			case <-source.closed:
			case <-time.After(100 * time.Millisecond):
				t.Fatal("closing the response left the upstream Read blocked")
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("pipeline reader did not return after Close")
			}
			_ = stream.Close()
			if got := source.closes.Load(); got != 1 {
				t.Fatalf("upstream closed %d times", got)
			}
		})
	}
}

type blockedPipelineSource struct {
	waiting, closed chan struct{}
	readOnce        sync.Once
	closeOnce       sync.Once
	closes          atomic.Int32
}

func (s *blockedPipelineSource) Read([]byte) (int, error) {
	s.readOnce.Do(func() { close(s.waiting) })
	<-s.closed
	return 0, io.EOF
}

func (s *blockedPipelineSource) Close() error {
	s.closes.Add(1)
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func TestNestedResponsePipelineCloseReleasesHTTPConnection(t *testing.T) {
	disconnected, stop := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-stop:
		}
	}))
	defer server.Close()
	defer close(stop)
	client := server.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	filtered := newBuildXSearchResponseFilter(buildPromptCacheRoute{}).stream(response.Body)
	stream := (*responsesToolCompatibility)(nil).normalizeResponseStream(filtered)
	_ = stream.Close()
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("nested response adapters retained the HTTP connection after Close")
	}
}
