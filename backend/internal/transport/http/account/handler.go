package account

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

const (
	maxAccountImportBytes         = 30 << 20
	maxAccountImportFiles         = 1000
	accountEventHeartbeatInterval = 15 * time.Second
)

// Dependencies are assembled in app; each field exposes one account capability.
type Dependencies struct {
	Administration accountapp.Administration
	Credentials    accountapp.CredentialTransfer
	Maintenance    accountapp.Maintenance
	Onboarding     *accountsyncapp.Onboarding
	// DeviceOnboarding 完成 Device 接入编排（App 注入账号同步协调者）。
	DeviceOnboarding deviceOnboardingCoordinator
	// ModelSyncUpdates 更新账号并按需补齐模型能力（App 注入）。
	ModelSyncUpdates modelSyncUpdateCoordinator
}

type deviceOnboardingCoordinator interface {
	CompleteDeviceLogin(ctx context.Context, sessionID string) (accountsyncapp.DeviceCompletion, error)
}

type modelSyncUpdateCoordinator interface {
	UpdateWithModelSync(ctx context.Context, id uint64, input accountapp.UpdateInput) (accountsyncapp.UpdateWithSyncResult, error)
}

type Handler struct {
	maintenance accountapp.Maintenance
	importer    accountapp.CredentialTransfer
	admin       accountapp.Administration
	onboarding  *accountsyncapp.Onboarding
	// deviceOnboarding 完成 Device 接入编排（轮询+初始同步+读回）。
	deviceOnboarding deviceOnboardingCoordinator
	// modelSyncUpdates 更新账号并按需补齐模型能力。
	modelSyncUpdates modelSyncUpdateCoordinator
	logger           *slog.Logger
	// qualityStates 质量轴状态注入缝隙(依赖倒置,出口节点同款):
	// 组合根注入质量层读函数;nil=质量层剥离态,账号响应不含质量字段。
	qualityStates func() map[uint64]AccountQualityState
}

// AccountQualityState 是账号质量轴状态的投影(质量层提供;词汇为纯
// 数据,底座不理解羁押/服刑语义,只透传展示)。
type AccountQualityState struct {
	State  string `json:"state"`                   // remanded | sentenced
	CaseID uint64 `json:"caseId,omitempty,string"` // 关联案件号(可解释性)
}

// SetQualityStates 安装质量状态注入缝隙(nil 保持未设——剥离形态)。
func (h *Handler) SetQualityStates(provider func() map[uint64]AccountQualityState) {
	if provider == nil {
		return
	}
	h.qualityStates = provider
}

// attachQualityStates 旁注质量轴状态(缝隙未注入时空转——剥离形态
// 账号列表照常,只是没有质量徽章)。
func (h *Handler) attachQualityStates(items []accountResponse) {
	if h.qualityStates == nil {
		return
	}
	states := h.qualityStates()
	if len(states) == 0 {
		return
	}
	for i := range items {
		if state, ok := states[items[i].ID]; ok {
			quality := state
			items[i].Quality = &quality
		}
	}
}

func NewHandler(deps Dependencies, logger ...*slog.Logger) *Handler {
	instance := &Handler{maintenance: deps.Maintenance, importer: deps.Credentials, admin: deps.Administration, onboarding: deps.Onboarding, deviceOnboarding: deps.DeviceOnboarding, modelSyncUpdates: deps.ModelSyncUpdates}
	if len(logger) > 0 && logger[0] != nil {
		instance.logger = logger[0]
	}
	return instance
}

