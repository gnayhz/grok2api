package middleware

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type encodingFlushWriter struct {
	*httptest.ResponseRecorder
	failure error
	flushed bool
}

func (w *encodingFlushWriter) FlushError() error { w.flushed = true; return w.failure }

func TestGzipPreservesStreamFlushFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, contentType := range []string{"text/event-stream", "audio/mpeg", "video/mp4"} {
		t.Run(contentType, func(t *testing.T) {
			failure := errors.New("socket flush failed")
			output := &encodingFlushWriter{ResponseRecorder: httptest.NewRecorder(), failure: failure}
			router := gin.New()
			router.Use(Gzip())
			router.GET("/", func(c *gin.Context) {
				c.Header("Content-Type", contentType)
				_, _ = c.Writer.Write([]byte("payload"))
				if err := http.NewResponseController(c.Writer).Flush(); !errors.Is(err, failure) {
					t.Errorf("flush failure lost through gzip/Gin: %v", err)
				}
			})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			router.ServeHTTP(output, req)
			if !output.flushed || output.Header().Get("Content-Encoding") != "" || output.Body.String() != "payload" {
				t.Fatalf("stream changed: flushed=%v encoding=%s bytes=%d", output.flushed, output.Header().Get("Content-Encoding"), output.Body.Len())
			}
		})
	}
}

func TestGzipEncodingFinishAndPanicRelease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, shouldPanic := range []bool{false, true} {
		router := gin.New()
		router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) { c.AbortWithStatus(500) }), Gzip())
		router.GET("/", func(c *gin.Context) {
			c.Header("Content-Type", "application/json")
			_, _ = c.Writer.Write([]byte(`{"value":"complete"}`))
			if shouldPanic {
				panic("encoding resource test")
			}
			if err := FinishResponseEncoding(c.Writer); err != nil {
				t.Fatal(err)
			}
			size := c.Writer.Size()
			if err := FinishResponseEncoding(c.Writer); err != nil || c.Writer.Size() != size {
				t.Fatalf("finish duplicated bytes: size=%d after=%d err=%v", size, c.Writer.Size(), err)
			}
			if _, err := c.Writer.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("write reused finished encoder: %v", err)
			}
		})
		// A subsequent request can reuse the pool without inheriting an ended or
		// failed stream. Panic must also finish/return the owned compressor.
		for range 2 {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			output := httptest.NewRecorder()
			router.ServeHTTP(output, req)
			zr, err := gzip.NewReader(output.Body)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(zr)
			_ = zr.Close()
			if err != nil || string(decoded) != `{"value":"complete"}` {
				t.Fatalf("encoding not completed exactly once: body=%s err=%v", decoded, err)
			}
		}
	}
}
