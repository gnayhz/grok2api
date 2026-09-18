package qualityhttp

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

func (h *Handler) postResourceChecks(c *gin.Context) {
	var req struct {
		Kind        string       `json:"kind" binding:"required"`
		ResourceIDs []resourceID `json:"resource_ids" binding:"required,min=1,max=32"`
		Model       string       `json:"model" binding:"required"`
	}
	if c.ShouldBindJSON(&req) != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", "invalid resource check request")
		return
	}
	if h.deps.ResourceChecks == nil {
		response.Error(c, http.StatusServiceUnavailable, "quality_check_unavailable", "resource checks unavailable")
		return
	}
	ids := make([]uint64, len(req.ResourceIDs))
	for i, id := range req.ResourceIDs {
		ids[i] = uint64(id)
	}
	items, err := h.deps.ResourceChecks.Start(c.Request.Context(), req.Kind, ids, req.Model)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "quality_check_failed", "unable to prepare resource checks")
		return
	}
	type submissionDTO struct {
		ResourceID resourceID `json:"resource_id"`
		ID         resourceID `json:"id,omitempty"`
		Error      string     `json:"error,omitempty"`
	}
	result := make([]submissionDTO, 0, len(items))
	for _, item := range items {
		result = append(result, submissionDTO{ResourceID: resourceID(item.ResourceID), ID: resourceID(item.ID), Error: item.Error})
	}
	response.Success(c, http.StatusAccepted, gin.H{"items": result})
}

func (h *Handler) getResourceChecks(c *gin.Context) {
	ids := []uint64{}
	parts := strings.Split(c.Query("resource_ids"), ",")
	if len(parts) > 32 {
		response.Error(c, http.StatusBadRequest, "invalid_request", "too many resources")
		return
	}
	for _, value := range parts {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			response.Error(c, http.StatusBadRequest, "invalid_request", "invalid resource IDs")
			return
		}
		ids = append(ids, id)
	}
	if h.deps.ResourceChecks == nil {
		response.Error(c, http.StatusServiceUnavailable, "quality_check_unavailable", "resource checks unavailable")
		return
	}
	items, err := h.deps.ResourceChecks.List(c.Request.Context(), c.Query("kind"), ids)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "quality_check_failed", "unable to read resource checks")
		return
	}
	result := make([]resourceCheckDTO, 0, len(items))
	for _, item := range items {
		result = append(result, checkDTO(item))
	}
	response.Success(c, http.StatusOK, gin.H{"items": result})
}