// log 返回已注入的 logger；未注入时回退 slog.Default()（测试/零值构造）。
func (h *Handler) log() *slog.Logger {
	if h.logger != nil {
		return h.logger
	}
	return slog.Default()
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/accounts", h.list)
	router.GET("/accounts/summary", h.summary)
	router.GET("/accounts/export", h.exportCredentials)
	router.POST("/accounts/export", h.exportSelectedCredentials)
	router.GET("/accounts/:id", h.get)
	router.POST("/accounts/device/start", h.startDevice)
	router.POST("/accounts/device/:sessionId/poll", h.pollDevice)
	router.POST("/accounts/import", h.importAuth)
	router.POST("/accounts/web/import", h.importWebAuth)
	router.POST("/accounts/console/import", h.importConsoleAuth)
	router.POST("/accounts/web/convert-to-build", h.convertWebToBuild)
	router.POST("/accounts/web/sync-to-console", h.syncWebToConsole)
	router.POST("/accounts/web/run-scripts", h.runWebAccountScripts)
	router.POST("/accounts/web/refresh-quotas", h.refreshAllWebQuotas)
	router.POST("/accounts/web/:id/accept-terms", h.acceptWebTerms)
	router.POST("/accounts/web/:id/birth-date", h.setWebBirthDate)
	router.POST("/accounts/web/:id/nsfw", h.enableWebNSFW)
	router.POST("/accounts/console/refresh-quotas", h.refreshAllConsoleQuotas)
	router.POST("/accounts/refresh-billing", h.refreshAllBilling)
	router.POST("/accounts/reset-quota", h.resetAllBuildQuota)
	router.POST("/accounts/refresh-tokens", h.refreshAllTokens)
	router.POST("/accounts/cleanup", h.cleanup)
	router.POST("/accounts/cleanup-preview", h.cleanupPreview)
	router.POST("/accounts/batch/refresh-billing", h.batchRefreshBilling)
	router.POST("/accounts/batch/reset-quota", h.batchResetQuota)
	router.POST("/accounts/batch/refresh-quotas", h.batchRefreshQuotas)
	router.POST("/accounts/batch/refresh-tokens", h.batchRefreshTokens)
	router.POST("/accounts/detect", h.detectBuildAccounts)
	router.PATCH("/accounts/batch", h.batchUpdate)
	router.POST("/accounts/deletion-preview", h.previewDeletion)
	router.DELETE("/accounts", h.batchDelete)
	router.PATCH("/accounts/:id", h.update)
	router.DELETE("/accounts/:id", h.delete)
	router.POST("/accounts/:id/clear-cooldown", h.clearCooldown)
	router.POST("/accounts/:id/clear-cooldown-force", h.clearCooldownUnconditional)
	router.POST("/accounts/:id/refresh-token", h.refreshToken)
	router.POST("/accounts/:id/refresh-billing", h.refreshBilling)
	router.POST("/accounts/:id/refresh-quota", h.refreshWebQuota)
}

