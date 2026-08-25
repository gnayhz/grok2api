package middleware

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/gin-gonic/gin"
)

// TestWriteRateLimitRetryAfter：RPM typed error 必须渲染整秒向上取整的
// Retry-After；非 typed 错误与零值不渲染（并发/用量/鉴权错误的重试时机
// 不可由窗口推导，不应误导客户端）。
func TestWriteRateLimitRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() *gin.Context {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest("GET", "/v1/ping", nil)
		return context
	}

	c := newContext()
	writeRateLimitRetryAfter(c, &clientkey.RateLimitedError{RetryAfter: 42 * time.Second})
	if got := c.Writer.Header().Get("Retry-After"); got != "42" {
		t.Fatalf("Retry-After = %q, want 42", got)
	}

	// 毫秒向上取整到秒。
	c = newContext()
	writeRateLimitRetryAfter(c, &clientkey.RateLimitedError{RetryAfter: 1500 * time.Millisecond})
	if got := c.Writer.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("fractional seconds must round up, got %q", got)
	}

	// 普通 429（并发/用量）与零值不渲染。
	c = newContext()
	writeRateLimitRetryAfter(c, clientkey.ErrConcurrencyLimit)
	if got := c.Writer.Header().Get("Retry-After"); got != "" {
		t.Fatalf("concurrency limit must not set Retry-After, got %q", got)
	}
	c = newContext()
	writeRateLimitRetryAfter(c, &clientkey.RateLimitedError{})
	if got := c.Writer.Header().Get("Retry-After"); got != "" {
		t.Fatalf("zero retry-after must not set header, got %q", got)
	}
}
