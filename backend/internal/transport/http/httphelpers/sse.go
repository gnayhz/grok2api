package httphelpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// StreamWriteTimeout 是流式（SSE/媒体）响应每次写入的截止时间。历史实现
// 在 account/model/inference 三处各自维护同值的 30s 常量，统一到这里。
const StreamWriteTimeout = 30 * time.Second

// SSEHeaders 设置 SSE 响应所需的三个响应头。管理面所有事件流共用。
func SSEHeaders(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("X-Accel-Buffering", "no")
}

// SetStreamWriteDeadline 为流式响应设置写入截止时间；ResponseWriter 不支持
// 写截止时间（httptest 等）时返回 nil，与历史实现的 ErrNotSupported 忽略一致。
func SetStreamWriteDeadline(writer http.ResponseWriter) error {
	err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(StreamWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// SSEEvent 写出一帧 event/data：JSON 编码 value、设置写截止时间、Flush，
// 最后返回请求上下文错误（客户端断开）。
func SSEEvent(c *gin.Context, event string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := SetStreamWriteDeadline(c.Writer); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	c.Writer.Flush()
	return c.Request.Context().Err()
}

// SSEComment 写出一帧注释（连接占位与心跳）。
func SSEComment(c *gin.Context, comment string) error {
	if err := SetStreamWriteDeadline(c.Writer); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.Writer, ": %s\n\n", comment); err != nil {
		return err
	}
	c.Writer.Flush()
	return c.Request.Context().Err()
}
