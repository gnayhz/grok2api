package settings

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *settingsapp.Service
}

func NewHandler(service *settingsapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/settings", h.get)
	router.PUT("/settings", h.update)
	router.DELETE("/settings", h.reset)
	router.POST("/settings/egress-rotation/reset", h.resetRotation)
}

type settingsConfigDTO struct {
	Server            serverConfigDTO            `json:"server"`
	ProviderBuild     providerBuildConfigDTO     `json:"providerBuild"`
	ProviderWeb       providerWebConfigDTO       `json:"providerWeb"`
	ProviderConsole   providerConsoleConfigDTO   `json:"providerConsole"`
	Batch             batchConfigDTO             `json:"batch"`
	Media             mediaConfigDTO             `json:"media"`
	Frontend          frontendConfigDTO          `json:"frontend"`
	Routing           routingConfigDTO           `json:"routing"`
	Audit             auditConfigDTO             `json:"audit"`
	ClientKeyDefaults clientKeyDefaultsConfigDTO `json:"clientKeyDefaults"`
	Accounts          *accountsConfigDTO         `json:"accounts,omitempty"`
	// RequestRetry/EgressRotation 为指针节：旧管理端未发送时
	// 保持 nil，服务端沿用当前值而非清零。
	RequestRetry   *requestRetryConfigDTO   `json:"requestRetry,omitempty"`
	EgressRotation *egressRotationConfigDTO `json:"egressRotation,omitempty"`
}

type requestRetryConfigDTO struct {
	Enabled             bool   `json:"enabled"`
	MaxAttempts         int    `json:"maxAttempts"`
	OnExhausted         string `json:"onExhausted"`
	AccountCooldown     string `json:"accountCooldown"`
	EvidenceTimeout     string `json:"evidenceTimeout"`
	CreatedTimeout      string `json:"createdTimeout"`
	IdleAccountCooldown string `json:"idleAccountCooldown"`
}

type egressRotationConfigDTO struct {
	Enabled                  bool   `json:"enabled"`
	MaxAttemptsPerQuarantine int    `json:"maxAttemptsPerQuarantine"`
	MinNodeInterval          string `json:"minNodeInterval"`
	MaxGlobalPerHour         int    `json:"maxGlobalPerHour"`
	WebhookTimeout           string `json:"webhookTimeout"`
	WebhookRetries           int    `json:"webhookRetries"`
	SettleDelay              string `json:"settleDelay"`
	ProbeTimeout             string `json:"probeTimeout"`
	ProbeInterval            string `json:"probeInterval"`
}

type serverConfigDTO struct {
	MaxConcurrentRequests int `json:"maxConcurrentRequests"`
}

type providerConsoleConfigDTO struct {
	BaseURL           string `json:"baseURL"`
	ChatTimeout       string `json:"chatTimeout"`
	StreamIdleTimeout string `json:"streamIdleTimeout"`
}

type mediaConfigDTO struct {
	MaxImageBytes           int64  `json:"maxImageBytes"`
	MaxTotalBytes           int64  `json:"maxTotalBytes"`
	CleanupThresholdPercent int    `json:"cleanupThresholdPercent"`
	CleanupInterval         string `json:"cleanupInterval"`
}

type frontendConfigDTO struct {
	PublicAPIBaseURL string `json:"publicApiBaseURL"`
}

type providerBuildConfigDTO struct {
	BaseURL                string `json:"baseURL"`
	FallbackBaseURL        string `json:"fallbackBaseURL"`
	ClientVersion          string `json:"clientVersion"`
	ClientIdentifier       string `json:"clientIdentifier"`
	TokenAuth              string `json:"tokenAuth"`
	TokenAuthConfigured    bool   `json:"tokenAuthConfigured"`
	UserAgent              string `json:"userAgent"`
	SessionIdleConnTimeout string `json:"sessionIdleConnTimeout"`
	ResponseHeaderTimeout  string `json:"responseHeaderTimeout"`
	StreamIdleTimeout      string `json:"streamIdleTimeout"`
}

