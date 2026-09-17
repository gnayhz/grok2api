package settings

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

func (s *Service) mergeEditable(current settingsdomain.Config, input EditableConfig) (settingsdomain.Config, error) {
	if input.Audit.CommitDelayMS < 0 {
		return settingsdomain.Config{}, errors.New("audit.commitDelayMS 不能为负数")
	}
	if int64(input.Audit.CommitDelayMS) > math.MaxInt64/int64(time.Millisecond) {
		return settingsdomain.Config{}, errors.New("audit.commitDelayMS 超出有效时长范围")
	}
	next := current
	// Optional sections belong to the candidate snapshot. Failed validation or
	// a durable CAS conflict must leave both active and file snapshots intact.
	if current.RequestRetry != nil {
		cloned := *current.RequestRetry
		next.RequestRetry = &cloned
	}
	if current.EgressRotation != nil {
		cloned := *current.EgressRotation
		next.EgressRotation = &cloned
	}
	next.Server.MaxConcurrentRequests = input.Server.MaxConcurrentRequests
	next.ProviderBuild.BaseURL = strings.TrimSpace(input.ProviderBuild.BaseURL)
	next.ProviderBuild.FallbackBaseURL = normalizeBuildFallbackBaseURL(input.ProviderBuild.FallbackBaseURL)
	next.ProviderBuild.ClientVersion = strings.TrimSpace(input.ProviderBuild.ClientVersion)
	next.ProviderBuild.ClientIdentifier = strings.TrimSpace(input.ProviderBuild.ClientIdentifier)
	if tokenAuth := strings.TrimSpace(input.ProviderBuild.TokenAuth); tokenAuth != "" {
		next.ProviderBuild.TokenAuth = tokenAuth
	}
	next.ProviderBuild.UserAgent = strings.TrimSpace(input.ProviderBuild.UserAgent)
	next.ProviderWeb.BaseURL = strings.TrimSpace(input.ProviderWeb.BaseURL)
	next.ProviderWeb.StatsigMode = strings.TrimSpace(input.ProviderWeb.StatsigMode)
	next.ProviderWeb.StatsigSignerURL = strings.TrimSpace(input.ProviderWeb.StatsigSignerURL)
	if input.ProviderWeb.ClearanceProvided {
		next.ProviderWeb.ClearanceMode = strings.TrimSpace(input.ProviderWeb.ClearanceMode)
		next.ProviderWeb.FlareSolverrURL = strings.TrimSpace(input.ProviderWeb.FlareSolverrURL)
	}
	if next.ProviderWeb.StatsigMode == statsigModeManual {
		if value := strings.TrimSpace(input.ProviderWeb.StatsigManualValue); value != "" {
			next.ProviderWeb.StatsigManualValue = value
		}
	} else {
		next.ProviderWeb.StatsigManualValue = ""
	}
	next.ProviderWeb.MediaConcurrency = input.ProviderWeb.MediaConcurrency
	next.ProviderWeb.AllowNSFW = input.ProviderWeb.AllowNSFW
	next.ProviderConsole.BaseURL = strings.TrimSpace(input.ProviderConsole.BaseURL)
	next.Batch.ImportConcurrency = input.Batch.ImportConcurrency
	next.Batch.ConversionConcurrency = input.Batch.ConversionConcurrency
	next.Batch.SyncConcurrency = input.Batch.SyncConcurrency
	next.Batch.RefreshConcurrency = input.Batch.RefreshConcurrency
	next.Media.MaxImageBytes = input.Media.MaxImageBytes
	next.Media.MaxTotalBytes = input.Media.MaxTotalBytes
	next.Media.CleanupThresholdPercent = input.Media.CleanupThresholdPercent
	next.Frontend.PublicAPIBaseURL = strings.TrimSpace(input.Frontend.PublicAPIBaseURL)
	next.Routing.MaxAttempts = input.Routing.MaxAttempts
	next.Routing.VideoMaxAttempts = input.Routing.VideoMaxAttempts
	next.Routing.PreferFreeBuild = input.Routing.PreferFreeBuild
	if input.Routing.AccountIsolatedConnectionsProvided {
		next.Routing.AccountIsolatedConnections = boolPointer(input.Routing.AccountIsolatedConnections)
	}
	if input.Routing.SegmentedSelectorProvided {
		next.Routing.SegmentedSelector = &settingsdomain.SegmentedSelectorConfig{
			ActiveEnabled: input.Routing.SegmentedSelector.Enabled,
			MinCandidates: input.Routing.SegmentedSelector.MinCandidates,
			WindowSize:    input.Routing.SegmentedSelector.WindowSize,
		}
	}
	if input.Routing.MarkBuildChatDeniedAsReauthProvided {
		next.Routing.MarkBuildChatDeniedAsReauth = input.Routing.MarkBuildChatDeniedAsReauth
	}
	next.Audit.BufferSize = input.Audit.BufferSize
	next.Audit.BatchSize = input.Audit.BatchSize
	if input.Audit.CommitDelayMS > 0 {
		delay := time.Duration(input.Audit.CommitDelayMS) * time.Millisecond
		if err := auditdomain.ValidateCommitDelay(delay); err != nil {
			return settingsdomain.Config{}, err
		}
		next.Audit.CommitDelay = delay
	}
	if err := mergeAuditRetention(&next.Audit, input.Audit); err != nil {
		return settingsdomain.Config{}, err
	}
	next.ClientKeyDefaults.RPMLimit = input.ClientKeyDefaults.RPMLimit
	next.ClientKeyDefaults.MaxConcurrent = input.ClientKeyDefaults.MaxConcurrent
	if input.AccountsProvided {
		if input.Accounts.MarkBuildForbiddenReauthProvided {
			next.Accounts.MarkBuildForbiddenReauth = input.Accounts.MarkBuildForbiddenReauth
		}
		if input.Accounts.BuildForbiddenReauthCodesProvided {
			next.Accounts.BuildForbiddenReauthCodes = normalizeForbiddenCodes(input.Accounts.BuildForbiddenReauthCodes)
		}
		if input.Accounts.ExcludeBuildBotFlaggedFromSchedulingProvided {
			next.Accounts.ExcludeBuildBotFlaggedFromScheduling = input.Accounts.ExcludeBuildBotFlaggedFromScheduling
		}
		next.Accounts.AutoCleanReauthEnabled = input.Accounts.AutoCleanReauthEnabled
		next.Accounts.AutoCleanIncludeDisabled = input.Accounts.AutoCleanIncludeDisabled
	}
	if input.RequestRetryProvided {
		if next.RequestRetry == nil {
			next.RequestRetry = &settingsdomain.RequestRetryConfig{}
		}
		next.RequestRetry.Enabled = input.RequestRetry.Enabled
		next.RequestRetry.MaxAttempts = input.RequestRetry.MaxAttempts
		next.RequestRetry.OnExhausted = strings.TrimSpace(input.RequestRetry.OnExhausted)
	}
	if input.EgressRotationProvided {
		if next.EgressRotation == nil {
			next.EgressRotation = &settingsdomain.EgressRotationConfig{}
		}
		next.EgressRotation.Enabled = input.EgressRotation.Enabled
		next.EgressRotation.MaxAttemptsPerQuarantine = input.EgressRotation.MaxAttemptsPerQuarantine
		next.EgressRotation.MaxGlobalPerHour = input.EgressRotation.MaxGlobalPerHour
		next.EgressRotation.WebhookRetries = input.EgressRotation.WebhookRetries
	}

	type durationInput struct {
		path  string
		value string
		set   func(time.Duration)
	}
	durations := []durationInput{
		{"routing.stickyTTL", input.Routing.StickyTTL, func(value time.Duration) { next.Routing.StickyTTL = value }},
		{"routing.cooldownBase", input.Routing.CooldownBase, func(value time.Duration) { next.Routing.CooldownBase = value }},
		{"routing.cooldownMax", input.Routing.CooldownMax, func(value time.Duration) { next.Routing.CooldownMax = value }},
		{"routing.capacityWait", input.Routing.CapacityWait, func(value time.Duration) { next.Routing.CapacityWait = value }},
		{"audit.flushInterval", input.Audit.FlushInterval, func(value time.Duration) { next.Audit.FlushInterval = value }},
		{"providerWeb.quotaTimeout", input.ProviderWeb.QuotaTimeout, func(value time.Duration) { next.ProviderWeb.QuotaTimeout = value }},
		{"providerWeb.chatTimeout", input.ProviderWeb.ChatTimeout, func(value time.Duration) { next.ProviderWeb.ChatTimeout = value }},
		{"providerWeb.imageTimeout", input.ProviderWeb.ImageTimeout, func(value time.Duration) { next.ProviderWeb.ImageTimeout = value }},
		{"providerWeb.videoTimeout", input.ProviderWeb.VideoTimeout, func(value time.Duration) { next.ProviderWeb.VideoTimeout = value }},
		{"providerWeb.recoveryBackoffBase", input.ProviderWeb.RecoveryBackoffBase, func(value time.Duration) { next.ProviderWeb.RecoveryBackoffBase = value }},
		{"providerWeb.recoveryBackoffMax", input.ProviderWeb.RecoveryBackoffMax, func(value time.Duration) { next.ProviderWeb.RecoveryBackoffMax = value }},
		{"providerConsole.chatTimeout", input.ProviderConsole.ChatTimeout, func(value time.Duration) { next.ProviderConsole.ChatTimeout = value }},
		{"media.cleanupInterval", input.Media.CleanupInterval, func(value time.Duration) { next.Media.CleanupInterval = value }},
		{"batch.randomDelay", input.Batch.RandomDelay, func(value time.Duration) { next.Batch.RandomDelay = durationPointer(value) }},
	}
	if strings.TrimSpace(input.ProviderBuild.ResponseHeaderTimeout) != "" {
		durations = append(durations, durationInput{"providerBuild.responseHeaderTimeout", input.ProviderBuild.ResponseHeaderTimeout, func(value time.Duration) { next.ProviderBuild.ResponseHeaderTimeout = value }})
	}
	if strings.TrimSpace(input.ProviderBuild.StreamIdleTimeout) != "" {
		durations = append(durations, durationInput{"providerBuild.streamIdleTimeout", input.ProviderBuild.StreamIdleTimeout, func(value time.Duration) { next.ProviderBuild.StreamIdleTimeout = value }})
	}
	if strings.TrimSpace(input.ProviderWeb.StreamIdleTimeout) != "" {
		durations = append(durations, durationInput{"providerWeb.streamIdleTimeout", input.ProviderWeb.StreamIdleTimeout, func(value time.Duration) { next.ProviderWeb.StreamIdleTimeout = value }})
	}
	if strings.TrimSpace(input.ProviderConsole.StreamIdleTimeout) != "" {
		durations = append(durations, durationInput{"providerConsole.streamIdleTimeout", input.ProviderConsole.StreamIdleTimeout, func(value time.Duration) { next.ProviderConsole.StreamIdleTimeout = value }})
	}
	if input.ProviderWeb.ClearanceProvided {
		durations = append(durations,
			durationInput{"providerWeb.clearanceTimeout", input.ProviderWeb.ClearanceTimeout, func(value time.Duration) { next.ProviderWeb.ClearanceTimeout = value }},
			durationInput{"providerWeb.clearanceRefresh", input.ProviderWeb.ClearanceRefresh, func(value time.Duration) { next.ProviderWeb.ClearanceRefresh = value }},
		)
	}
	if input.AccountsProvided {
		durations = append(durations,
			durationInput{"accounts.autoCleanReauthInterval", input.Accounts.AutoCleanReauthInterval, func(value time.Duration) { next.Accounts.AutoCleanReauthInterval = value }},
			durationInput{"accounts.autoCleanReauthMinAge", input.Accounts.AutoCleanReauthMinAge, func(value time.Duration) { next.Accounts.AutoCleanReauthMinAge = value }},
		)
	}
	if input.RequestRetryProvided {
		if next.RequestRetry == nil {
			next.RequestRetry = &settingsdomain.RequestRetryConfig{}
		}
		durations = append(durations,
			durationInput{"requestRetry.accountCooldown", input.RequestRetry.AccountCooldown, func(value time.Duration) { next.RequestRetry.AccountCooldown = value }},
			durationInput{"requestRetry.evidenceTimeout", input.RequestRetry.EvidenceTimeout, func(value time.Duration) { next.RequestRetry.EvidenceTimeout = value }},
			durationInput{"requestRetry.createdTimeout", input.RequestRetry.CreatedTimeout, func(value time.Duration) { next.RequestRetry.CreatedTimeout = value }},
			durationInput{"requestRetry.idleAccountCooldown", input.RequestRetry.IdleAccountCooldown, func(value time.Duration) { next.RequestRetry.IdleAccountCooldown = value }},
		)
	}
	if input.EgressRotationProvided {
		if next.EgressRotation == nil {
			next.EgressRotation = &settingsdomain.EgressRotationConfig{}
		}
		durations = append(durations,
			durationInput{"egressRotation.minNodeInterval", input.EgressRotation.MinNodeInterval, func(value time.Duration) { next.EgressRotation.MinNodeInterval = value }},
			durationInput{"egressRotation.webhookTimeout", input.EgressRotation.WebhookTimeout, func(value time.Duration) { next.EgressRotation.WebhookTimeout = value }},
			durationInput{"egressRotation.settleDelay", input.EgressRotation.SettleDelay, func(value time.Duration) { next.EgressRotation.SettleDelay = value }},
			durationInput{"egressRotation.probeTimeout", input.EgressRotation.ProbeTimeout, func(value time.Duration) { next.EgressRotation.ProbeTimeout = value }},
			durationInput{"egressRotation.probeInterval", input.EgressRotation.ProbeInterval, func(value time.Duration) { next.EgressRotation.ProbeInterval = value }},
		)
	}
	for _, item := range durations {
		value, err := time.ParseDuration(strings.TrimSpace(item.value))
		if err != nil {
			return settingsdomain.Config{}, fmt.Errorf("%s 必须是有效时长", item.path)
		}
		item.set(value)
	}
	if next.ProviderWeb.StreamIdleTimeout > next.ProviderWeb.ChatTimeout {
		return settingsdomain.Config{}, errors.New("providerWeb.streamIdleTimeout 不能超过 providerWeb.chatTimeout")
	}
	if next.ProviderConsole.StreamIdleTimeout > next.ProviderConsole.ChatTimeout {
		return settingsdomain.Config{}, errors.New("providerConsole.streamIdleTimeout 不能超过 providerConsole.chatTimeout")
	}
	if s.validate != nil {
		if err := s.validate(next); err != nil {
			return settingsdomain.Config{}, err
		}
	}
	return next, nil
}
