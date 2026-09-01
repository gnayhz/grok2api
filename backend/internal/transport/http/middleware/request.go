package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/gin-gonic/gin"
)

const RequestIDKey = "requestId"
const maxRequestIDLength = 64

// RequestID 为每个请求生成稳定关联 ID，并写入响应头。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if !validRequestID(requestID) {
			requestID, _ = security.NewOpaqueToken(12)
			if requestID == "" {
				requestID = "req-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			}
		}
		c.Set(RequestIDKey, requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

// ClientIP captures Gin's trusted-proxy-aware caller address once at ingress.
// The server config controls which reverse proxies may supply forwarding headers.
func ClientIP() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(requestmeta.WithClientIP(c.Request.Context(), c.ClientIP()))
		c.Next()
	}
}

// validRequestID 只接受适合写入日志和审计索引的短 ASCII 标识。
func validRequestID(value string) bool {
	if value == "" || len(value) > maxRequestIDLength {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') {
			continue
		}
		switch character {
		case '-', '_', '.', ':':
		default:
			return false
		}
	}
	return true
}

// Timeout 为 HTTP 请求设置统一生命周期上限。
func Timeout(duration time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), duration)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// MaxBodyBytes 对所有请求体应用统一硬上限，避免管理端绑定无界读取。
// XAI 视频 PUT（/v1/media/uploads/）使用更高的视频安全上限，由票据再次限长。
func MaxBodyBytes(limit int64) gin.HandlerFunc {
	const mediaUploadPathPrefix = "/v1/media/uploads/"
	const mediaUploadMaxBytes = 256 << 20
	return func(c *gin.Context) {
		if c.Request.Body != nil && limit > 0 {
			effective := limit
			if strings.HasPrefix(c.Request.URL.Path, mediaUploadPathPrefix) && c.Request.Method == http.MethodPut {
				if mediaUploadMaxBytes > effective {
					effective = mediaUploadMaxBytes
				}
			}
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, effective)
		}
		c.Next()
	}
}

// SecurityHeaders 为 API 和媒体响应添加通用浏览器安全边界。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		c.Next()
	}
}

// AccessLog 只记录路径、状态和耗时，不读取请求或响应正文。
// 5xx 额外计入 http_request_server_error_total（方法 × 状态码，有限标签
// 空间）：成功路径零额外开销，服务端故障面获得分钟级 performance_metric
// 速率可观测——此前 5xx 只存在于单条日志，无法与 CollectAndReset 周期
// 对齐做趋势/告警。
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		startedAt := time.Now()
		// emit 在 defer 中执行并对 panic 补记:panic 展开会跳过 c.Next() 之后的
		// 常规语句(故障期观测盲区),捕获后按 500 记账再抛回外层 gin.Recovery
		// 统一恢复——栈处理仍归 Recovery,访问日志不再缺行。
		defer func() {
			if recovered := recover(); recovered != nil {
				emitAccessLog(c, logger, startedAt, http.StatusInternalServerError)
				panic(recovered)
			}
			emitAccessLog(c, logger, startedAt, c.Writer.Status())
		}()
		c.Next()
	}
}

func emitAccessLog(c *gin.Context, logger *slog.Logger, startedAt time.Time, status int) {
	requestID, _ := c.Get(RequestIDKey)
	// c.FullPath() 只返回已注册路由模板；未匹配路由(404/405)为空。
	// 回退到原始 URL 路径，保证 404 风暴可定位到具体入口（round 7：
	// 实测未知路径的访问日志 path 为空，无法回答"什么在被打"）。
	path := c.FullPath()
	if path == "" {
		path = c.Request.URL.Path
	}
	logger.Info("http_request", "request_id", requestID, "method", c.Request.Method, "path", path, "status", status, "duration_ms", time.Since(startedAt).Milliseconds())
	if status >= http.StatusInternalServerError {
		perfmetrics.Default.Inc("http_request_server_error_total", perfmetrics.Labels{
			Subsystem: "http",
			Operation: path,
			Outcome:   strconv.Itoa(status),
		})
	}
}