type accountResponse struct {
	ID                       uint64     `json:"id,string"`
	Provider                 string     `json:"provider"`
	AuthType                 string     `json:"authType"`
	WebTier                  string     `json:"webTier,omitempty"`
	WebTierSyncedAt          *time.Time `json:"webTierSyncedAt,omitempty"`
	WebNSFWEnabledAt         *time.Time `json:"nsfwEnabledAt,omitempty"`
	WebTermsAcceptedAt       *time.Time `json:"termsAcceptedAt,omitempty"`
	Name                     string     `json:"name"`
	Email                    string     `json:"email,omitempty"`
	UserID                   string     `json:"userId,omitempty"`
	TeamID                   string     `json:"teamId,omitempty"`
	Enabled                  bool       `json:"enabled"`
	AuthStatus               string     `json:"authStatus"`
	AuthError                string     `json:"authError,omitempty"`
	ExpiresAt                *time.Time `json:"expiresAt,omitempty"`
	Refreshable              bool       `json:"refreshable"`
	RefreshDueAt             *time.Time `json:"refreshDueAt,omitempty"`
	LastRefreshAt            *time.Time `json:"lastRefreshAt,omitempty"`
	RefreshFailures          int        `json:"refreshFailureCount"`
	LastRefreshErrorStatus   int        `json:"lastRefreshErrorStatus,omitempty"`
	LastRefreshError         string     `json:"lastRefreshErrorCode,omitempty"`
	LastRefreshErrorMessage  string     `json:"lastRefreshErrorMessage,omitempty"`
	LastRefreshErrorResponse string     `json:"lastRefreshErrorResponse,omitempty"`
	Priority                 int        `json:"priority"`
	MaxConcurrent            int        `json:"maxConcurrent"`
	MinimumRemaining         float64    `json:"minimumRemaining"`
	FailureCount             int        `json:"failureCount"`
	CooldownUntil            *time.Time `json:"cooldownUntil,omitempty"`
	LastError                string     `json:"lastError,omitempty"`
	// EnabledDoesNotClearCooldown is set on PATCH when enabled was changed
	// while the account is still cooling. Toggling enabled is not a health reset.
	EnabledDoesNotClearCooldown bool                    `json:"enabledDoesNotClearCooldown,omitempty"`
	LastUsedAt                  *time.Time              `json:"lastUsedAt,omitempty"`
	LinkedAccountID             uint64                  `json:"linkedAccountId,omitempty,string"`
	LinkedName                  string                  `json:"linkedAccountName,omitempty"`
	LinkedProvider              string                  `json:"linkedProvider,omitempty"`
	LinkedAccounts              []linkedAccountResponse `json:"linkedAccounts,omitempty"`
	CreatedAt                   time.Time               `json:"createdAt"`
	ObservedModel               string                  `json:"observedModel,omitempty"`
	ObservedModelAt             *time.Time              `json:"observedModelAt,omitempty"`
	CloudflareCookieConfigured  bool                    `json:"cloudflareCookieConfigured"`
	BuildSuperEntitled          bool                    `json:"buildSuperEntitled"`
	BuildRouteMode              string                  `json:"buildRouteMode"`
	BuildBotFlagged             bool                    `json:"buildBotFlagged"`
	BuildBotFlagSource          int                     `json:"buildBotFlagSource,omitempty"`
	// RiskStatus 非空表示长期风控标记（当前 rsc_denied = RSC 注册风控）。
	RiskStatus          string                `json:"riskStatus,omitempty"`
	RiskTrigger         string                `json:"riskTrigger,omitempty"`
	RiskOriginAccountID uint64                `json:"riskOriginAccountId,omitempty,string"`
	RiskCheckedAt       *time.Time            `json:"riskCheckedAt,omitempty"`
	RiskDetail          string                `json:"riskDetail,omitempty"`
	ModelSyncFailed     bool                  `json:"modelSyncFailed,omitempty"`
	Billing             *billingResponse      `json:"billing,omitempty"`
	Quota               quotaResponse         `json:"quota"`
	QuotaWindows        []quotaWindowResponse `json:"quotaWindows,omitempty"`
	// Quality 质量轴(裁决亭)状态徽章;nil=未被裁决亭动过。
	Quality *AccountQualityState `json:"quality,omitempty"`
}

