package account

import (
	"errors"
	"net/http"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

func (h *Handler) startDevice(c *gin.Context) {
	value, err := h.maintenance.StartDeviceLogin(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusBadGateway, "deviceLoginStartFailed", "启动 Device OAuth 失败")
		return
	}
	response.Success(c, http.StatusCreated, gin.H{"sessionId": value.SessionID, "userCode": value.UserCode, "verificationUri": value.VerificationURI, "verificationUriComplete": value.VerificationURIComplete, "intervalSeconds": int(value.Interval.Seconds()), "expiresAt": value.ExpiresAt})
}

func (h *Handler) pollDevice(c *gin.Context) {
	completion, err := h.deviceOnboarding.CompleteDeviceLogin(c.Request.Context(), c.Param("sessionId"))
	if errors.Is(err, accountapp.ErrDevicePending) {
		response.Success(c, http.StatusAccepted, gin.H{"status": "pending"})
		return
	}
	if errors.Is(err, accountapp.ErrDeviceSlowDown) {
		response.Error(c, http.StatusTooManyRequests, "devicePollTooFast", "轮询过快，请稍后重试")
		return
	}
	if errors.Is(err, accountapp.ErrDeviceDenied) {
		response.Error(c, http.StatusGone, "deviceLoginExpired", "Device OAuth 已拒绝或过期")
		return
	}
	if err != nil {
		response.Error(c, http.StatusBadGateway, "deviceLoginFailed", "Device OAuth 登录失败")
		return
	}
	status := "succeeded"
	if completion.Sync.Failed > 0 {
		status = "syncFailed"
	}
	response.Success(c, http.StatusOK, gin.H{"status": status, "account": newAccountResponse(completion.Account), "synced": completion.Sync.Succeeded, "syncFailed": completion.Sync.Failed})
}
