package inference

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	"github.com/gin-gonic/gin"
)

// Controlled real-socket relay measurements exclude provider compute and the
// gateway selector. They measure parsing, conversion, HTTP writes and flushes.
func TestLocalRelayLatencyDistribution(t *testing.T) {
	if os.Getenv("GROK2API_RELAY_LATENCY") != "1" {
		t.Skip("opt-in timing experiment")
	}
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		operation string
		protocol  streamProtocol
	}{{"responses", streamProtocolResponses}, {"chat", streamProtocolChat}, {"messages", streamProtocolAnthropic}} {
		t.Run(tc.operation, func(t *testing.T) {
			generated := make(chan time.Time, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				generated <- time.Now()
				for range 64 {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"token abcdefghijklmnopqrstuvwxyz\"}\n\n")
					w.(http.Flusher).Flush()
				}
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":64}}}\n\n")
			}))
			defer upstream.Close()
			upstreamClient := upstream.Client()
			defer upstreamClient.CloseIdleConnections()
			relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.URL, nil)
				response, err := upstreamClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				var source io.ReadCloser = responseflow.New(response.Body, nil)
				if tc.operation != "responses" {
					source = conversation.ConvertResponseStreamWithOptions(source, tc.operation, conversation.ResponseOptions{})
				}
				defer source.Close()
				ctx, _ := gin.CreateTestContext(w)
				ctx.Request = r
				ctx.Header("Content-Type", "text/event-stream")
				if _, err := copyStream(ctx.Writer, source, tc.protocol, nil); err != nil {
					t.Error(err)
				}
			}))
			defer relay.Close()
			client := relay.Client()
			defer client.CloseIdleConnections()
			var first, total []int64
			for i := 0; i < 210; i++ {
				start := time.Now()
				response, err := client.Get(relay.URL)
				if err != nil {
					t.Fatal(err)
				}
				scanner := bufio.NewScanner(response.Body)
				var arrived time.Time
				for scanner.Scan() {
					if arrived.IsZero() && strings.Contains(scanner.Text(), "token abc") {
						arrived = time.Now()
					}
				}
				err = scanner.Err()
				response.Body.Close()
				origin := <-generated
				if err != nil || arrived.IsZero() {
					t.Fatalf("missing answer: %v", err)
				}
				if i >= 10 {
					first = append(first, arrived.Sub(origin).Microseconds())
					total = append(total, time.Since(start).Microseconds())
				}
			}
			slices.Sort(first)
			slices.Sort(total)
			t.Logf("samples=%d frames=64 relay_first_us p50=%d p95=%d p99=%d total_us p50=%d p95=%d p99=%d", len(first), first[100], first[190], first[198], total[100], total[190], total[198])
		})
	}
}

func BenchmarkResponseRelay64Frames(b *testing.B) {
	const delta = "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"token abcdefghijklmnopqrstuvwxyz\"}\n\n"
	const done = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":64}}}\n\n"
	frames := strings.Repeat(delta, 64) + done
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		operation string
		protocol  streamProtocol
	}{{"responses", streamProtocolResponses}, {"chat", streamProtocolChat}, {"messages", streamProtocolAnthropic}} {
		b.Run(tc.operation, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var source io.ReadCloser = responseflow.New(io.NopCloser(strings.NewReader(frames)), nil)
				if tc.operation != "responses" {
					source = conversation.ConvertResponseStreamWithOptions(source, tc.operation, conversation.ResponseOptions{})
				}
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				_, err := copyStream(ctx.Writer, source, tc.protocol, nil)
				source.Close()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