type linkedAccountResponse struct {
	ID       uint64 `json:"id,string"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	UserID   string `json:"userId,omitempty"`
}

type quotaWindowResponse struct {
	Mode          string                   `json:"mode"`
	Remaining     int                      `json:"remaining"`
	Total         int                      `json:"total"`
	UsagePercent  float64                  `json:"usagePercent"`
	Breakdown     []quotaBreakdownResponse `json:"breakdown,omitempty"`
	WindowSeconds int                      `json:"windowSeconds"`
	ResetAt       *time.Time               `json:"resetAt,omitempty"`
	SyncedAt      *time.Time               `json:"syncedAt,omitempty"`
	Source        string                   `json:"source"`
}

type quotaBreakdownResponse struct {
	ProductCode  int     `json:"productCode"`
	UsagePercent float64 `json:"usagePercent"`
}

type billingResponse struct {
	PlanCode             string                   `json:"planCode,omitempty"`
	PlanName             string                   `json:"planName,omitempty"`
	MonthlyLimit         float64                  `json:"monthlyLimit"`
	Used                 float64                  `json:"used"`
	Remaining            float64                  `json:"remaining"`
	OnDemandCap          float64                  `json:"onDemandCap"`
	OnDemandUsed         float64                  `json:"onDemandUsed"`
	PrepaidBalance       float64                  `json:"prepaidBalance"`
	CreditUsagePercent   float64                  `json:"creditUsagePercent"`
	IsUnifiedBillingUser bool                     `json:"isUnifiedBillingUser"`
	OnDemandEnabled      *bool                    `json:"onDemandEnabled,omitempty"`
	TopUpMethod          string                   `json:"topUpMethod,omitempty"`
	UsagePeriodType      string                   `json:"usagePeriodType,omitempty"`
	UsagePeriodStart     string                   `json:"usagePeriodStart,omitempty"`
	UsagePeriodEnd       string                   `json:"usagePeriodEnd,omitempty"`
	BillingPeriodStart   string                   `json:"billingPeriodStart,omitempty"`
	BillingPeriodEnd     string                   `json:"billingPeriodEnd,omitempty"`
	History              []billingHistoryResponse `json:"history,omitempty"`
	SyncedAt             time.Time                `json:"syncedAt"`
}

type billingHistoryResponse struct {
	Year         int     `json:"year"`
	Month        int     `json:"month"`
	PeriodType   string  `json:"periodType,omitempty"`
	PeriodStart  string  `json:"periodStart,omitempty"`
	PeriodEnd    string  `json:"periodEnd,omitempty"`
	IncludedUsed float64 `json:"includedUsed"`
	OnDemandUsed float64 `json:"onDemandUsed"`
	TotalUsed    float64 `json:"totalUsed"`
}

type quotaResponse struct {
	Type            string     `json:"type"`
	Source          string     `json:"source"`
	Confidence      string     `json:"confidence"`
	Unit            string     `json:"unit,omitempty"`
	Used            float64    `json:"used"`
	Limit           float64    `json:"limit"`
	Remaining       float64    `json:"remaining"`
	UsagePercent    float64    `json:"usagePercent"`
	LimitKnown      bool       `json:"limitKnown"`
	WindowHours     int        `json:"windowHours,omitempty"`
	Observed        bool       `json:"observed"`
	Confirmed       bool       `json:"confirmed"`
	Status          string     `json:"status"`
	PeriodStart     string     `json:"periodStart,omitempty"`
	PeriodEnd       string     `json:"periodEnd,omitempty"`
	ExhaustedAt     *time.Time `json:"exhaustedAt,omitempty"`
	NextProbeAt     *time.Time `json:"nextProbeAt,omitempty"`
	LastConfirmedAt *time.Time `json:"lastConfirmedAt,omitempty"`
}

// writeServiceError 仅暴露明确的账号业务错误，未知内部错误使用稳定文案。
func (h *Handler) writeServiceError(c *gin.Context, code string, err error, fallbackStatus int, fallbackMessage string) {
	switch {
	case errors.Is(err, accountapp.ErrImportLimit):
		response.Error(c, http.StatusBadRequest, "accountImportLimitExceeded", err.Error())
	case errors.Is(err, accountapp.ErrExportLimit):
		response.Error(c, http.StatusBadRequest, "accountExportLimitExceeded", err.Error())
	case errors.Is(err, accountapp.ErrInvalidInput), errors.Is(err, accountapp.ErrInvalidImport):
		response.Error(c, http.StatusBadRequest, code, err.Error())
	case errors.Is(err, accountapp.ErrAccountPoolMismatch):
		response.Error(c, http.StatusConflict, "accountPoolMismatch", err.Error())
	case errors.Is(err, accountapp.ErrConflict):
		response.Error(c, http.StatusConflict, code, err.Error())
	case errors.Is(err, accountapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "accountNotFound", err.Error())
	case errors.Is(err, accountapp.ErrUnsupported):
		response.Error(c, http.StatusConflict, "accountOperationUnsupported", err.Error())
	case errors.Is(err, accountapp.ErrConversionBusy):
		response.Error(c, http.StatusConflict, "accountConversionBusy", err.Error())
	case errors.Is(err, accountapp.ErrWebAccountScriptBusy):
		response.Error(c, http.StatusConflict, "webAccountScriptBusy", err.Error())
	default:
		// 未分类错误（仓储故障等）此前只返回通用文案且不留痕——运维无从
		// 定位（round 98 活体复现：手工种子数据触发 500 时服务端零日志）。
		// 与 round 96 的 egress writeError 处理对齐：真实原因落日志。
		h.log().Error("account_service_error", "error", err, "code", code, "request_id", c.Value(middleware.RequestIDKey))
		response.Error(c, fallbackStatus, code, fallbackMessage)
	}
}

func newAccountResponse(value accountapp.View) accountResponse {
	c := value.Credential
	buildRouteMode := c.BuildRouteMode
	if c.Provider != accountdomain.ProviderBuild || !buildRouteMode.IsValid() {
		buildRouteMode = accountdomain.BuildRouteAuto
	}
	result := accountResponse{
		ID: c.ID, Provider: string(c.Provider), AuthType: string(c.AuthType), WebTier: string(c.WebTier),
		WebTierSyncedAt: c.WebTierSyncedAt, WebNSFWEnabledAt: c.WebNSFWEnabledAt, WebTermsAcceptedAt: c.WebTermsAcceptedAt, Name: c.Name, Email: c.Email, UserID: c.UserID, TeamID: c.TeamID,
		Enabled: c.Enabled, AuthStatus: string(c.AuthStatus), AuthError: c.AuthError, Refreshable: c.EncryptedRefreshToken != "",
		RefreshDueAt: c.RefreshDueAt, LastRefreshAt: c.LastRefreshAt,
		RefreshFailures: c.RefreshFailureCount, LastRefreshErrorStatus: c.LastRefreshErrorStatus, LastRefreshError: c.LastRefreshErrorCode, LastRefreshErrorMessage: c.LastRefreshErrorMessage, LastRefreshErrorResponse: c.LastRefreshErrorResponse,
		Priority: c.Priority, MaxConcurrent: c.MaxConcurrent, MinimumRemaining: c.MinimumRemaining,
		FailureCount: c.FailureCount, CooldownUntil: c.CooldownUntil, LastError: c.LastError,
		LastUsedAt: c.LastUsedAt, LinkedAccountID: c.LinkedAccountID, LinkedName: c.LinkedAccountName, LinkedProvider: string(c.LinkedProvider),
		CreatedAt: c.CreatedAt, ObservedModel: c.ObservedModel, ObservedModelAt: c.ObservedModelAt,
		CloudflareCookieConfigured: c.EncryptedCloudflareCookie != "",
		BuildSuperEntitled:         c.BuildSuperEntitled && c.Provider == accountdomain.ProviderBuild,
		BuildRouteMode:             string(buildRouteMode),
		BuildBotFlagged:            value.BuildBotFlagged && c.Provider == accountdomain.ProviderBuild,
		BuildBotFlagSource:         buildBotFlagSourceResponse(c.Provider, value.BuildBotFlagged, value.BuildBotFlagSource),
		RiskStatus:                 c.RiskStatus,
		RiskTrigger:                c.RiskTrigger,
		RiskOriginAccountID:        c.RiskOriginAccountID,
		RiskCheckedAt:              c.RiskCheckedAt,
		RiskDetail:                 c.RiskDetail,
		Quota:                      newQuotaResponse(value.Quota), QuotaWindows: make([]quotaWindowResponse, 0, len(value.QuotaWindows)),
	}
	for _, linked := range c.LinkedAccounts {
		result.LinkedAccounts = append(result.LinkedAccounts, linkedAccountResponse{ID: linked.ID, Provider: string(linked.Provider), Name: linked.Name, Email: linked.Email, UserID: linked.UserID})
	}
	for _, window := range value.QuotaWindows {
		breakdown := make([]quotaBreakdownResponse, 0, len(window.Breakdown))
		for _, item := range window.Breakdown {
			breakdown = append(breakdown, quotaBreakdownResponse{ProductCode: item.ProductCode, UsagePercent: item.UsagePercent})
		}
		result.QuotaWindows = append(result.QuotaWindows, quotaWindowResponse{
			Mode: window.Mode, Remaining: window.Remaining, Total: window.Total,
			UsagePercent: window.UsagePercent, Breakdown: breakdown,
			WindowSeconds: window.WindowSeconds, ResetAt: window.ResetAt, SyncedAt: window.SyncedAt,
			Source: string(window.Source),
		})
	}
	if !c.ExpiresAt.IsZero() {
		expiresAt := c.ExpiresAt
		result.ExpiresAt = &expiresAt
	}
	if value.Billing != nil {
		billing := newBillingResponse(*value.Billing)
		result.Billing = &billing
	}
	return result
}

func buildBotFlagSourceResponse(provider accountdomain.Provider, flagged bool, source int) int {
	if provider != accountdomain.ProviderBuild || !flagged {
		return 0
	}
	if source != 1 && source != 2 {
		return 0
	}
	return source
}

func newQuotaResponse(value accountapp.QuotaView) quotaResponse {
	return quotaResponse{Type: string(value.Type), Source: value.Source, Confidence: value.Confidence, Unit: value.Unit, Used: value.Used, Limit: value.Limit, Remaining: value.Remaining, UsagePercent: value.UsagePercent, LimitKnown: value.LimitKnown, WindowHours: value.WindowHours, Observed: value.Observed, Confirmed: value.Confirmed, Status: string(value.Status), PeriodStart: value.PeriodStart, PeriodEnd: value.PeriodEnd, ExhaustedAt: value.ExhaustedAt, NextProbeAt: value.NextProbeAt, LastConfirmedAt: value.LastConfirmedAt}
}

func newBillingResponse(value accountdomain.Billing) billingResponse {
	history := make([]billingHistoryResponse, 0, len(value.History))
	for _, entry := range value.History {
		history = append(history, billingHistoryResponse{
			Year: entry.Year, Month: entry.Month,
			PeriodType: entry.PeriodType, PeriodStart: entry.PeriodStart, PeriodEnd: entry.PeriodEnd,
			IncludedUsed: entry.IncludedUsed, OnDemandUsed: entry.OnDemandUsed, TotalUsed: entry.TotalUsed,
		})
	}
	return billingResponse{PlanCode: value.PlanCode, PlanName: value.PlanName, MonthlyLimit: value.MonthlyLimit, Used: value.Used, Remaining: value.Remaining(), OnDemandCap: value.OnDemandCap, OnDemandUsed: value.OnDemandUsed, PrepaidBalance: value.PrepaidBalance, CreditUsagePercent: value.CreditUsagePercent, IsUnifiedBillingUser: value.IsUnifiedBillingUser, OnDemandEnabled: value.OnDemandEnabled, TopUpMethod: value.TopUpMethod, UsagePeriodType: value.UsagePeriodType, UsagePeriodStart: value.UsagePeriodStart, UsagePeriodEnd: value.UsagePeriodEnd, BillingPeriodStart: value.BillingPeriodStart, BillingPeriodEnd: value.BillingPeriodEnd, History: history, SyncedAt: value.SyncedAt}
}

// parseIDs 适配 httphelpers.ParseIDs 到账号面历史文案「ID 无效」：该面
// 从不透出具体非法值，保持客户端可见响应不变。
func parseIDs(values []string) ([]uint64, error) {
	ids, err := httphelpers.ParseIDs(values)
	if err != nil {
		return nil, errAccountIDInvalid
	}
	return ids, nil
}

var errAccountIDInvalid = errors.New("ID 无效")

func (h *Handler) validateProviderIDs(c *gin.Context, ids []uint64, providerValue string) bool {
	provider := accountdomain.Provider(providerValue)
	if !provider.IsValid() {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "账号来源无效")
		return false
	}
	valid, err := h.admin.AccountsBelongToProvider(c.Request.Context(), ids, provider)
	if err != nil {
		h.writeServiceError(c, "accountPoolValidationFailed", err, http.StatusInternalServerError, "校验账号号池失败")
		return false
	}
	if !valid {
		response.Error(c, http.StatusConflict, "accountPoolMismatch", "批量操作包含不属于当前号池的账号")
		return false
	}
	return true
}