type providerWebConfigDTO struct {
	BaseURL                 string  `json:"baseURL"`
	StatsigMode             string  `json:"statsigMode"`
	StatsigManualValue      string  `json:"statsigManualValue,omitempty"`
	StatsigManualConfigured bool    `json:"statsigManualConfigured"`
	StatsigSignerURL        string  `json:"statsigSignerURL"`
	ClearanceMode           *string `json:"clearanceMode,omitempty"`
	FlareSolverrURL         *string `json:"flareSolverrURL,omitempty"`
	ClearanceTimeout        *string `json:"clearanceTimeout,omitempty"`
	ClearanceRefresh        *string `json:"clearanceRefresh,omitempty"`
	QuotaTimeout            string  `json:"quotaTimeout"`
	ChatTimeout             string  `json:"chatTimeout"`
	StreamIdleTimeout       string  `json:"streamIdleTimeout"`
	ImageTimeout            string  `json:"imageTimeout"`
	VideoTimeout            string  `json:"videoTimeout"`
	MediaConcurrency        int     `json:"mediaConcurrency"`
	AllowNSFW               bool    `json:"allowNSFW"`
	RecoveryBackoffBase     string  `json:"recoveryBackoffBase"`
	RecoveryBackoffMax      string  `json:"recoveryBackoffMax"`
}

type batchConfigDTO struct {
	ImportConcurrency     int    `json:"importConcurrency"`
	ConversionConcurrency int    `json:"conversionConcurrency"`
	SyncConcurrency       int    `json:"syncConcurrency"`
	RefreshConcurrency    int    `json:"refreshConcurrency"`
	RandomDelay           string `json:"randomDelay"`
}

type routingConfigDTO struct {
	StickyTTL                   string                      `json:"stickyTTL"`
	CooldownBase                string                      `json:"cooldownBase"`
	CooldownMax                 string                      `json:"cooldownMax"`
	CapacityWait                string                      `json:"capacityWait"`
	MaxAttempts                 int                         `json:"maxAttempts"`
	VideoMaxAttempts            int                         `json:"videoMaxAttempts"`
	PreferFreeBuild             bool                        `json:"preferFreeBuild"`
	MarkBuildChatDeniedAsReauth *bool                       `json:"markBuildChatDeniedAsReauth,omitempty"`
	AccountIsolatedConnections  *bool                       `json:"accountIsolatedConnections,omitempty"`
	SegmentedSelector           *segmentedSelectorConfigDTO `json:"segmentedSelector,omitempty"`
}

type segmentedSelectorConfigDTO struct {
	Enabled       bool `json:"enabled"`
	MinCandidates int  `json:"minCandidates"`
	WindowSize    int  `json:"windowSize"`
}

type auditConfigDTO struct {
	BufferSize          int     `json:"bufferSize"`
	BatchSize           int     `json:"batchSize"`
	FlushInterval       string  `json:"flushInterval"`
	CommitDelayMS       int     `json:"commitDelayMS"`
	RetentionDays       *int    `json:"retentionDays,omitempty"`
	RetentionPeriod     *string `json:"retentionPeriod,omitempty"`
	RetentionSource     string  `json:"retentionSource,omitempty"`
	FileRetentionPeriod string  `json:"fileRetentionPeriod,omitempty"`
	FileRetentionSource string  `json:"fileRetentionSource,omitempty"`
}

type clientKeyDefaultsConfigDTO struct {
	RPMLimit      int `json:"rpmLimit"`
	MaxConcurrent int `json:"maxConcurrent"`
}

