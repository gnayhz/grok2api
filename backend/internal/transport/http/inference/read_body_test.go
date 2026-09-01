package inference

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestReadRequestBodyCorrectness 锁定读取契约:已知长度精确读回、超限
// 声明仍完整(不因预分配截断)、未知长度回退 ReadAll 语义。
func TestReadRequestBodyCorrectness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := strings.Repeat("x", 1<<20)
	newRequest := func(contentLength int64) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
		request.ContentLength = contentLength
		return request
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	c.Request = newRequest(int64(len(payload)))
	body, err := readRequestBody(c, int64(32<<20))
	if err != nil || len(body) != len(payload) || !bytes.Equal(body, []byte(payload)) {
		t.Fatalf("known-length body len=%d err=%v", len(body), err)
	}

	c.Request = newRequest(int64(len(payload)))
	oversized, err := readRequestBody(c, 1024)
	if err != nil || len(oversized) != len(payload) {
		t.Fatalf("oversized-declared len=%d err=%v", len(oversized), err)
	}

	c.Request = newRequest(-1)
	unknown, err := readRequestBody(c, int64(32<<20))
	if err != nil || len(unknown) != len(payload) {
		t.Fatalf("chunked len=%d err=%v", len(unknown), err)
	}
}

// BenchmarkReadRequestBody 量化预分配收益:1MB 体,B/op 对比
// (prealloc ≈ 1×;ReadAll 倍增 ≈ 2×且多次分配)。
func BenchmarkReadRequestBody(b *testing.B) {
	gin.SetMode(gin.TestMode)
	payload := []byte(strings.Repeat("x", 1<<20))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	reset := func() {
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
		request.ContentLength = int64(len(payload))
		c.Request = request
	}
	b.Run("prealloc", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			reset()
			if _, err := readRequestBody(c, int64(32<<20)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("readall-baseline", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			reset()
			if _, err := io.ReadAll(c.Request.Body); err != nil {
				b.Fatal(err)
			}
		}
	})
}
