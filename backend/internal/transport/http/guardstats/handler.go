package guardstats

import (
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

// Handler 暴露守卫特征统计的只读快照。计数为进程本地(重启归零),多实例
// 部署时每实例各自统计,与 egress routing-stats 同语义。
type Handler struct{ gateway *gateway.Service }

func NewHandler(services ...*gateway.Service) *Handler {
	h := &Handler{}
	if len(services) > 0 {
		h.gateway = services[0]
	}
	return h
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/guard-stats", h.get)
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, h.gateway.GuardStatsSnapshotWithContext(c.Request.Context()))
}
