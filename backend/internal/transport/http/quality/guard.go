package qualityhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/guard"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type guardPolicyDTO struct {
	EvidenceTimeout      string   `json:"evidence_timeout"`
	CreatedTimeout       string   `json:"created_timeout"`
	AdmissionTimeout     string   `json:"admission_timeout"`
	ToolAdmissionTimeout string   `json:"tool_admission_timeout"`
	AccountCooldown      string   `json:"account_cooldown"`
	IdleAccountCooldown  string   `json:"idle_account_cooldown"`
	Enabled              bool     `json:"enabled"`
	GuardedModels        []string `json:"guarded_models"`
	MaxAttempts          int      `json:"max_attempts"`
	ReasoningExpected    bool     `json:"reasoning_expected"`
	ExhaustionPolicy     string   `json:"exhaustion_policy"`
}

type guardResponse struct {
	guardPolicyDTO
	Revision     uint64         `json:"revision,string"`
	FileDefaults guardPolicyDTO `json:"file_defaults"`
	SelfCheck    gin.H          `json:"self_check"`
	Updated      bool           `json:"updated,omitempty"`
}

func guardPolicyResponse(cfg guard.Config) guardPolicyDTO {
	return guardPolicyDTO{EvidenceTimeout: cfg.EvidenceTimeout.String(), CreatedTimeout: cfg.CreatedTimeout.String(),
		AdmissionTimeout: cfg.AdmissionTimeout.String(), ToolAdmissionTimeout: cfg.ToolAdmissionTimeout.String(),
		AccountCooldown: cfg.AccountCooldown.String(), IdleAccountCooldown: cfg.IdleAccountCooldown.String(),
		Enabled: cfg.Enabled, GuardedModels: cfg.GuardedModels, MaxAttempts: cfg.MaxAttempts,
		ReasoningExpected: cfg.ReasoningExpected, ExhaustionPolicy: cfg.ExhaustionPolicy()}
}
func (h *Handler) guardResponse(cfg guard.Config, updated bool) guardResponse {
	return guardResponse{guardPolicyDTO: guardPolicyResponse(cfg), Revision: cfg.Revision,
		FileDefaults: guardPolicyResponse(h.deps.Guard.FileDefaults()), SelfCheck: h.selfCheckPayload(), Updated: updated}
}
func (h *Handler) getGuard(c *gin.Context) {
	cfg, err := h.deps.Guard.Read(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "quality_guard_load_failed", "读取守卫配置失败")
		return
	}
	response.Success(c, http.StatusOK, h.guardResponse(cfg, false))
}

type guardConfigDTO struct {
	Revision             json.RawMessage `json:"revision"`
	EvidenceTimeout      string          `json:"evidence_timeout"`
	CreatedTimeout       string          `json:"created_timeout"`
	AdmissionTimeout     string          `json:"admission_timeout"`
	ToolAdmissionTimeout string          `json:"tool_admission_timeout"`
	AccountCooldown      string          `json:"account_cooldown"`
	IdleAccountCooldown  string          `json:"idle_account_cooldown"`
	Enabled              *bool           `json:"enabled"`
	GuardedModels        *[]string       `json:"guarded_models"`
	MaxAttempts          *int            `json:"max_attempts"`
	ReasoningExpected    *bool           `json:"reasoning_expected"`
}

// Legacy numeric revisions remain accepted only where JavaScript can represent
// them exactly. New clients always use decimal strings, including reset.
func guardRevision(raw json.RawMessage) (uint64, error) {
	value := string(raw)
	quoted := len(raw) > 0 && raw[0] == '"'
	if quoted {
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, err
		}
	}
	revision, err := strconv.ParseUint(value, 10, 64)
	if err != nil || (!quoted && revision > (1<<53)-1) {
		return 0, errors.New("revision 必须是十进制版本字符串")
	}
	return revision, nil
}
func (h *Handler) putGuard(c *gin.Context) {
	var dto guardConfigDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	revision, err := guardRevision(dto.Revision)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if dto.Enabled == nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", "enabled 字段必须显式传入")
		return
	}
	cfg := h.deps.Guard.Config()
	cfg.Revision, cfg.Enabled = revision, *dto.Enabled
	for _, item := range []struct {
		raw string
		dst *time.Duration
	}{
		{dto.EvidenceTimeout, &cfg.EvidenceTimeout}, {dto.CreatedTimeout, &cfg.CreatedTimeout},
		{dto.AdmissionTimeout, &cfg.AdmissionTimeout}, {dto.ToolAdmissionTimeout, &cfg.ToolAdmissionTimeout},
		{dto.AccountCooldown, &cfg.AccountCooldown}, {dto.IdleAccountCooldown, &cfg.IdleAccountCooldown},
	} {
		if item.raw == "" {
			continue
		}
		value, err := time.ParseDuration(item.raw)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalid_request", "守卫时长格式无效")
			return
		}
		*item.dst = value
	}
	if dto.GuardedModels != nil {
		cfg.GuardedModels = *dto.GuardedModels
	}
	if dto.MaxAttempts != nil {
		cfg.MaxAttempts = *dto.MaxAttempts
	}
	if dto.ReasoningExpected != nil {
		cfg.ReasoningExpected = *dto.ReasoningExpected
	}
	saved, err := h.deps.Guard.Update(c.Request.Context(), cfg)
	if err != nil {
		writeGuardError(c, err)
		return
	}
	response.Success(c, http.StatusOK, h.guardResponse(saved, true))
}
func (h *Handler) resetGuard(c *gin.Context) {
	var dto struct {
		Revision json.RawMessage `json:"revision"`
	}
	if err := c.ShouldBindJSON(&dto); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	revision, err := guardRevision(dto.Revision)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	saved, err := h.deps.Guard.ResetToDefaults(c.Request.Context(), revision)
	if err != nil {
		writeGuardError(c, err)
		return
	}
	response.Success(c, http.StatusOK, h.guardResponse(saved, true))
}
func writeGuardError(c *gin.Context, err error) {
	if errors.Is(err, guard.ErrConflict) {
		response.Error(c, http.StatusConflict, "quality_guard_conflict", "守卫配置已被其他会话更新，请重新载入")
		return
	}
	if errors.Is(err, guard.ErrInvalidJurisdiction) {
		response.Error(c, http.StatusBadRequest, "invalid_jurisdiction_entry", "管辖条目必须是公开模型名或受支持的渠道:模型名")
		return
	}
	if errors.Is(err, guard.ErrInvalidInput) {
		response.Error(c, http.StatusBadRequest, "quality_guard_update_rejected", err.Error())
		return
	}
	response.Error(c, http.StatusInternalServerError, "quality_guard_update_failed", "保存守卫配置失败")
}
