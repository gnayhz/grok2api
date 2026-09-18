package settings

import (
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

func toEditable(cfg settingsdomain.Config) EditableConfig {
	randomDelay := time.Duration(0)
	if cfg.Batch.RandomDelay != nil {
		randomDelay = *cfg.Batch.RandomDelay
	}
	accountIsolated := false
	if cfg.Routing.AccountIsolatedConnections != nil {
		accountIsolated = *cfg.Routing.AccountIsolatedConnections
	}
	segmented := settingsdomain.SegmentedSelectorConfig{}
	if cfg.Routing.SegmentedSelector != nil {
		segmented = *cfg.Routing.SegmentedSelector
	}
	retention := optionalDuration(cfg.Audit.RetentionPeriod)
	editable := EditableConfig{
		Server: ServerConfig{MaxConcurrentRequests: cfg.Server.MaxConcurrentRequests},
		ProviderBuild: ProviderBuildConfig{
			BaseURL: cfg.ProviderBuild.BaseURL, FallbackBaseURL: normalizeBuildFallbackBaseURL(cfg.ProviderBuild.FallbackBaseURL),
			ClientVersion: cfg.ProviderBuild.ClientVersion, ClientIdentifier: cfg.ProviderBuild.ClientIdentifier,
			TokenAuth: cfg.ProviderBuild.TokenAuth, UserAgent: cfg.ProviderBuild.UserAgent,
			SessionIdleConnTimeout: formatDuration(cfg.ProviderBuild.SessionIdleConnTimeout),
			ResponseHeaderTimeout:  formatDuration(cfg.ProviderBuild.ResponseHeaderTimeout),
			StreamIdleTimeout:      formatDuration(cfg.ProviderBuild.StreamIdleTimeout),
		},
		ProviderWeb: ProviderWebConfig{
			BaseURL: cfg.ProviderWeb.BaseURL, QuotaTimeout: formatDuration(cfg.ProviderWeb.QuotaTimeout),
			StatsigMode: cfg.ProviderWeb.StatsigMode, StatsigManualConfigured: cfg.ProviderWeb.StatsigManualValue != "",
			StatsigSignerURL: cfg.ProviderWeb.StatsigSignerURL,
			ClearanceMode:    cfg.ProviderWeb.ClearanceMode, FlareSolverrURL: cfg.ProviderWeb.FlareSolverrURL,
			ClearanceTimeout: formatDuration(cfg.ProviderWeb.ClearanceTimeout), ClearanceRefresh: formatDuration(cfg.ProviderWeb.ClearanceRefresh),
			ChatTimeout: formatDuration(cfg.ProviderWeb.ChatTimeout), StreamIdleTimeout: formatDuration(cfg.ProviderWeb.StreamIdleTimeout),
			ImageTimeout: formatDuration(cfg.ProviderWeb.ImageTimeout), VideoTimeout: formatDuration(cfg.ProviderWeb.VideoTimeout),
			MediaConcurrency: cfg.ProviderWeb.MediaConcurrency, AllowNSFW: cfg.ProviderWeb.AllowNSFW,
			RecoveryBackoffBase: formatDuration(cfg.ProviderWeb.RecoveryBackoffBase), RecoveryBackoffMax: formatDuration(cfg.ProviderWeb.RecoveryBackoffMax),
		},
		ProviderConsole: ProviderConsoleConfig{
			BaseURL: cfg.ProviderConsole.BaseURL, ChatTimeout: formatDuration(cfg.ProviderConsole.ChatTimeout),
			StreamIdleTimeout: formatDuration(cfg.ProviderConsole.StreamIdleTimeout),
		},
		Batch: BatchConfig{
			ImportConcurrency: cfg.Batch.ImportConcurrency, ConversionConcurrency: cfg.Batch.ConversionConcurrency,
			SyncConcurrency: cfg.Batch.SyncConcurrency, RefreshConcurrency: cfg.Batch.RefreshConcurrency,
			RandomDelay: formatDuration(randomDelay),
		},
		Media: MediaConfig{
			MaxImageBytes: cfg.Media.MaxImageBytes, MaxTotalBytes: cfg.Media.MaxTotalBytes,
			CleanupThresholdPercent: cfg.Media.CleanupThresholdPercent, CleanupInterval: formatDuration(cfg.Media.CleanupInterval),
		},
		Frontend: FrontendConfig{PublicAPIBaseURL: cfg.Frontend.PublicAPIBaseURL},
		Routing: RoutingConfig{
			StickyTTL: formatDuration(cfg.Routing.StickyTTL), CooldownBase: formatDuration(cfg.Routing.CooldownBase),
			CooldownMax: formatDuration(cfg.Routing.CooldownMax), CapacityWait: formatDuration(cfg.Routing.CapacityWait),
			MaxAttempts: cfg.Routing.MaxAttempts, VideoMaxAttempts: cfg.Routing.VideoMaxAttempts,
			MarkBuildChatDeniedAsReauth: cfg.Routing.MarkBuildChatDeniedAsReauth, MarkBuildChatDeniedAsReauthProvided: true,
			PreferFreeBuild:            cfg.Routing.PreferFreeBuild,
			AccountIsolatedConnections: accountIsolated, AccountIsolatedConnectionsProvided: true,
			SegmentedSelector: SegmentedSelectorConfig{
				Enabled: segmented.ActiveEnabled, MinCandidates: segmented.MinCandidates, WindowSize: segmented.WindowSize,
			},
			SegmentedSelectorProvided: true,
		},
		Audit: AuditConfig{
			BufferSize: cfg.Audit.BufferSize, BatchSize: cfg.Audit.BatchSize, FlushInterval: formatDuration(cfg.Audit.FlushInterval),
			CommitDelayMS:   int(cfg.Audit.CommitDelay / time.Millisecond),
			RetentionPeriod: formatDuration(retention), RetentionPeriodProvided: true,
			RetentionDays: int(retention / (24 * time.Hour)), RetentionDaysProvided: retention%(24*time.Hour) == 0,
			RetentionSource: cfg.Audit.RetentionSource,
		},
		ClientKeyDefaults: ClientKeyDefaultsConfig{RPMLimit: cfg.ClientKeyDefaults.RPMLimit, MaxConcurrent: cfg.ClientKeyDefaults.MaxConcurrent},
		Accounts: AccountsConfig{
			MarkBuildForbiddenReauth:             cfg.Accounts.MarkBuildForbiddenReauth,
			BuildForbiddenReauthCodes:            append([]string(nil), cfg.Accounts.BuildForbiddenReauthCodes...),
			ExcludeBuildBotFlaggedFromScheduling: cfg.Accounts.ExcludeBuildBotFlaggedFromScheduling,
			MarkBuildForbiddenReauthProvided:     true, BuildForbiddenReauthCodesProvided: true,
			ExcludeBuildBotFlaggedFromSchedulingProvided: true,
			AutoCleanReauthEnabled:                       cfg.Accounts.AutoCleanReauthEnabled,
			AutoCleanReauthInterval:                      formatDuration(cfg.Accounts.AutoCleanReauthInterval),
			AutoCleanReauthMinAge:                        formatDuration(cfg.Accounts.AutoCleanReauthMinAge),
			AutoCleanIncludeDisabled:                     cfg.Accounts.AutoCleanIncludeDisabled,
		},
		AccountsProvided: true,
	}
	if cfg.RequestRetry != nil {
		editable.RequestRetry = RequestRetryEditable{
			Enabled: cfg.RequestRetry.Enabled, MaxAttempts: cfg.RequestRetry.MaxAttempts, OnExhausted: cfg.RequestRetry.OnExhausted,
			AccountCooldown: formatDuration(cfg.RequestRetry.AccountCooldown), EvidenceTimeout: formatDuration(cfg.RequestRetry.EvidenceTimeout),
			CreatedTimeout: formatDuration(cfg.RequestRetry.CreatedTimeout), IdleAccountCooldown: formatDuration(cfg.RequestRetry.IdleAccountCooldown),
		}
		editable.RequestRetryProvided = true
	}
	if cfg.EgressRotation != nil {
		editable.EgressRotation = EgressRotationEditable{
			Enabled: cfg.EgressRotation.Enabled, MaxAttemptsPerQuarantine: cfg.EgressRotation.MaxAttemptsPerQuarantine,
			MinNodeInterval: formatDuration(cfg.EgressRotation.MinNodeInterval), MaxGlobalPerHour: cfg.EgressRotation.MaxGlobalPerHour,
			WebhookTimeout: formatDuration(cfg.EgressRotation.WebhookTimeout), WebhookRetries: cfg.EgressRotation.WebhookRetries,
			SettleDelay: formatDuration(cfg.EgressRotation.SettleDelay), ProbeTimeout: formatDuration(cfg.EgressRotation.ProbeTimeout),
			ProbeInterval: formatDuration(cfg.EgressRotation.ProbeInterval),
		}
		editable.EgressRotationProvided = true
	}
	return editable
}
