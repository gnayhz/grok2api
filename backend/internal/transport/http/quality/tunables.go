package qualityhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

type TunablesStore interface {
	Read(context.Context) (management.Snapshot, error)
	Update(context.Context, uint64, management.Config) (management.Snapshot, error)
}

// QualityTunables exposes lifecycle bounds and their durable/application revisions.
type QualityTunables struct {
	management.Config
	Revision        uint64            `json:"revision,string"`
	AppliedRevision uint64            `json:"applied_revision,string"`
	ApplyPending    bool              `json:"apply_pending"`
	ApplyError      string            `json:"apply_error"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Applied         management.Config `json:"applied"`
}

func (h *Handler) tunablesDTO(state management.Snapshot) QualityTunables {
	return QualityTunables{Config: state.Config, Revision: state.Revision, AppliedRevision: state.AppliedRevision, ApplyPending: state.ApplyPending, ApplyError: state.ApplyError, UpdatedAt: state.UpdatedAt, Applied: state.Applied}
}

func (h *Handler) getSettings(c *gin.Context) {
	if h.deps.Tunables == nil {
		response.Error(c, http.StatusServiceUnavailable, "quality_tunables_unavailable", "参数面未接线")
		return
	}
	state, err := h.deps.Tunables.Read(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "quality_tunables_unavailable", err.Error())
		return
	}
	response.Success(c, http.StatusOK, h.tunablesDTO(state))
}

func (h *Handler) putSettings(c *gin.Context) {
	if h.deps.Tunables == nil {
		response.Error(c, http.StatusServiceUnavailable, "quality_tunables_unavailable", "参数面未接线")
		return
	}
	var input struct {
		management.Config
		Revision *string `json:"revision"`
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		response.Error(c, http.StatusBadRequest, "invalid_request", "request must contain one settings object")
		return
	}
	if input.Revision == nil {
		response.Error(c, http.StatusPreconditionRequired, "quality_settings_rejected", "请刷新并携带 revision 保存质量参数")
		return
	}

	revision, err := strconv.ParseUint(*input.Revision, 10, 64)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", "revision 必须是无符号整数的字符串")
		return
	}
	state, err := h.deps.Tunables.Update(c.Request.Context(), revision, input.Config)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, repository.ErrConflict) {
			status = http.StatusConflict
		}
		if errors.Is(err, management.ErrInvalidInput) {
			status = http.StatusBadRequest
		}
		response.Error(c, status, "quality_settings_rejected", err.Error())
		return
	}
	status := http.StatusOK
	if state.ApplyPending {
		status = http.StatusAccepted
	}
	response.Success(c, status, h.tunablesDTO(state))
}
