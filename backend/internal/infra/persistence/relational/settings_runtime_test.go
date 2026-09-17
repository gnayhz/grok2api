package relational

import (
	"context"
	"time"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func loadSettingsConfig(ctx context.Context, base config.Config, repo repository.RuntimeSettingsRepository) (config.Config, time.Time, uint64, error) {
	runtime, updatedAt, revision, found, err := settingsapp.LoadPersisted(ctx, repo)
	if err != nil {
		return config.Config{}, updatedAt, revision, err
	}
	if !found {
		return base, updatedAt, revision, nil
	}
	loaded, err := config.ApplyRuntimeSettings(base, runtime)
	if err != nil {
		return config.Config{}, updatedAt, revision, err
	}
	if err := loaded.Validate(); err != nil {
		return config.Config{}, updatedAt, revision, err
	}
	return loaded, updatedAt, revision, nil
}

func egressRotationLimit(cfg settingsdomain.Config) int {
	if cfg.EgressRotation == nil {
		return 0
	}
	return cfg.EgressRotation.MaxGlobalPerHour
}