type accountsConfigDTO struct {
	MarkBuildForbiddenReauth             *bool     `json:"markBuildForbiddenReauth,omitempty"`
	BuildForbiddenReauthCodes            *[]string `json:"buildForbiddenReauthCodes,omitempty"`
	ExcludeBuildBotFlaggedFromScheduling *bool     `json:"excludeBuildBotFlaggedFromScheduling,omitempty"`
	AutoCleanReauthEnabled               bool      `json:"autoCleanReauthEnabled"`
	AutoCleanReauthInterval              string    `json:"autoCleanReauthInterval"`
	AutoCleanReauthMinAge                string    `json:"autoCleanReauthMinAge"`
	AutoCleanIncludeDisabled             bool      `json:"autoCleanIncludeDisabled"`
}

type settingsResponse struct {
	Config                   settingsConfigDTO              `json:"config"`
	RecommendedProviderBuild providerBuildRecommendationDTO `json:"recommendedProviderBuild"`
	UpdatedAt                time.Time                      `json:"updatedAt"`
	Revision                 uint64                         `json:"revision,string"`
	AppliedRevision          uint64                         `json:"appliedRevision,string"`
	ApplyPending             bool                           `json:"applyPending"`
	ApplyTargets             []applyStatusDTO               `json:"applyTargets"`
	Notification             notificationStatusDTO          `json:"notification"`
	RestartRequired          []string                       `json:"restartRequired"`
	// FileRequestRetry 文件配置基线的 requestRetry 节(覆盖标记/回同步数据源)。
	FileRequestRetry *requestRetryConfigDTO `json:"fileRequestRetry,omitempty"`
}

type applyStatusDTO struct {
	Name            string     `json:"name"`
	AppliedRevision uint64     `json:"appliedRevision,string"`
	Pending         bool       `json:"pending"`
	Error           string     `json:"error,omitempty"`
	LastAttemptAt   *time.Time `json:"lastAttemptAt,omitempty"`
}
type notificationStatusDTO struct {
	Revision      uint64     `json:"revision,string"`
	State         string     `json:"state"`
	Error         string     `json:"error,omitempty"`
	LastAttemptAt *time.Time `json:"lastAttemptAt,omitempty"`
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

type providerBuildRecommendationDTO struct {
	ClientVersion string `json:"clientVersion"`
	UserAgent     string `json:"userAgent"`
}

type updateRequest struct {
	Revision uint64            `json:"revision,string"`
	Config   settingsConfigDTO `json:"config" binding:"required"`
}

func (h *Handler) get(c *gin.Context) {
	snapshot, err := h.service.Read(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "settingsLoadFailed", "读取运行设置失败")
		return
	}
	response.Success(c, http.StatusOK, newSettingsResponse(snapshot))
}

func (h *Handler) update(c *gin.Context) {
	var request updateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+err.Error())
		return
	}
	result, err := h.service.Update(c.Request.Context(), request.Revision, request.Config.toApplication())
	if err != nil {
		if errors.Is(err, settingsapp.ErrInvalidInput) {
			response.Error(c, http.StatusBadRequest, "settingsUpdateFailed", err.Error())
			return
		}
		if errors.Is(err, settingsapp.ErrConflict) {
			response.Error(c, http.StatusConflict, "settingsConflict", "设置已被其他会话更新，请刷新后重试")
			return
		}
		response.Error(c, http.StatusInternalServerError, "settingsUpdateFailed", "保存运行设置失败")
		return
	}
	status := http.StatusOK
	if result.ApplyPending {
		status = http.StatusAccepted
	}
	response.Success(c, status, newSettingsResponse(result))
}

