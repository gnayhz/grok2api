package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestConcurrencyGateStormExactAccounting：并发风暴下闸门记账必须精确——
// limit=8、100 并发：恰好 8 个通过（真实持槽等待）、92 个 503 拒绝、
// 峰值不超限、全部完成后计数归零（无槽位泄漏）。
func TestConcurrencyGateStormExactAccounting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gate := NewConcurrencyGate(8)
	router := gin.New()
	router.Use(gate.Middleware())
	var inFlight atomic.Int64
	var peak atomic.Int64

	var holders sync.WaitGroup
	holders.Add(8)
	releaseAll := make(chan struct{})
	router.GET("/probe", func(c *gin.Context) {
		current := inFlight.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		if current <= 8 {
			holders.Done()
			<-releaseAll
		}
		inFlight.Add(-1)
		c.Status(http.StatusOK)
	})
	go func() {
		holders.Wait()
		time.Sleep(50 * time.Millisecond)
		close(releaseAll)
	}()

	var accepted, rejected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe", nil))
			if recorder.Code == http.StatusOK {
				accepted.Add(1)
			} else if recorder.Code == http.StatusServiceUnavailable {
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := accepted.Load(); got != 8 {
		t.Fatalf("accepted = %d, want exactly 8", got)
	}
	if got := rejected.Load(); got != 92 {
		t.Fatalf("rejected = %d, want exactly 92", got)
	}
	if got := peak.Load(); got > 8 {
		t.Fatalf("in-flight peak = %d exceeds limit 8", got)
	}
	gate.mu.Lock()
	active := gate.active
	gate.mu.Unlock()
	if active != 0 {
		t.Fatalf("slot leak: active = %d after all requests completed", active)
	}
}
