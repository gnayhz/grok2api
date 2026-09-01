package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/gin-gonic/gin"
)

// TestAccessLogRecordsPanickingRequests 锁定 panic 补记契约:handler panic
// 时访问日志仍按 500 落一行(此前 panic 展开跳过 c.Next() 之后的 emit,
// 故障期观测盲区),再抛回外层 Recovery 统一恢复。
func TestAccessLogRecordsPanickingRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var mu sync.Mutex
	var output strings.Builder
	sink := &lockedBuilder{mu: &mu, b: &output}
	router := gin.New()
	router.Use(gin.Recovery(), AccessLog(slog.New(slog.NewTextHandler(sink, nil))))
	router.GET("/panic-probe", func(c *gin.Context) { panic("kaboom") })

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic-probe", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("recovery status = %d", recorder.Code)
	}
	mu.Lock()
	logged := output.String()
	mu.Unlock()
	if !strings.Contains(logged, "path=/panic-probe") || !strings.Contains(logged, "status=500") {
		t.Fatalf("panic request missing from access log: %s", logged)
	}
	// panic 补记会向全局 perfmetrics 计一笔 500——排空注册表,不污染
	// 同包后续测试(Totals 类测试断言精确计数)。
	perfmetrics.Default.CollectAndReset()
}

type lockedBuilder struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w *lockedBuilder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
