package app

import (
	"fmt"
	"strings"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
)

func serverUpdateCheckEnabled(cfg config.Config) bool {
	if cfg.Server.UpdateCheckEnabled == nil {
		return true
	}
	return *cfg.Server.UpdateCheckEnabled
}

func invalidationSourceInstance(cfg config.Config) string {
	if value := strings.TrimSpace(cfg.Deployment.InstanceID); value != "" {
		return value
	}
	return fmt.Sprintf("process-%d", time.Now().UnixNano())
}

func maxBatchConcurrency(value config.BatchConfig) int {
	return max(value.ImportConcurrency, value.ConversionConcurrency, value.SyncConcurrency, value.RefreshConcurrency)
}

func webProviderConfig(cfg config.Config) webprovider.Config {
	return webprovider.Config{
		BaseURL: cfg.Provider.Web.BaseURL, QuotaTimeout: cfg.Provider.Web.QuotaTimeout.Value(),
		StatsigMode: cfg.Provider.Web.StatsigMode, StatsigManualValue: cfg.Provider.Web.StatsigManualValue,
		StatsigSignerURL: cfg.Provider.Web.StatsigSignerURL,
		ChatTimeout:      cfg.Provider.Web.ChatTimeout.Value(), StreamIdleTimeout: cfg.Provider.Web.StreamIdleTimeout.Value(),
		ImageTimeout: cfg.Provider.Web.ImageTimeout.Value(),
		VideoTimeout: cfg.Provider.Web.VideoTimeout.Value(), MaxInputImageBytes: cfg.Media.MaxImageBytes,
		AllowNSFW: cfg.Provider.Web.AllowNSFW,
	}
}

func consoleProviderConfig(cfg config.Config) consoleprovider.Config {
	return consoleprovider.Config{
		BaseURL: cfg.Provider.Console.BaseURL, SessionBaseURL: cfg.Provider.Web.BaseURL,
		Timeout: cfg.Provider.Console.ChatTimeout.Value(), StreamIdleTimeout: cfg.Provider.Console.StreamIdleTimeout.Value(),
	}
}

func accountAutoCleanConfig(value config.AccountsConfig) accountapp.AutoCleanConfig {
	return accountapp.AutoCleanConfig{
		Enabled:         value.AutoCleanReauthEnabled,
		Interval:        value.AutoCleanReauthInterval.Value(),
		MinAge:          value.AutoCleanReauthMinAge.Value(),
		IncludeDisabled: value.AutoCleanIncludeDisabled,
	}
}

func auditLedgerConfig(value config.AuditConfig) auditapp.LedgerConfig {
	return auditapp.LedgerConfig{
		Mode:                      auditapp.LedgerMode(value.LedgerMode),
		FailureThreshold:          value.LedgerFailureThreshold,
		UnhealthyGrace:            value.LedgerUnhealthyGrace.Value(),
		QueueHighWatermarkPercent: value.LedgerQueueHighWatermarkPct,
	}
}

func mediaConfig(cfg config.Config) mediaapp.Config {
	return mediaapp.Config{
		PublicBaseURL: cfg.Frontend.EffectivePublicAPIBaseURL(),
		MaxImageBytes: cfg.Media.MaxImageBytes, MaxTotalBytes: cfg.Media.MaxTotalBytes,
		CleanupThresholdPercent: cfg.Media.CleanupThresholdPercent, CleanupInterval: cfg.Media.CleanupInterval.Value(),
	}
}
