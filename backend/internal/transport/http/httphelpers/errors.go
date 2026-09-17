package httphelpers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// OpenAIErrorType 按 HTTP 状态码推导 OpenAI 兼容错误信封的 type 字段：
// 401=authentication_error、429=rate_limit_error、5xx=server_error、
// 其余 invalid_request_error。middleware 与 inference handler 的历史口径一致。
func OpenAIErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// WriteOpenAIError 写出 OpenAI 兼容错误信封：
// {"error":{"message":...,"type":<按状态码推导>,"code":...,"param":null}}。
func WriteOpenAIError(c *gin.Context, status int, code, message string) {
	WriteOpenAIErrorTyped(c, status, OpenAIErrorType(status), code, nil, message)
}

// WriteOpenAIErrorTyped 写出显式 type 与 param 的 OpenAI 兼容错误信封
// （图片生成的 image_generation_user_error、带 param 的请求参数错误等）。
func WriteOpenAIErrorTyped(c *gin.Context, status int, errorType, code string, param any, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": errorType, "code": code, "param": param}})
}

// WriteAnthropicError 写出 Anthropic 错误信封：
// {"type":"error","error":{"type":...,"message":[...],"code":...}}。
// errorCode 省略或为空、或等于 upstream_unavailable 时不带 code 字段
// （与 inference handler 的历史口径一致）。
func WriteAnthropicError(c *gin.Context, status int, errorType, message string, errorCode ...string) {
	errorPayload := gin.H{"type": errorType, "message": message}
	if len(errorCode) > 0 && errorCode[0] != "" && errorCode[0] != "upstream_unavailable" {
		errorPayload["code"] = errorCode[0]
	}
	c.AbortWithStatusJSON(status, gin.H{"type": "error", "error": errorPayload})
}
