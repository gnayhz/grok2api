package account

import "github.com/gin-gonic/gin"

// writeAccountEvent 是账户 SSE 事件编码器的测试接缝：让编码格式可以脱离
// 心跳循环单独验证。它是测试专用包装，不属于生产 API，因此留在测试文件里，
// 生产文件 stream.go 只保留真实的事件流类型。
func writeAccountEvent(c *gin.Context, event string, value any) error {
	return (&accountEventStream{context: c}).Write(event, value)
}
