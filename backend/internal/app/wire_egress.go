package app

import (
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

func egressRotationConfig(cfg config.Config) egressapp.RotationConfig {
	return egressapp.RotationConfig{
		Enabled:                  cfg.Egress.Rotation.Enabled,
		MaxAttemptsPerQuarantine: cfg.Egress.Rotation.MaxAttemptsPerQuarantine,
		MinNodeInterval:          cfg.Egress.Rotation.MinNodeInterval.Value(),
		MaxGlobalPerHour:         cfg.Egress.Rotation.MaxGlobalPerHour,
		WebhookTimeout:           cfg.Egress.Rotation.WebhookTimeout.Value(),
		WebhookRetries:           cfg.Egress.Rotation.WebhookRetries,
		SettleDelay:              cfg.Egress.Rotation.SettleDelay.Value(),
		ProbeTimeout:             cfg.Egress.Rotation.ProbeTimeout.Value(),
		ProbeInterval:            cfg.Egress.Rotation.ProbeInterval.Value(),
	}
}

func clearanceConfig(cfg config.Config) infraegress.ClearanceConfig {
	return infraegress.ClearanceConfig{
		Mode: cfg.Provider.Web.ClearanceMode, FlareSolverrURL: cfg.Provider.Web.FlareSolverrURL,
		TargetURL: cfg.Provider.Web.BaseURL, Timeout: cfg.Provider.Web.ClearanceTimeout.Value(),
		RefreshInterval: cfg.Provider.Web.ClearanceRefresh.Value(),
	}
}
