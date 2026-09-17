package system

import (
	"context"
	"net/http"
	"strings"

	updatecheckapp "github.com/chenyme/grok2api/backend/internal/application/updatecheck"
	"github.com/chenyme/grok2api/backend/internal/buildinfo"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

// versionResponse 在更新检查快照之上附带构建提交：仓库合并了修复不等于
// 线上在跑修复——事故的直接教训。嵌入结构体使原有字段平铺
// 不变，仅新增 buildCommit 一个键。
type versionResponse struct {
	updatecheckapp.Snapshot
	BuildCommit string `json:"buildCommit"`
}

type Handler struct {
	publicAPIBaseURL func() string
	updates          UpdateChecks
}

// UpdateChecks 是系统端点所需的更新检查能力(HTTP 消费面)。
type UpdateChecks interface {
	Snapshot() updatecheckapp.Snapshot
	Check(ctx context.Context) updatecheckapp.Snapshot
}

func NewHandler(publicAPIBaseURL func() string, updates UpdateChecks) *Handler {
	if publicAPIBaseURL == nil {
		publicAPIBaseURL = func() string { return "" }
	}
	return &Handler{publicAPIBaseURL: publicAPIBaseURL, updates: updates}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/system", h.get)
	router.GET("/system/version", h.version)
	router.POST("/system/update/check", h.checkUpdate)
}

func (h *Handler) version(c *gin.Context) {
	response.Success(c, http.StatusOK, versionResponse{Snapshot: h.updates.Snapshot(), BuildCommit: buildinfo.CurrentCommit()})
}

func (h *Handler) checkUpdate(c *gin.Context) {
	response.Success(c, http.StatusOK, h.updates.Check(c.Request.Context()))
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, gin.H{"publicApiBaseURL": strings.TrimRight(strings.TrimSpace(h.publicAPIBaseURL()), "/")})
}
