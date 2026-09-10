package middleware

import (
	"compress/gzip"
	"errors"
	"io"
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
	finished   bool
	finishErr  error
}

var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

func (w *gzipWriter) Write(data []byte) (int, error) {
	if w.finished {
		return 0, io.ErrClosedPipe
	}
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

// Unwrap 把包装链交给 http.ResponseController:没有它, SetWriteDeadline 会在此
// 层返回 ErrNotSupported, 声明接受 gzip 的客户端(浏览器默认)在 SSE/媒体转发
// 上失去 30s 写超时保护——停滞的 TCP 接收窗口可把 Write 阻塞到 RequestTimeout。
// gin 的 responseWriter 同样实现 Unwrap, 链条最终到达真实的 *http.response。
func (w *gzipWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *gzipWriter) Flush() { _ = w.FlushError() }

func (w *gzipWriter) FlushError() error {
	if w.finishErr != nil {
		return w.finishErr
	}
	if w.gz != nil && !w.finished {
		if err := w.gz.Flush(); err != nil {
			return err
		}
	}
	// Gin's void Flush would hide a network FlushError. Bypass its wrapper,
	// as inference does for an uncompressed response.
	var target http.ResponseWriter = w.ResponseWriter
	if wrapper, ok := target.(interface{ Unwrap() http.ResponseWriter }); ok {
		target = wrapper.Unwrap()
	}
	err := http.NewResponseController(target).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// FinishResponseEncoding drains encoder-owned bytes before a handler records
// delivery. It does not close the HTTP connection or assert client consumption.
// Ordinary writers have no encoding work; wrappers expose it through Unwrap.
func FinishResponseEncoding(writer http.ResponseWriter) error {
	for {
		if encoder, ok := writer.(interface{ FinishResponseEncoding() error }); ok {
			return encoder.FinishResponseEncoding()
		}
		wrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		writer = wrapper.Unwrap()
	}
}

func (w *gzipWriter) FinishResponseEncoding() error {
	if w.finished || w.gz == nil {
		return w.finishErr
	}
	w.finished = true
	w.finishErr = w.gz.Close()
	return w.finishErr
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
		defer func() {
			_ = rw.FinishResponseEncoding()
			if rw.gz != nil {
				gzipWriterPool.Put(rw.gz)
			}
			// A recovered panic or outer middleware must not start another
			// encoding stream after this wrapper relinquishes its compressor.
			rw.finished = true
		}()
		c.Next()
	}
}
