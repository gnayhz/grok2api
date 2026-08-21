package middleware

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// gzipWriter 在首个 Write 时按响应 Content-Type 决定是否 gzip 压缩：
// 仅压缩静态资产与 JSON（可缓冲整段），SSE/流式推理响应原样透传
// （Flusher 语义必须保留——守卫的实时扣留判决依赖逐块下发）。
type gzipWriter struct {
	gin.ResponseWriter
	underlying http.ResponseWriter
	gz         *gzip.Writer
	started    bool
	skip       bool
}

var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

func (w *gzipWriter) Write(data []byte) (int, error) {
	if !w.started {
		w.started = true
		ct := w.Header().Get("Content-Type")
		if strings.Contains(ct, "text/event-stream") || strings.Contains(ct, "audio/") || strings.Contains(ct, "video/") {
			w.skip = true
		} else {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Del("Content-Length")
			gz := gzipWriterPool.Get().(*gzip.Writer)
			gz.Reset(w.underlying)
			w.gz = gz
		}
	}
	if w.skip {
		return w.ResponseWriter.Write(data)
	}
	return w.gz.Write(data)
}

func (w *gzipWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *gzipWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	w.ResponseWriter.Flush()
}

// Gzip 按请求协商启用响应压缩（Accept-Encoding: gzip）。
func Gzip() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.Contains(c.Request.Header.Get("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}
		rw := &gzipWriter{ResponseWriter: c.Writer, underlying: c.Writer}
		c.Writer = rw
		c.Next()
		if rw.gz != nil {
			_ = rw.gz.Close()
			gzipWriterPool.Put(rw.gz)
		}
	}
}
