package qualityhttp

import (
	"errors"
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

func (h *Handler) postAccountCheck(c *gin.Context) {
	id, ok := httphelpers.PathUint(c, "id")
	var input struct {
		Model string `json:"model" binding:"required"`
	}
	if !ok || c.ShouldBindJSON(&input) != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", "account and model required")
		return
	}
	if h.deps.AccountChecks == nil {
		response.Error(c, http.StatusServiceUnavailable, "quality_check_unavailable", "quality checks unavailable")
		return
	}
	checkID, err := h.deps.AccountChecks.Start(c.Request.Context(), id, input.Model)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, management.ErrCheckQueueFull) {
			status = http.StatusTooManyRequests
		}
		response.Error(c, status, "quality_check_failed", err.Error())
		return
	}
	response.Success(c, http.StatusAccepted, gin.H{"id": checkID})
}

func (h *Handler) getAccountChecks(c *gin.Context) {
	id, ok := httphelpers.PathUint(c, "id")
	if !ok {
		response.Error(c, http.StatusBadRequest, "invalid_request", "account required")
		return
	}
	if h.deps.AccountChecks == nil {
		response.Error(c, http.StatusServiceUnavailable, "quality_check_unavailable", "quality checks unavailable")
		return
	}
	items, err := h.deps.AccountChecks.List(c.Request.Context(), id)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "quality_check_failed", "failed to read quality checks")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}
