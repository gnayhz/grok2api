package qualityhttp

import (
	"context"
	"errors"
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

// QualityTunables keeps the existing flat response fields. Capacity is a
// read-only projection; revision and apply status refer only to quality policy.
type QualityTunables struct {
	management.Config
	MaxRotationsPerHour int               `json:"max_rotations_per_hour"`
	Revision            uint64            `json:"revision,string"`
	AppliedRevision     uint64            `json:"applied_revision,string"`
	ApplyPending        bool              `json:"apply_pending"`
	ApplyError          string            `json:"apply_error"`
	UpdatedAt           time.Time         `json:"updated_at"`
	Applied             management.Config `json:"applied"`
}

func (h *Handler) tunablesDTO(state management.Snapshot) QualityTunables {
	capacity := 0
	if h.deps.RotationCapacity != nil {
		capacity = h.deps.RotationCapacity()
	}
	return QualityTunables{Config: state.Config, MaxRotationsPerHour: capacity, Revision: state.Revision,
		AppliedRevision: state.AppliedRevision, ApplyPending: state.ApplyPending, ApplyError: state.ApplyError,
		UpdatedAt: state.UpdatedAt, Applied: state.Applied}
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
		Revision            *string `json:"revision"`
		MaxRotationsPerHour *int    `json:"max_rotations_per_hour"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if input.Revision == nil {
		response.Error(c, http.StatusPreconditionRequired, "quality_settings_rejected", "请刷新并携带 revision 保存质量参数")
		return
	}
	if input.MaxRotationsPerHour != nil && (h.deps.RotationCapacity == nil || *input.MaxRotationsPerHour != h.deps.RotationCapacity()) {
		response.Error(c, http.StatusBadRequest, "quality_settings_rejected", "轮换总容量请在出口轮换设置中修改")
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
