package account

import (
	httphelpers "github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

func (h *Handler) acceptWebTerms(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	if err := h.maintenance.AcceptWebTerms(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "webTermsAcceptanceFailed", err, http.StatusBadGateway, "接受 Grok Web 服务协议失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"completed": true})
}

func (h *Handler) setWebBirthDate(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	if err := h.maintenance.SetWebBirthDate(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "webBirthDateUpdateFailed", err, http.StatusBadGateway, "设置 Grok Web 账号生日失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"completed": true})
}

func (h *Handler) enableWebNSFW(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	if err := h.maintenance.EnableWebNSFW(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "webNSFWEnableFailed", err, http.StatusBadGateway, "开启 Grok Web NSFW 失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"completed": true})
}