// reset records file-default mode with the same durable CAS contract as save.
// Legacy empty DELETE bodies use the service's observed revision. New clients
// send their snapshot revision for protection against stale browser state too.
func (h *Handler) reset(c *gin.Context) {
	var request struct {
		Revision *uint64 `json:"revision,string"`
	}
	err := c.ShouldBindJSON(&request)
	if err != nil && !errors.Is(err, io.EOF) || err == nil && request.Revision == nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: revision 必须为版本字符串")
		return
	}
	expectedRevision := h.service.Get().Revision
	if request.Revision != nil {
		expectedRevision = *request.Revision
	}
	result, err := h.service.ResetToDefaults(c.Request.Context(), expectedRevision)
	if errors.Is(err, settingsapp.ErrConflict) {
		response.Error(c, http.StatusConflict, "settingsConflict", "设置已被其他会话更新，请刷新后重试")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "settingsResetFailed", "恢复文件默认设置失败")
		return
	}
	status := http.StatusOK
	if result.ApplyPending {
		status = http.StatusAccepted
	}
	response.Success(c, status, newSettingsResponse(result))
}

func (h *Handler) resetRotation(c *gin.Context) {
	var request struct {
		Revision *uint64 `json:"revision,string"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Revision == nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "revision 必须为版本字符串")
		return
	}
	result, err := h.service.ResetEgressRotation(c.Request.Context(), *request.Revision)
	if errors.Is(err, settingsapp.ErrConflict) {
		response.Error(c, http.StatusConflict, "settingsConflict", "设置已被其他会话更新，请刷新后重试")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "settingsResetFailed", "恢复出口轮换默认设置失败")
		return
	}
	status := http.StatusOK
	if result.ApplyPending {
		status = http.StatusAccepted
	}
	response.Success(c, status, newSettingsResponse(result))
}

func (value settingsConfigDTO) toApplication() settingsapp.EditableConfig {
	clearanceProvided := value.ProviderWeb.ClearanceMode != nil || value.ProviderWeb.FlareSolverrURL != nil ||
		value.ProviderWeb.ClearanceTimeout != nil || value.ProviderWeb.ClearanceRefresh != nil
	result := settingsapp.EditableConfig{
		Server: settingsapp.ServerConfig{MaxConcurrentRequests: value.Server.MaxConcurrentRequests},
		ProviderBuild: settingsapp.ProviderBuildConfig{
			BaseURL: value.ProviderBuild.BaseURL, FallbackBaseURL: value.ProviderBuild.FallbackBaseURL,
			ClientVersion: value.ProviderBuild.ClientVersion, ClientIdentifier: value.ProviderBuild.ClientIdentifier,
			TokenAuth: value.ProviderBuild.TokenAuth, UserAgent: value.ProviderBuild.UserAgent,
			SessionIdleConnTimeout: value.ProviderBuild.SessionIdleConnTimeout,
			ResponseHeaderTimeout:  value.ProviderBuild.ResponseHeaderTimeout,
			StreamIdleTimeout:      value.ProviderBuild.StreamIdleTimeout,
		},
		ProviderWeb: settingsapp.ProviderWebConfig{
			BaseURL: value.ProviderWeb.BaseURL, QuotaTimeout: value.ProviderWeb.QuotaTimeout,
			StatsigMode: value.ProviderWeb.StatsigMode, StatsigManualValue: value.ProviderWeb.StatsigManualValue,
			StatsigManualConfigured: value.ProviderWeb.StatsigManualConfigured, StatsigSignerURL: value.ProviderWeb.StatsigSignerURL,
			ClearanceMode: optionalString(value.ProviderWeb.ClearanceMode), FlareSolverrURL: optionalString(value.ProviderWeb.FlareSolverrURL),
			ClearanceTimeout: optionalString(value.ProviderWeb.ClearanceTimeout), ClearanceRefresh: optionalString(value.ProviderWeb.ClearanceRefresh),
			ClearanceProvided: clearanceProvided,
			ChatTimeout:       value.ProviderWeb.ChatTimeout, StreamIdleTimeout: value.ProviderWeb.StreamIdleTimeout,
			ImageTimeout:     value.ProviderWeb.ImageTimeout,
			VideoTimeout:     value.ProviderWeb.VideoTimeout,
			MediaConcurrency: value.ProviderWeb.MediaConcurrency, AllowNSFW: value.ProviderWeb.AllowNSFW,
			RecoveryBackoffBase: value.ProviderWeb.RecoveryBackoffBase, RecoveryBackoffMax: value.ProviderWeb.RecoveryBackoffMax,
		},
		ProviderConsole: settingsapp.ProviderConsoleConfig{
			BaseURL: value.ProviderConsole.BaseURL, ChatTimeout: value.ProviderConsole.ChatTimeout,
			StreamIdleTimeout: value.ProviderConsole.StreamIdleTimeout,
		},
		Batch: settingsapp.BatchConfig{
			ImportConcurrency: value.Batch.ImportConcurrency, ConversionConcurrency: value.Batch.ConversionConcurrency,
			SyncConcurrency: value.Batch.SyncConcurrency, RefreshConcurrency: value.Batch.RefreshConcurrency,
			RandomDelay: value.Batch.RandomDelay,
		},
		Media: settingsapp.MediaConfig{
			MaxImageBytes: value.Media.MaxImageBytes, MaxTotalBytes: value.Media.MaxTotalBytes,
			CleanupThresholdPercent: value.Media.CleanupThresholdPercent, CleanupInterval: value.Media.CleanupInterval,
		},
		Frontend: settingsapp.FrontendConfig{
			PublicAPIBaseURL: value.Frontend.PublicAPIBaseURL,
		},
		Routing: settingsapp.RoutingConfig{
			StickyTTL: value.Routing.StickyTTL, CooldownBase: value.Routing.CooldownBase,
			CooldownMax: value.Routing.CooldownMax, CapacityWait: value.Routing.CapacityWait, MaxAttempts: value.Routing.MaxAttempts, VideoMaxAttempts: value.Routing.VideoMaxAttempts,
			PreferFreeBuild:                     value.Routing.PreferFreeBuild,
			MarkBuildChatDeniedAsReauth:         boolValue(value.Routing.MarkBuildChatDeniedAsReauth),
			MarkBuildChatDeniedAsReauthProvided: value.Routing.MarkBuildChatDeniedAsReauth != nil,
			AccountIsolatedConnections:          boolValue(value.Routing.AccountIsolatedConnections),
			AccountIsolatedConnectionsProvided:  value.Routing.AccountIsolatedConnections != nil,
		},
		Audit: settingsapp.AuditConfig{
			BufferSize: value.Audit.BufferSize, BatchSize: value.Audit.BatchSize, FlushInterval: value.Audit.FlushInterval, CommitDelayMS: value.Audit.CommitDelayMS,
			RetentionDays: intValue(value.Audit.RetentionDays), RetentionDaysProvided: value.Audit.RetentionDays != nil,
			RetentionPeriod: optionalString(value.Audit.RetentionPeriod), RetentionPeriodProvided: value.Audit.RetentionPeriod != nil,
		},
		ClientKeyDefaults: settingsapp.ClientKeyDefaultsConfig{
			RPMLimit: value.ClientKeyDefaults.RPMLimit, MaxConcurrent: value.ClientKeyDefaults.MaxConcurrent,
		},
	}
	if value.Routing.SegmentedSelector != nil {
		result.Routing.SegmentedSelector = settingsapp.SegmentedSelectorConfig{
			Enabled: value.Routing.SegmentedSelector.Enabled, MinCandidates: value.Routing.SegmentedSelector.MinCandidates,
			WindowSize: value.Routing.SegmentedSelector.WindowSize,
		}
		result.Routing.SegmentedSelectorProvided = true
	}
	if value.Accounts != nil {
		result.Accounts = settingsapp.AccountsConfig{
			MarkBuildForbiddenReauth:                     boolValue(value.Accounts.MarkBuildForbiddenReauth),
			BuildForbiddenReauthCodes:                    stringSliceValue(value.Accounts.BuildForbiddenReauthCodes),
			ExcludeBuildBotFlaggedFromScheduling:         boolValue(value.Accounts.ExcludeBuildBotFlaggedFromScheduling),
			MarkBuildForbiddenReauthProvided:             value.Accounts.MarkBuildForbiddenReauth != nil,
			BuildForbiddenReauthCodesProvided:            value.Accounts.BuildForbiddenReauthCodes != nil,
			ExcludeBuildBotFlaggedFromSchedulingProvided: value.Accounts.ExcludeBuildBotFlaggedFromScheduling != nil,
			AutoCleanReauthEnabled:                       value.Accounts.AutoCleanReauthEnabled,
			AutoCleanReauthInterval:                      value.Accounts.AutoCleanReauthInterval,
			AutoCleanReauthMinAge:                        value.Accounts.AutoCleanReauthMinAge,
			AutoCleanIncludeDisabled:                     value.Accounts.AutoCleanIncludeDisabled,
		}
		result.AccountsProvided = true
	}
	if value.RequestRetry != nil {
		result.RequestRetry = settingsapp.RequestRetryEditable{
			Enabled: value.RequestRetry.Enabled, MaxAttempts: value.RequestRetry.MaxAttempts,
			OnExhausted: value.RequestRetry.OnExhausted, AccountCooldown: value.RequestRetry.AccountCooldown,
			EvidenceTimeout: value.RequestRetry.EvidenceTimeout, CreatedTimeout: value.RequestRetry.CreatedTimeout,
			IdleAccountCooldown: value.RequestRetry.IdleAccountCooldown,
		}
		result.RequestRetryProvided = true
	}
	if value.EgressRotation != nil {
		result.EgressRotation = settingsapp.EgressRotationEditable{
			Enabled: value.EgressRotation.Enabled, MaxAttemptsPerQuarantine: value.EgressRotation.MaxAttemptsPerQuarantine,
			MinNodeInterval: value.EgressRotation.MinNodeInterval, MaxGlobalPerHour: value.EgressRotation.MaxGlobalPerHour,
			WebhookTimeout: value.EgressRotation.WebhookTimeout, WebhookRetries: value.EgressRotation.WebhookRetries,
			SettleDelay: value.EgressRotation.SettleDelay, ProbeTimeout: value.EgressRotation.ProbeTimeout,
			ProbeInterval: value.EgressRotation.ProbeInterval,
		}
		result.EgressRotationProvided = true
	}
	return result
}

func newSettingsResponse(value settingsapp.Snapshot) settingsResponse {
	targets := make([]applyStatusDTO, len(value.ApplyTargets))
	for i, state := range value.ApplyTargets {
		targets[i] = applyStatusDTO{Name: state.Name, AppliedRevision: state.AppliedRevision, Pending: state.Pending, Error: state.Error, LastAttemptAt: optionalTime(state.LastAttemptAt)}
	}
	config := value.Config
	return settingsResponse{
		Config: settingsConfigDTO{
			Server: serverConfigDTO{MaxConcurrentRequests: config.Server.MaxConcurrentRequests},
			ProviderBuild: providerBuildConfigDTO{
				BaseURL: config.ProviderBuild.BaseURL, FallbackBaseURL: config.ProviderBuild.FallbackBaseURL,
				ClientVersion: config.ProviderBuild.ClientVersion, ClientIdentifier: config.ProviderBuild.ClientIdentifier,
				TokenAuth:           config.ProviderBuild.TokenAuth,
				TokenAuthConfigured: strings.TrimSpace(config.ProviderBuild.TokenAuth) != "", UserAgent: config.ProviderBuild.UserAgent,
				SessionIdleConnTimeout: config.ProviderBuild.SessionIdleConnTimeout,
				ResponseHeaderTimeout:  config.ProviderBuild.ResponseHeaderTimeout,
				StreamIdleTimeout:      config.ProviderBuild.StreamIdleTimeout,
			},
			ProviderWeb: providerWebConfigDTO{
				BaseURL: config.ProviderWeb.BaseURL, QuotaTimeout: config.ProviderWeb.QuotaTimeout,
				StatsigMode: config.ProviderWeb.StatsigMode, StatsigManualConfigured: config.ProviderWeb.StatsigManualConfigured,
				StatsigSignerURL: config.ProviderWeb.StatsigSignerURL,
				ClearanceMode:    stringPointer(config.ProviderWeb.ClearanceMode), FlareSolverrURL: stringPointer(config.ProviderWeb.FlareSolverrURL),
				ClearanceTimeout: stringPointer(config.ProviderWeb.ClearanceTimeout), ClearanceRefresh: stringPointer(config.ProviderWeb.ClearanceRefresh),
				ChatTimeout: config.ProviderWeb.ChatTimeout, StreamIdleTimeout: config.ProviderWeb.StreamIdleTimeout,
				ImageTimeout:     config.ProviderWeb.ImageTimeout,
				VideoTimeout:     config.ProviderWeb.VideoTimeout,
				MediaConcurrency: config.ProviderWeb.MediaConcurrency, AllowNSFW: config.ProviderWeb.AllowNSFW,
				RecoveryBackoffBase: config.ProviderWeb.RecoveryBackoffBase, RecoveryBackoffMax: config.ProviderWeb.RecoveryBackoffMax,
			},
			ProviderConsole: providerConsoleConfigDTO{
				BaseURL: config.ProviderConsole.BaseURL, ChatTimeout: config.ProviderConsole.ChatTimeout,
				StreamIdleTimeout: config.ProviderConsole.StreamIdleTimeout,
			},
			Batch: batchConfigDTO{
				ImportConcurrency: config.Batch.ImportConcurrency, ConversionConcurrency: config.Batch.ConversionConcurrency,
				SyncConcurrency: config.Batch.SyncConcurrency, RefreshConcurrency: config.Batch.RefreshConcurrency,
				RandomDelay: config.Batch.RandomDelay,
			},
			Media: mediaConfigDTO{
				MaxImageBytes: config.Media.MaxImageBytes, MaxTotalBytes: config.Media.MaxTotalBytes,
				CleanupThresholdPercent: config.Media.CleanupThresholdPercent, CleanupInterval: config.Media.CleanupInterval,
			},
			Frontend: frontendConfigDTO{
				PublicAPIBaseURL: config.Frontend.PublicAPIBaseURL,
			},
			Routing: routingConfigDTO{
				StickyTTL: config.Routing.StickyTTL, CooldownBase: config.Routing.CooldownBase,
				CooldownMax: config.Routing.CooldownMax, CapacityWait: config.Routing.CapacityWait, MaxAttempts: config.Routing.MaxAttempts, VideoMaxAttempts: config.Routing.VideoMaxAttempts,
				MarkBuildChatDeniedAsReauth: boolPointer(config.Routing.MarkBuildChatDeniedAsReauth),
				PreferFreeBuild:             config.Routing.PreferFreeBuild,
				AccountIsolatedConnections:  boolPointer(config.Routing.AccountIsolatedConnections),
				SegmentedSelector: &segmentedSelectorConfigDTO{
					Enabled: config.Routing.SegmentedSelector.Enabled, MinCandidates: config.Routing.SegmentedSelector.MinCandidates,
					WindowSize: config.Routing.SegmentedSelector.WindowSize,
				},
			},
			Audit: auditConfigDTO{
				BufferSize: config.Audit.BufferSize, BatchSize: config.Audit.BatchSize, FlushInterval: config.Audit.FlushInterval, CommitDelayMS: config.Audit.CommitDelayMS,
				RetentionDays:   legacyRetentionDays(config.Audit),
				RetentionPeriod: stringPointer(config.Audit.RetentionPeriod), RetentionSource: config.Audit.RetentionSource,
				FileRetentionPeriod: config.Audit.FileRetentionPeriod, FileRetentionSource: config.Audit.FileRetentionSource,
			},
			ClientKeyDefaults: clientKeyDefaultsConfigDTO{
				RPMLimit: config.ClientKeyDefaults.RPMLimit, MaxConcurrent: config.ClientKeyDefaults.MaxConcurrent,
			},
			Accounts: &accountsConfigDTO{
				MarkBuildForbiddenReauth:             boolPointer(config.Accounts.MarkBuildForbiddenReauth),
				BuildForbiddenReauthCodes:            stringSlicePointer(config.Accounts.BuildForbiddenReauthCodes),
				ExcludeBuildBotFlaggedFromScheduling: boolPointer(config.Accounts.ExcludeBuildBotFlaggedFromScheduling),
				AutoCleanReauthEnabled:               config.Accounts.AutoCleanReauthEnabled,
				AutoCleanReauthInterval:              config.Accounts.AutoCleanReauthInterval,
				AutoCleanReauthMinAge:                config.Accounts.AutoCleanReauthMinAge,
				AutoCleanIncludeDisabled:             config.Accounts.AutoCleanIncludeDisabled,
			},
			RequestRetry: requestRetryDTOFrom(config.RequestRetry),
			EgressRotation: &egressRotationConfigDTO{
				Enabled: config.EgressRotation.Enabled, MaxAttemptsPerQuarantine: config.EgressRotation.MaxAttemptsPerQuarantine,
				MinNodeInterval: config.EgressRotation.MinNodeInterval, MaxGlobalPerHour: config.EgressRotation.MaxGlobalPerHour,
				WebhookTimeout: config.EgressRotation.WebhookTimeout, WebhookRetries: config.EgressRotation.WebhookRetries,
				SettleDelay: config.EgressRotation.SettleDelay, ProbeTimeout: config.EgressRotation.ProbeTimeout,
				ProbeInterval: config.EgressRotation.ProbeInterval,
			},
		},
		RecommendedProviderBuild: providerBuildRecommendationDTO{
			ClientVersion: value.RecommendedProviderBuild.ClientVersion,
			UserAgent:     value.RecommendedProviderBuild.UserAgent,
		},
		UpdatedAt: value.UpdatedAt, Revision: value.Revision, RestartRequired: value.RestartRequired,
		AppliedRevision: value.AppliedRevision, ApplyPending: value.ApplyPending, ApplyTargets: targets,
		Notification:     notificationStatusDTO{Revision: value.Notification.Revision, State: value.Notification.State, Error: value.Notification.Error, LastAttemptAt: optionalTime(value.Notification.LastAttemptAt)},
		FileRequestRetry: requestRetryDTOFrom(value.FileRequestRetry),
	}
}

func requestRetryDTOFrom(value settingsapp.RequestRetryEditable) *requestRetryConfigDTO {
	return &requestRetryConfigDTO{
		Enabled: value.Enabled, MaxAttempts: value.MaxAttempts,
		OnExhausted: value.OnExhausted, AccountCooldown: value.AccountCooldown,
		EvidenceTimeout: value.EvidenceTimeout, CreatedTimeout: value.CreatedTimeout,
		IdleAccountCooldown: value.IdleAccountCooldown,
	}
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func stringPointer(value string) *string { return &value }

func boolPointer(value bool) *bool { return &value }

func boolValue(value *bool) bool {
	if value == nil {
		return false
	}
	return *value
}

func intPointer(value int) *int { return &value }

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func stringSliceValue(value *[]string) []string {
	if value == nil {
		return nil
	}
	return append([]string(nil), (*value)...)
}

func stringSlicePointer(value []string) *[]string {
	cloned := append([]string(nil), value...)
	return &cloned
}

func legacyRetentionDays(value settingsapp.AuditConfig) *int {
	if !value.RetentionDaysProvided {
		return nil
	}
	return intPointer(value.RetentionDays)
}
