package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// captureAccessWriter 捕获异步刷写的行(替代 stdout)。
type captureAccessWriter struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (w *captureAccessWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.Write(p)
}

func (w *captureAccessWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}

// newTestAsyncAccessLogger 用可捕获 writer 构造一个独立的异步 logger
// (不走进程级单例,避免污染其他测试的 stdout)。
func newTestAsyncAccessLogger(t *testing.T) (*slog.Logger, *captureAccessWriter) {
	t.Helper()
	capture := &captureAccessWriter{}
	writer := newAsyncAccessLogWriterWithSink(capture)
	t.Cleanup(func() { _ = writer.Close() })
	return slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})), capture
}

// TestAsyncAccessLogDeliversLines 锁定异步日志契约:中间件产出的访问
// 日志行最终(批量刷写周期内)按序到达 sink,格式与同步 JSON 一致。
func TestAsyncAccessLogDeliversLines(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger, capture := newTestAsyncAccessLogger(t)
	router := gin.New()
	router.Use(AccessLog(logger))
	router.GET("/v1/echo", func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < 3; i++ {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/echo", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(capture.String(), "\"msg\":\"http_request\"") == 3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("async access lines missing: %q", capture.String())
}
