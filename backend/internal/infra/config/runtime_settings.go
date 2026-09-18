package config

import (
	"strings"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

// persistedAuditRetention is shared by startup, reload and each cleanup batch.
// Explicit persisted zero overrides the file, including legacy duration values.
func persistedAuditRetention(base AuditConfig, value settingsdomain.AuditConfig) (AuditConfig, error) {
	if value.RetentionPeriod != nil {
		base.RetentionPeriod = Duration(*value.RetentionPeriod)
		base.RetentionSource = "runtime"
	} else if value.RetentionDays != nil {
		period, err := settingsdomain.LegacyRetentionPeriod(*value.RetentionDays)
		if err != nil {
			return base, err
		}
		base.RetentionPeriod = Duration(period)
		base.RetentionSource = "legacy_runtime"
	}
	return base, (auditdomain.RetentionPolicy{Period: base.RetentionPeriod.Value()}).Validate()
}

// ApplyRuntimeSettings resolves a durable overlay, including omissions from old versions.
func ApplyRuntimeSettings(base Config, value settingsdomain.Config) (Config, error) {
	return applyRuntimeSettings(base, value, true)
}

// ApplyRuntimeSnapshot maps a complete editable snapshot without repairing
// explicit invalid or empty values using defaults for old persisted records.
func ApplyRuntimeSnapshot(base Config, value settingsdomain.Config) (Config, error) {
	next, err := applyRuntimeSettings(base, value, false)
	if err != nil {
		return Config{}, err
	}
	if err := next.Validate(); err != nil {
		return Config{}, err
	}
	return next, nil
}

func ResolveRuntimeSettings(base Config, value settingsdomain.Config) (settingsdomain.Config, error) {
	next, err := ApplyRuntimeSettings(base, value)
	if err != nil {
		return settingsdomain.Config{}, err
	}
	if err := next.Validate(); err != nil {
		return settingsdomain.Config{}, err
	}
	return ToRuntimeSettings(next), nil
}

func applyRuntimeSettings(base Config, value settingsdomain.Config, legacy bool) (Config, error) {
	resolvedAudit, err := persistedAuditRetention(base.Audit, value.Audit)
	if err != nil {
		return Config{}, err
	}
	// 旧版运行设置没有 Server 字段，反序列化后为零；升级时沿用当前配置默认值。
	if !legacy || value.Server.MaxConcurrentRequests > 0 {
		base.Server.MaxConcurrentRequests = value.Server.MaxConcurrentRequests
	}
	capacityWait := value.Routing.CapacityWait
	if legacy && capacityWait <= 0 {
		capacityWait = base.Routing.CapacityWait.Value()
	}
	base.Provider.Build = BuildProviderConfig{
		BaseURL: value.ProviderBuild.BaseURL, FallbackBaseURL: settingsdomain.NormalizeBuildFallbackBaseURL(value.ProviderBuild.FallbackBaseURL),
		ClientVersion: value.ProviderBuild.ClientVersion, ClientIdentifier: value.ProviderBuild.ClientIdentifier,
		TokenAuth: value.ProviderBuild.TokenAuth, UserAgent: value.ProviderBuild.UserAgent,
		SessionIdleConnTimeout: Duration(value.ProviderBuild.SessionIdleConnTimeout),
		ResponseHeaderTimeout:  Duration(value.ProviderBuild.ResponseHeaderTimeout),
		StreamIdleTimeout:      Duration(value.ProviderBuild.StreamIdleTimeout),
	}
	if legacy && value.ProviderBuild.SessionIdleConnTimeout == 0 {
		base.Provider.Build.SessionIdleConnTimeout = Duration(settingsdomain.DefaultBuildSessionIdleConnTimeout)
	}
	if legacy && value.ProviderBuild.ResponseHeaderTimeout <= 0 {
		base.Provider.Build.ResponseHeaderTimeout = Duration(settingsdomain.DefaultBuildResponseHeaderTimeout)
	}
	if legacy && value.ProviderBuild.StreamIdleTimeout <= 0 {
		base.Provider.Build.StreamIdleTimeout = Duration(settingsdomain.DefaultBuildStreamIdleTimeout)
	}
	clearanceMode := strings.TrimSpace(value.ProviderWeb.ClearanceMode)
	if legacy && clearanceMode == "" {
		clearanceMode = base.Provider.Web.ClearanceMode
	}
	flareSolverrURL := strings.TrimSpace(value.ProviderWeb.FlareSolverrURL)
	if legacy && flareSolverrURL == "" {
		flareSolverrURL = base.Provider.Web.FlareSolverrURL
	}
	clearanceTimeout := value.ProviderWeb.ClearanceTimeout
	if legacy && clearanceTimeout <= 0 {
		clearanceTimeout = base.Provider.Web.ClearanceTimeout.Value()
	}
	clearanceRefresh := value.ProviderWeb.ClearanceRefresh
	if legacy && clearanceRefresh <= 0 {
		clearanceRefresh = base.Provider.Web.ClearanceRefresh.Value()
	}
	base.Provider.Web = WebProviderConfig{
		BaseURL: value.ProviderWeb.BaseURL, QuotaTimeout: Duration(value.ProviderWeb.QuotaTimeout),
		StatsigMode: value.ProviderWeb.StatsigMode, StatsigManualValue: value.ProviderWeb.StatsigManualValue, StatsigSignerURL: value.ProviderWeb.StatsigSignerURL,
		ClearanceMode: clearanceMode, FlareSolverrURL: flareSolverrURL,
		ClearanceTimeout: Duration(clearanceTimeout), ClearanceRefresh: Duration(clearanceRefresh),
		ChatTimeout: Duration(value.ProviderWeb.ChatTimeout), StreamIdleTimeout: Duration(value.ProviderWeb.StreamIdleTimeout),
		ImageTimeout:     Duration(value.ProviderWeb.ImageTimeout),
		VideoTimeout:     Duration(value.ProviderWeb.VideoTimeout),
		MediaConcurrency: value.ProviderWeb.MediaConcurrency, AllowNSFW: value.ProviderWeb.AllowNSFW,
		RecoveryBackoffBase: Duration(value.ProviderWeb.RecoveryBackoffBase), RecoveryBackoffMax: Duration(value.ProviderWeb.RecoveryBackoffMax),
	}
	if legacy && value.ProviderWeb.StreamIdleTimeout <= 0 {
		base.Provider.Web.StreamIdleTimeout = Duration(settingsdomain.DefaultWebStreamIdleTimeout)
	}
	// Console 是后续版本新增的完整配置段；旧 JSON 整段缺失时沿用代码默认值。
	if !legacy || value.ProviderConsole != (settingsdomain.ProviderConsoleConfig{}) {
		base.Provider.Console = ConsoleProviderConfig{
			BaseURL: value.ProviderConsole.BaseURL, ChatTimeout: Duration(value.ProviderConsole.ChatTimeout),
			StreamIdleTimeout: Duration(value.ProviderConsole.StreamIdleTimeout),
		}
		if legacy && value.ProviderConsole.StreamIdleTimeout <= 0 {
			base.Provider.Console.StreamIdleTimeout = Duration(settingsdomain.DefaultConsoleStreamIdleTimeout)
		}
	}
	randomDelay := time.Duration(-1)
	if value.Batch.RandomDelay != nil {
		randomDelay = *value.Batch.RandomDelay
	}
	base.Batch = BatchConfig{
		ImportConcurrency: value.Batch.ImportConcurrency, ConversionConcurrency: value.Batch.ConversionConcurrency,
		SyncConcurrency: value.Batch.SyncConcurrency, RefreshConcurrency: value.Batch.RefreshConcurrency,
		RandomDelay: Duration(randomDelay),
	}
	base.Media.MaxImageBytes = value.Media.MaxImageBytes
	base.Media.MaxTotalBytes = value.Media.MaxTotalBytes
	base.Media.CleanupThresholdPercent = value.Media.CleanupThresholdPercent
	base.Media.CleanupInterval = Duration(value.Media.CleanupInterval)
	base.Frontend.PublicAPIBaseURLOverride = strings.TrimSpace(value.Frontend.PublicAPIBaseURL)
	segmentedEnabled := base.Routing.SegmentedSelectorEnabled
	segmentedMinCandidates := base.Routing.SegmentedMinCandidates
	segmentedWindowSize := base.Routing.SegmentedWindowSize
	accountIsolatedConnections := base.Routing.AccountIsolatedConnections
	if value.Routing.AccountIsolatedConnections != nil {
		accountIsolatedConnections = *value.Routing.AccountIsolatedConnections
	}
	if value.Routing.SegmentedSelector != nil {
		segmentedEnabled = value.Routing.SegmentedSelector.ActiveEnabled
		segmentedMinCandidates = value.Routing.SegmentedSelector.MinCandidates
		segmentedWindowSize = value.Routing.SegmentedSelector.WindowSize
	}
	base.Routing = RoutingConfig{
		StickyTTL: Duration(value.Routing.StickyTTL), CooldownBase: Duration(value.Routing.CooldownBase),
		CooldownMax: Duration(value.Routing.CooldownMax), CapacityWait: Duration(capacityWait), MaxAttempts: value.Routing.MaxAttempts, VideoMaxAttempts: value.Routing.VideoMaxAttempts,
		MarkBuildChatDeniedAsReauth: value.Routing.MarkBuildChatDeniedAsReauth,
		PreferFreeBuild:             value.Routing.PreferFreeBuild,
		AccountIsolatedConnections:  accountIsolatedConnections,
		SegmentedSelectorEnabled:    segmentedEnabled,
		SegmentedMinCandidates:      segmentedMinCandidates,
		SegmentedWindowSize:         segmentedWindowSize,
		ReasoningReplayEnabled:      base.Routing.ReasoningReplayEnabled, ReasoningReplayTTL: base.Routing.ReasoningReplayTTL,
		ConversationHistoryRetention: base.Routing.ConversationHistoryRetention, ConversationHistoryMaxBytes: base.Routing.ConversationHistoryMaxBytes,
		ReasoningReplayMaxEntries: base.Routing.ReasoningReplayMaxEntries,
	}
	commitDelay := base.Audit.CommitDelay.Value()
	if value.Audit.CommitDelay > 0 {
		commitDelay = value.Audit.CommitDelay
	}

	base.Audit = AuditConfig{
		JournalDirectory: base.Audit.JournalDirectory, JournalMaxBytes: base.Audit.JournalMaxBytes,
		BufferSize: value.Audit.BufferSize, BatchSize: value.Audit.BatchSize, FlushInterval: Duration(value.Audit.FlushInterval),
		CommitDelay: Duration(commitDelay), RetentionPeriod: resolvedAudit.RetentionPeriod, RetentionSource: resolvedAudit.RetentionSource,
		LedgerMode: base.Audit.LedgerMode, LedgerFailureThreshold: base.Audit.LedgerFailureThreshold,
		LedgerUnhealthyGrace: base.Audit.LedgerUnhealthyGrace, LedgerQueueHighWatermarkPct: base.Audit.LedgerQueueHighWatermarkPct,
	}
	base.ClientKeyDefaults = ClientKeyDefaultsConfig{
		RPMLimit: value.ClientKeyDefaults.RPMLimit, MaxConcurrent: value.ClientKeyDefaults.MaxConcurrent,
	}
	// Accounts 为后续新增段；旧持久化缺字段时沿用代码默认（全部关闭）。
	if !legacy || value.Accounts.AutoCleanReauthInterval > 0 {
		base.Accounts.AutoCleanReauthInterval = Duration(value.Accounts.AutoCleanReauthInterval)
	}
	if !legacy || value.Accounts.AutoCleanReauthMinAge > 0 {
		base.Accounts.AutoCleanReauthMinAge = Duration(value.Accounts.AutoCleanReauthMinAge)
	}
	base.Accounts.AutoCleanReauthEnabled = value.Accounts.AutoCleanReauthEnabled
	base.Accounts.AutoCleanIncludeDisabled = value.Accounts.AutoCleanIncludeDisabled
	base.Accounts.MarkBuildForbiddenReauth = value.Accounts.MarkBuildForbiddenReauth
	if value.Accounts.BuildForbiddenReauthCodes != nil {
		base.Accounts.BuildForbiddenReauthCodes = append([]string(nil), value.Accounts.BuildForbiddenReauthCodes...)
	}
	base.Accounts.ExcludeBuildBotFlaggedFromScheduling = value.Accounts.ExcludeBuildBotFlaggedFromScheduling
	// RequestRetry/EgressRotation:指针节,nil(旧载荷/未保存过)沿用文件基线。
	// AccountRisk 节已随旧风险归因链删除(切换手册第2步)。
	if value.RequestRetry != nil {
		// guardedModels 是 yaml 级白名单，不在管理端 DTO。整节赋值不得用
		// 缺省/空切片/陈旧持久化名单覆盖文件基线（空使用领域默认清单）。
		fileGuarded := append([]string(nil), base.RequestRetry.GuardedModels...)
		fileAdmission, fileToolAdmission := base.RequestRetry.AdmissionTimeout, base.RequestRetry.ToolAdmissionTimeout
		base.RequestRetry = RequestRetryConfig{
			Enabled: value.RequestRetry.Enabled, MaxAttempts: value.RequestRetry.MaxAttempts,
			AdmissionTimeout: fileAdmission, ToolAdmissionTimeout: fileToolAdmission,
			OnExhausted: value.RequestRetry.OnExhausted, AccountCooldown: Duration(value.RequestRetry.AccountCooldown),
			EvidenceTimeout: Duration(value.RequestRetry.EvidenceTimeout), CreatedTimeout: Duration(value.RequestRetry.CreatedTimeout),
			IdleAccountCooldown: Duration(value.RequestRetry.IdleAccountCooldown),
			GuardedModels:       fileGuarded,
		}
	}
	if value.EgressRotation != nil {
		base.Egress.Rotation = EgressRotationConfig{
			Enabled: value.EgressRotation.Enabled, MaxAttemptsPerQuarantine: value.EgressRotation.MaxAttemptsPerQuarantine,
			MinNodeInterval: Duration(value.EgressRotation.MinNodeInterval), MaxGlobalPerHour: value.EgressRotation.MaxGlobalPerHour,
			WebhookTimeout: Duration(value.EgressRotation.WebhookTimeout), WebhookRetries: value.EgressRotation.WebhookRetries,
			SettleDelay: Duration(value.EgressRotation.SettleDelay), ProbeTimeout: Duration(value.EgressRotation.ProbeTimeout),
			ProbeInterval: Duration(value.EgressRotation.ProbeInterval),
		}
	}
	return base, nil
}

func ToRuntimeSettings(value Config) settingsdomain.Config {
	randomDelay := value.Batch.RandomDelay.Value()
	accountIsolatedConnections := value.Routing.AccountIsolatedConnections
	return settingsdomain.Config{
		Server: settingsdomain.ServerConfig{MaxConcurrentRequests: value.Server.MaxConcurrentRequests},
		ProviderBuild: settingsdomain.ProviderBuildConfig{
			BaseURL: value.Provider.Build.BaseURL, FallbackBaseURL: settingsdomain.NormalizeBuildFallbackBaseURL(value.Provider.Build.FallbackBaseURL),
			ClientVersion: value.Provider.Build.ClientVersion, ClientIdentifier: value.Provider.Build.ClientIdentifier,
			TokenAuth: value.Provider.Build.TokenAuth, UserAgent: value.Provider.Build.UserAgent,
			SessionIdleConnTimeout: value.Provider.Build.SessionIdleConnTimeout.Value(),
			ResponseHeaderTimeout:  value.Provider.Build.ResponseHeaderTimeout.Value(),
			StreamIdleTimeout:      value.Provider.Build.StreamIdleTimeout.Value(),
		},
		ProviderWeb: settingsdomain.ProviderWebConfig{
			BaseURL: value.Provider.Web.BaseURL, QuotaTimeout: value.Provider.Web.QuotaTimeout.Value(),
			StatsigMode: value.Provider.Web.StatsigMode, StatsigManualValue: value.Provider.Web.StatsigManualValue,
			StatsigSignerURL: value.Provider.Web.StatsigSignerURL,
			ClearanceMode:    value.Provider.Web.ClearanceMode, FlareSolverrURL: value.Provider.Web.FlareSolverrURL,
			ClearanceTimeout: value.Provider.Web.ClearanceTimeout.Value(), ClearanceRefresh: value.Provider.Web.ClearanceRefresh.Value(),
			ChatTimeout: value.Provider.Web.ChatTimeout.Value(), StreamIdleTimeout: value.Provider.Web.StreamIdleTimeout.Value(),
			ImageTimeout:     value.Provider.Web.ImageTimeout.Value(),
			VideoTimeout:     value.Provider.Web.VideoTimeout.Value(),
			MediaConcurrency: value.Provider.Web.MediaConcurrency, AllowNSFW: value.Provider.Web.AllowNSFW,
			RecoveryBackoffBase: value.Provider.Web.RecoveryBackoffBase.Value(), RecoveryBackoffMax: value.Provider.Web.RecoveryBackoffMax.Value(),
		},
		ProviderConsole: settingsdomain.ProviderConsoleConfig{
			BaseURL: value.Provider.Console.BaseURL, ChatTimeout: value.Provider.Console.ChatTimeout.Value(),
			StreamIdleTimeout: value.Provider.Console.StreamIdleTimeout.Value(),
		},
		Batch: settingsdomain.BatchConfig{
			ImportConcurrency: value.Batch.ImportConcurrency, ConversionConcurrency: value.Batch.ConversionConcurrency,
			SyncConcurrency: value.Batch.SyncConcurrency, RefreshConcurrency: value.Batch.RefreshConcurrency,
			RandomDelay: &randomDelay,
		},
		Media: settingsdomain.MediaConfig{
			MaxImageBytes: value.Media.MaxImageBytes, MaxTotalBytes: value.Media.MaxTotalBytes,
			CleanupThresholdPercent: value.Media.CleanupThresholdPercent, CleanupInterval: value.Media.CleanupInterval.Value(),
		},
		Frontend: settingsdomain.FrontendConfig{
			PublicAPIBaseURL:     value.Frontend.PublicAPIBaseURLOverride,
			FilePublicAPIBaseURL: value.Frontend.PublicAPIBaseURL,
		},
		Routing: settingsdomain.RoutingConfig{
			StickyTTL: value.Routing.StickyTTL.Value(), CooldownBase: value.Routing.CooldownBase.Value(),
			CooldownMax: value.Routing.CooldownMax.Value(), CapacityWait: value.Routing.CapacityWait.Value(), MaxAttempts: value.Routing.MaxAttempts, VideoMaxAttempts: value.Routing.VideoMaxAttempts,
			MarkBuildChatDeniedAsReauth: value.Routing.MarkBuildChatDeniedAsReauth,
			PreferFreeBuild:             value.Routing.PreferFreeBuild,
			AccountIsolatedConnections:  &accountIsolatedConnections,
			SegmentedSelector: &settingsdomain.SegmentedSelectorConfig{
				ActiveEnabled: value.Routing.SegmentedSelectorEnabled,
				MinCandidates: value.Routing.SegmentedMinCandidates, WindowSize: value.Routing.SegmentedWindowSize,
			},
		},
		Audit: settingsdomain.AuditConfig{
			BufferSize: value.Audit.BufferSize, BatchSize: value.Audit.BatchSize, FlushInterval: value.Audit.FlushInterval.Value(), CommitDelay: value.Audit.CommitDelay.Value(),
			RetentionPeriod: durationPointer(value.Audit.RetentionPeriod.Value()),
			RetentionSource: value.Audit.RetentionSource,
		},
		ClientKeyDefaults: settingsdomain.ClientKeyDefaultsConfig{
			RPMLimit: value.ClientKeyDefaults.RPMLimit, MaxConcurrent: value.ClientKeyDefaults.MaxConcurrent,
		},
		Accounts: settingsdomain.AccountsConfig{
			MarkBuildForbiddenReauth:             value.Accounts.MarkBuildForbiddenReauth,
			BuildForbiddenReauthCodes:            append([]string(nil), value.Accounts.BuildForbiddenReauthCodes...),
			ExcludeBuildBotFlaggedFromScheduling: value.Accounts.ExcludeBuildBotFlaggedFromScheduling,
			AutoCleanReauthEnabled:               value.Accounts.AutoCleanReauthEnabled,
			AutoCleanReauthInterval:              value.Accounts.AutoCleanReauthInterval.Value(),
			AutoCleanReauthMinAge:                value.Accounts.AutoCleanReauthMinAge.Value(),
			AutoCleanIncludeDisabled:             value.Accounts.AutoCleanIncludeDisabled,
		},
		RequestRetry: &settingsdomain.RequestRetryConfig{
			Enabled:             value.RequestRetry.Enabled,
			MaxAttempts:         value.RequestRetry.MaxAttempts,
			OnExhausted:         value.RequestRetry.OnExhausted,
			AccountCooldown:     value.RequestRetry.AccountCooldown.Value(),
			EvidenceTimeout:     value.RequestRetry.EvidenceTimeout.Value(),
			CreatedTimeout:      value.RequestRetry.CreatedTimeout.Value(),
			IdleAccountCooldown: value.RequestRetry.IdleAccountCooldown.Value(),
			// guardedModels 是 yaml 级；apply 忽略 overlay，不回写以免陈旧名单进库。
		},
		EgressRotation: &settingsdomain.EgressRotationConfig{
			Enabled:                  value.Egress.Rotation.Enabled,
			MaxAttemptsPerQuarantine: value.Egress.Rotation.MaxAttemptsPerQuarantine,
			MinNodeInterval:          value.Egress.Rotation.MinNodeInterval.Value(),
			MaxGlobalPerHour:         value.Egress.Rotation.MaxGlobalPerHour,
			WebhookTimeout:           value.Egress.Rotation.WebhookTimeout.Value(),
			WebhookRetries:           value.Egress.Rotation.WebhookRetries,
			SettleDelay:              value.Egress.Rotation.SettleDelay.Value(),
			ProbeTimeout:             value.Egress.Rotation.ProbeTimeout.Value(),
			ProbeInterval:            value.Egress.Rotation.ProbeInterval.Value(),
		},
	}
}

func durationPointer(value time.Duration) *time.Duration { return &value }
