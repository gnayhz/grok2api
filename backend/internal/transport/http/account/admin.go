package account

import (
	"bytes"
	"encoding/json"
	"fmt"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	httphelpers "github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"io"
	"net/http"
	"strings"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

type updateRequest struct {
	Name                   *string                       `json:"name"`
	Enabled                *bool                         `json:"enabled"`
	Priority               *int                          `json:"priority"`
	MaxConcurrent          *int                          `json:"maxConcurrent"`
	MinimumRemaining       *float64                      `json:"minimumRemaining"`
	CloudflareCookies      *string                       `json:"cloudflareCookies"`
	ClearCloudflareCookies bool                          `json:"clearCloudflareCookies"`
	BuildSuperEntitled     *bool                         `json:"buildSuperEntitled"`
	BuildRouteMode         *accountdomain.BuildRouteMode `json:"buildRouteMode"`
	// RiskStatus 设置/清除长期风控标记；仅允许 "" 与 "rsc_denied"。
	RiskStatus *string `json:"riskStatus"`
}

type batchUpdateRequest struct {
	IDs              []string `json:"ids" binding:"required"`
	Provider         string   `json:"provider" binding:"required"`
	Enabled          *bool    `json:"enabled"`
	Priority         *int     `json:"priority"`
	MaxConcurrent    *int     `json:"maxConcurrent"`
	MinimumRemaining *float64 `json:"minimumRemaining"`
}

type batchDeleteRequest struct {
	IDs                 []string `json:"ids" binding:"required"`
	Provider            string   `json:"provider" binding:"required"`
	LinkedDeleteTargets []string `json:"linkedDeleteTargets"`
}

type deletionPreviewRequest struct {
	IDs                 []string `json:"ids" binding:"required"`
	Provider            string   `json:"provider" binding:"required"`
	LinkedDeleteTargets []string `json:"linkedDeleteTargets"`
}

type accountCleanupRequest struct {
	Provider            string                     `json:"provider" binding:"required"`
	Statuses            []accountapp.CleanupStatus `json:"statuses" binding:"required"`
	LinkedDeleteTargets []string                   `json:"linkedDeleteTargets"`
}

func (h *Handler) batchUpdate(c *gin.Context) {
	var request batchUpdateRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	updated, err := h.admin.BatchUpdate(c.Request.Context(), accountdomain.Provider(request.Provider), ids, accountapp.UpdateInput{Enabled: request.Enabled, Priority: request.Priority, MaxConcurrent: request.MaxConcurrent, MinimumRemaining: request.MinimumRemaining})
	if err != nil {
		h.writeServiceError(c, "accountBatchUpdateFailed", err, http.StatusInternalServerError, "批量更新账号失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"updated": updated})
}

func (h *Handler) batchDelete(c *gin.Context) {
	var request batchDeleteRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	result, err := h.admin.BatchDeleteWithLinked(c.Request.Context(), accountdomain.Provider(request.Provider), ids, targets)
	if err != nil {
		h.writeServiceError(c, "accountBatchDeleteFailed", err, http.StatusInternalServerError, "批量删除账号失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountDeleteResponse(result))
}

func (h *Handler) previewDeletion(c *gin.Context) {
	var request deletionPreviewRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	resolution, err := h.admin.PreviewLinkedDelete(c.Request.Context(), accountdomain.Provider(request.Provider), ids, targets)
	if err != nil {
		h.writeServiceError(c, "accountDeletionPreviewFailed", err, http.StatusInternalServerError, "预览删除账号失败")
		return
	}
	linked := gin.H{}
	for provider, count := range resolution.LinkedByProvider {
		linked[string(provider)] = count
	}
	response.Success(c, http.StatusOK, gin.H{
		"rootCount":        len(resolution.RootIDs),
		"linkedByProvider": linked,
		"total":            len(resolution.FinalIDs),
	})
}

func (h *Handler) cleanup(c *gin.Context) {
	var request accountCleanupRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	result, err := h.admin.CleanupAccounts(c.Request.Context(), accountdomain.Provider(request.Provider), request.Statuses, targets)
	if err != nil {
		h.writeServiceError(c, "accountCleanupFailed", err, http.StatusInternalServerError, "清理账号失败")
		return
	}
	byProvider := gin.H{}
	for provider, count := range result.DeletedByProvider {
		byProvider[string(provider)] = count
	}
	response.Success(c, http.StatusOK, gin.H{
		"deleted":           result.Deleted,
		"rootsDeleted":      result.RootsDeleted,
		"linkedDeleted":     result.LinkedDeleted,
		"skipped":           result.Skipped,
		"deletedByProvider": byProvider,
	})
}

// cleanupPreview returns root and linked-peer counts for the cleanup dialog.
func (h *Handler) cleanupPreview(c *gin.Context) {
	var request accountCleanupRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	preview, err := h.admin.PreviewCleanup(c.Request.Context(), accountdomain.Provider(request.Provider), request.Statuses, targets)
	if err != nil {
		h.writeServiceError(c, "accountCleanupPreviewFailed", err, http.StatusInternalServerError, "预览清理账号失败")
		return
	}
	rootsByStatus := gin.H{}
	for status, count := range preview.RootsByStatus {
		rootsByStatus[status] = count
	}
	linked := gin.H{}
	for provider, count := range preview.LinkedByProvider {
		linked[string(provider)] = count
	}
	response.Success(c, http.StatusOK, gin.H{
		"rootsByStatus":    rootsByStatus,
		"rootCount":        preview.RootCount,
		"linkedByProvider": linked,
		"total":            preview.Total,
	})
}

func (h *Handler) update(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	var request updateRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	var outcome accountsyncapp.UpdateWithSyncResult
	if h.modelSyncUpdates != nil {
		coordinated, err := h.modelSyncUpdates.UpdateWithModelSync(c.Request.Context(), id, accountapp.UpdateInput{
			Name: request.Name, Enabled: request.Enabled, Priority: request.Priority,
			MaxConcurrent: request.MaxConcurrent, MinimumRemaining: request.MinimumRemaining,
			CloudflareCookies: request.CloudflareCookies, ClearCloudflareCookies: request.ClearCloudflareCookies,
			BuildSuperEntitled: request.BuildSuperEntitled, BuildRouteMode: request.BuildRouteMode,
			RiskStatus: request.RiskStatus,
		})
		if err != nil {
			h.writeServiceError(c, "accountUpdateFailed", err, http.StatusInternalServerError, "更新账号失败")
			return
		}
		outcome = coordinated
	} else {
		// 未装配编排用例（测试/剥离形态）：仅执行已提交更新，不触发模型补齐。
		value, err := h.admin.Update(c.Request.Context(), id, accountapp.UpdateInput{
			Name: request.Name, Enabled: request.Enabled, Priority: request.Priority,
			MaxConcurrent: request.MaxConcurrent, MinimumRemaining: request.MinimumRemaining,
			CloudflareCookies: request.CloudflareCookies, ClearCloudflareCookies: request.ClearCloudflareCookies,
			BuildSuperEntitled: request.BuildSuperEntitled, BuildRouteMode: request.BuildRouteMode,
			RiskStatus: request.RiskStatus,
		})
		if err != nil {
			h.writeServiceError(c, "accountUpdateFailed", err, http.StatusInternalServerError, "更新账号失败")
			return
		}
		outcome.Account = value
	}
	result := newAccountResponse(outcome.Account)
	if outcome.Account.EnabledChanged && result.CooldownUntil != nil && time.Now().UTC().Before(*result.CooldownUntil) {
		result.EnabledDoesNotClearCooldown = true
	}
	result.ModelSyncFailed = outcome.ModelSyncFailed
	response.Success(c, http.StatusOK, result)
}

// clearCooldown 清除请求路径冷却但保留 missing_thinking strike(round 语义:
// strike 是持久质量污点,清除计时器不得让下一次 miss 变成"首击"而绕过
// 二击禁用策略)。
func (h *Handler) clearCooldown(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	value, err := h.admin.ClearCooldown(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "accountClearCooldownFailed", err, http.StatusInternalServerError, "清除账号冷却失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	var request struct {
		Provider            string   `json:"provider"`
		LinkedDeleteTargets []string `json:"linkedDeleteTargets"`
	}
	// Empty body = legacy single-account delete. Non-empty body must bind cleanly
	// so a truncated/malformed linked-delete request cannot silently drop targets.
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
			return
		}
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	if len(targets) > 0 {
		if request.Provider == "" {
			response.Error(c, http.StatusBadRequest, "invalidProvider", "删除关联账号时必须指定 provider")
			return
		}
		result, err := h.admin.DeleteWithLinked(c.Request.Context(), accountdomain.Provider(request.Provider), id, targets)
		if err != nil {
			h.writeServiceError(c, "accountDeleteFailed", err, http.StatusInternalServerError, "删除账号失败")
			return
		}
		response.Success(c, http.StatusOK, newAccountDeleteResponse(result))
		return
	}
	if err := h.admin.Delete(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "accountDeleteFailed", err, http.StatusInternalServerError, "删除账号失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func parseLinkedDeleteTargets(values []string) ([]accountdomain.Provider, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]accountdomain.Provider, 0, len(values))
	seen := map[accountdomain.Provider]struct{}{}
	for _, value := range values {
		provider := accountdomain.Provider(strings.TrimSpace(value))
		if !provider.IsValid() {
			return nil, fmt.Errorf("关联删除目标无效")
		}
		if _, ok := seen[provider]; ok {
			continue
		}
		seen[provider] = struct{}{}
		out = append(out, provider)
	}
	return out, nil
}

func newAccountDeleteResponse(result accountapp.AccountDeleteResult) gin.H {
	byProvider := gin.H{}
	for provider, count := range result.DeletedByProvider {
		byProvider[string(provider)] = count
	}
	return gin.H{
		"deleted":           result.Deleted,
		"rootsDeleted":      result.RootsDeleted,
		"linkedDeleted":     result.LinkedDeleted,
		"skipped":           result.Skipped,
		"deletedByProvider": byProvider,
	}
}

// clearCooldownUnconditional 是 clear-cooldown 的兼容别名路由：两者共享
// Service.ClearCooldown（保留 missing-thinking 打击标记，只清瞬态冷却），
// 失效事件同样携带保留后的标记，路由覆盖层与数据库终态一致。
func (h *Handler) clearCooldownUnconditional(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	value, err := h.admin.ClearCooldown(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "accountCooldownClearFailed", err, http.StatusInternalServerError, "解除账号冷却失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}
