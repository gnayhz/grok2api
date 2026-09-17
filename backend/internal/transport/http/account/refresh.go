package account

import (
	"encoding/json"
	"errors"
	httphelpers "github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"io"
	"net/http"
	"strconv"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

// batchAccountActionRequest 是 refresh/reset 族批量操作的独立入参：此前
// 四个 handler 复用 batchDeleteRequest，缺字段时校验错误把操作报成
// "batchDeleteRequest.Xxx"，且 linkedDeleteTargets 对这些操作毫无意义
// （round 77 活体复现：POST /accounts/batch/refresh-tokens 缺 provider
// 返回 Key: 'batchDeleteRequest.Provider'，误导 API 调用方）。
type batchAccountActionRequest struct {
	IDs      []string `json:"ids" binding:"required"`
	Provider string   `json:"provider" binding:"required"`
}

// detectBuildAccountsRequest 要求显式选择全部账号或提供非空 id 集合。
type detectBuildAccountsRequest struct {
	IDs      []string `json:"ids"`
	All      bool     `json:"all"`
	Provider string   `json:"provider"`
}

// accountDetectItemResponse 是检测任务的单账号增量事件；全量检测仅推送 invalid。
type accountDetectItemResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
}

type accountBatchResponse struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

type accountTokenRefreshResponse struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

func (h *Handler) batchRefreshBilling(c *gin.Context) {
	var request batchAccountActionRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if request.Provider != string(accountdomain.ProviderBuild) {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "仅 Grok Build 账号支持 Billing 同步")
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	succeeded, failed, err := h.maintenance.BatchRefreshBilling(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "billingBatchRefreshFailed", err, http.StatusBadGateway, "批量同步 Billing 失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"succeeded": succeeded, "failed": failed})
}

func (h *Handler) detectBuildAccounts(c *gin.Context) {
	var request detectBuildAccountsRequest
	if c.Request.Body != nil {
		if err := json.NewDecoder(c.Request.Body).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
			return
		}
	}
	if request.Provider != "" && request.Provider != string(accountdomain.ProviderBuild) {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "仅 Grok Build 账号支持可用性检测")
		return
	}
	hasIDs := len(request.IDs) > 0
	if request.All == hasIDs {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "必须明确选择全部账号或提供非空账号 ID")
		return
	}
	var ids []uint64
	if hasIDs {
		parsed, err := parseIDs(request.IDs)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
			return
		}
		if !h.validateProviderIDs(c, parsed, string(accountdomain.ProviderBuild)) {
			return
		}
		ids = parsed
	}
	stream := newAccountEventStream(c)
	defer stream.Close()
	itemObserver := func(item accountapp.BuildDetectItemResult) error {
		return stream.Write("item", accountDetectItemResponse{
			ID:         strconv.FormatUint(item.AccountID, 10),
			Name:       item.Name,
			Email:      item.Email,
			Outcome:    string(item.Outcome),
			Reason:     item.Reason,
			HTTPStatus: item.HTTPStatus,
		})
	}
	succeeded, failed, err := h.maintenance.DetectBuildAccountsWithProgress(c.Request.Context(), ids, request.All, stream.ProgressObserver(), itemObserver)
	if err != nil {
		stream.WriteError("accountDetectFailed", "检测 Grok Build 账号失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}

func (h *Handler) batchResetQuota(c *gin.Context) {
	var request batchAccountActionRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if request.Provider != string(accountdomain.ProviderBuild) {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "仅 Grok Build 账号支持手动重置额度状态")
		return
	}
	reset, err := h.maintenance.BatchResetQuotaState(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "quotaBatchResetFailed", err, http.StatusInternalServerError, "批量重置额度状态失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"reset": reset})
}

func (h *Handler) resetAllBuildQuota(c *gin.Context) {
	reset, err := h.maintenance.ResetAllBuildQuotaState(c.Request.Context())
	if err != nil {
		h.writeServiceError(c, "quotaResetFailed", err, http.StatusInternalServerError, "重置全部 Grok Build 额度状态失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"reset": reset})
}

func (h *Handler) batchRefreshQuotas(c *gin.Context) {
	var request batchAccountActionRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	providerValue := accountdomain.Provider(request.Provider)
	if !providerValue.IsValid() {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "账号来源无效")
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	var succeeded, failed int
	if providerValue == accountdomain.ProviderBuild {
		succeeded, failed, err = h.maintenance.BatchRefreshBilling(c.Request.Context(), ids)
	} else {
		succeeded, failed, err = h.maintenance.BatchRefreshQuota(c.Request.Context(), ids)
	}
	if err != nil {
		h.writeServiceError(c, "quotaBatchRefreshFailed", err, http.StatusBadGateway, "批量同步账号额度失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"succeeded": succeeded, "failed": failed})
}

func (h *Handler) batchRefreshTokens(c *gin.Context) {
	var request batchAccountActionRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if request.Provider != string(accountdomain.ProviderBuild) {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "仅 Grok Build 账号支持凭据刷新")
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	succeeded, failed, skipped, err := h.maintenance.BatchRefreshTokens(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "tokenRefreshFailed", err, http.StatusBadGateway, "批量刷新账号凭据失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"succeeded": succeeded, "failed": failed, "skipped": skipped})
}

func (h *Handler) refreshWebQuota(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	if _, err := h.maintenance.RefreshQuota(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "quotaRefreshFailed", err, http.StatusBadGateway, "同步 Provider 额度失败")
		return
	}
	value, err := h.admin.Get(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "accountGetFailed", err, http.StatusInternalServerError, "读取账号失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) refreshToken(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	value, err := h.maintenance.RefreshToken(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "tokenRefreshFailed", err, http.StatusBadGateway, "刷新账号凭据失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) refreshBilling(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	value, err := h.maintenance.RefreshBilling(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "billingRefreshFailed", err, http.StatusBadGateway, "刷新账号额度失败")
		return
	}
	response.Success(c, http.StatusOK, newBillingResponse(value))
}

func (h *Handler) refreshAllBilling(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, err := h.maintenance.SyncAllBillingWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("billingRefreshFailed", "刷新账号额度失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}

func (h *Handler) refreshAllTokens(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, skipped, err := h.maintenance.RefreshAllTokensWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("tokenRefreshFailed", "续期账号凭据失败")
		return
	}
	_ = stream.Write("complete", accountTokenRefreshResponse{Succeeded: succeeded, Failed: failed, Skipped: skipped})
}

func (h *Handler) refreshAllWebQuotas(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, err := h.maintenance.SyncAllWebQuotasWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("quotaRefreshFailed", "同步 Grok Web 账号额度失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}

func (h *Handler) refreshAllConsoleQuotas(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, err := h.maintenance.SyncAllConsoleQuotasWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("quotaRefreshFailed", "同步 Grok Console 账号额度失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}
