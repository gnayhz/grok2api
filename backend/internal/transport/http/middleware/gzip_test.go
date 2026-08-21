package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestGzipMiddlewareThreeForms 锁定 round 108：JSON 压缩、SSE 透传、
// 无协商直通。SSE 透传是守卫实时性的硬约束（逐块下发不可缓冲）。
func TestGzipMiddlewareThreeForms(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setup := func(path, contentType string) *gin.Engine {
		r := gin.New()
		r.Use(Gzip())
		r.GET(path, func(c *gin.Context) {
			c.Header("Content-Type", contentType)
			_, _ = c.Writer.WriteString("payload-" + path + "-payload-payload-payload")
		})
		return r
	}

	// JSON + gzip 协商 → 压缩
	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	setup("/json", "application/json").ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("json 应压缩, enc=%q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	decoded, _ := io.ReadAll(zr)
	if len(decoded) == 0 || string(decoded[:7]) != "payload" {
		t.Fatalf("解压内容异常: %q", decoded[:min(20, len(decoded))])
	}

	// SSE + gzip 协商 → 透传（不设 Content-Encoding）
	req2 := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	rec2 := httptest.NewRecorder()
	setup("/stream", "text/event-stream").ServeHTTP(rec2, req2)
	if rec2.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("SSE 不得压缩（守卫逐块下发约束）")
	}

	// 无协商 → 直通
	req3 := httptest.NewRequest(http.MethodGet, "/json", nil)
	rec3 := httptest.NewRecorder()
	setup("/json", "application/json").ServeHTTP(rec3, req3)
	if rec3.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("无 Accept-Encoding 不应压缩")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
