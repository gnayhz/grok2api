package gateway

import (
	"context"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

// Exercise the ownership chain with real sockets: raw event assembly, admission
// replay, a blocked client encoder, partial delivery, cancellation and EOF. Each
// wave must release every reservation before another wave can acquire capacity.
func TestCanonicalResponseResourcesUnderConcurrentDeliveryAndAbort(t *testing.T) {
	concurrency := stressSetting(t, "GROK2API_STRESS_CONCURRENCY", 12, 64)
	waves := stressSetting(t, "GROK2API_STRESS_WAVES", 10, 1000)
	pool := responsebuffer.NewPool(128 << 20)
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	baseFDs := stressFDCount()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	answer := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("a", 256<<10) + "\"}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"plan\"}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, answer)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"output_tokens\":100}}}\n\n")
	}))
	defer server.Close()
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	for wave := 0; wave < waves; wave++ {
		var workers sync.WaitGroup
		for slot := 0; slot < concurrency; slot++ {
			workers.Add(1)
			go func(mode int) {
				defer workers.Done()
				ctx, owner := selector.NewAttemptResources(context.Background())
				defer owner.Close()
				request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
				response, err := client.Do(request)
				if err != nil {
					t.Error(err)
					return
				}
				stream := responsecheck.Stream(responseflow.New(owner.Own(response.Body), pool.Request(32<<20)))
				body, verdict, _, _, err := peekQualityStreamReport(ctx, stream, qualityProtocolResponses, QualityRetryRuntime{})
				if body != nil {
					defer body.Close()
				}
				if err != nil || verdict != QualityDeliver {
					t.Errorf("admission: %s %v", verdict, err)
					return
				}
				converted := conversation.ConvertResponseStreamWithOptions(body, conversation.OperationChat, conversation.ResponseOptions{})
				defer converted.Close()
				switch mode {
				case 0:
					// Let the producer reach its pipe write with no client reader.
					time.Sleep(time.Millisecond)
				case 1:
					var first [1]byte
					_, _ = converted.Read(first[:])
					owner.Close()
				default:
					if _, err := io.Copy(io.Discard, converted); err != nil {
						t.Errorf("complete delivery: %v", err)
					}
				}
			}(slot % 3)
		}
		workers.Wait()
		if state := pool.Snapshot(); state.Used != 0 || state.Peak > state.Limit || state.Rejected != 0 {
			t.Fatalf("wave %d retained or exceeded response capacity: %+v", wave, state)
		}
	}
	transport.CloseIdleConnections()
	server.Close()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseGoroutines+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	afterGoroutines, afterFDs := runtime.NumGoroutine(), stressFDCount()
	if afterGoroutines > baseGoroutines+4 {
		t.Errorf("goroutines did not settle: before=%d after=%d", baseGoroutines, afterGoroutines)
	}
	if baseFDs >= 0 && afterFDs > baseFDs+2 {
		t.Errorf("sockets did not settle: before=%d after=%d", baseFDs, afterFDs)
	}
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > before.HeapAlloc+16<<20 {
		t.Errorf("heap did not settle: before=%d after=%d", before.HeapAlloc, after.HeapAlloc)
	}
	t.Logf("requests=%d concurrent=%d budget_peak=%d retained=%d heap_before=%d heap_after=%d goroutines=%d->%d fds=%d->%d", concurrency*waves, concurrency, pool.Snapshot().Peak, pool.Snapshot().Used, before.HeapAlloc, after.HeapAlloc, baseGoroutines, afterGoroutines, baseFDs, afterFDs)
}

func stressSetting(t *testing.T, name string, fallback, limit int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > limit {
		t.Fatalf("%s must be between 1 and %d", name, limit)
	}
	return value
}

func stressFDCount() int {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", os.Getpid()))
	if err != nil {
		return -1
	}
	return len(entries)
}

func BenchmarkCanonicalWithholdLargeEvent(b *testing.B) {
	for _, size := range []int{16 << 10, 256 << 10, 2 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			raw := "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"" + strings.Repeat("A", size) + "\"}}\n\n"
			pool := responsebuffer.NewPool(128 << 20)
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for range b.N {
				stream := responseflow.New(io.NopCloser(strings.NewReader(raw)), pool.Request(96<<20))
				body, verdict, _, _, err := peekQualityStreamReport(context.Background(), stream, qualityProtocolResponses, QualityRetryRuntime{})
				if body != nil {
					_ = body.Close()
				}
				if err != nil || verdict != QualityWithhold {
					b.Fatalf("large canonical event: verdict=%s err=%v", verdict, err)
				}
			}
			b.StopTimer()
			if pool.Snapshot().Used != 0 {
				b.Fatal("withhold retained response capacity")
			}
			b.ReportMetric(float64(pool.Snapshot().Peak), "budget_peak_B")
		})
	}
}
