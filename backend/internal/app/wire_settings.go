package app

import (
	"context"
	"fmt"
	"time"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func loadRuntimeSettings(ctx context.Context, fileCfg config.Config, repo repository.RuntimeSettingsRepository) (config.Config, time.Time, uint64, error) {
	runtime, updatedAt, revision, found, err := settingsapp.LoadPersisted(ctx, repo)
	if err != nil {
		return config.Config{}, time.Time{}, 0, err
	}
	if !found {
		return fileCfg, updatedAt, revision, nil
	}
	loaded, err := config.ApplyRuntimeSettings(fileCfg, runtime)
	if err != nil {
		return config.Config{}, time.Time{}, 0, err
	}
	if err := loaded.Validate(); err != nil {
		return config.Config{}, time.Time{}, 0, fmt.Errorf("校验运行设置: %w", err)
	}
	return loaded, updatedAt, revision, nil
}

func settingsApply(fileCfg config.Config, fn func(config.Config) error) func(context.Context, settingsdomain.Config) error {
	return func(_ context.Context, runtime settingsdomain.Config) error {
		next, err := config.ApplyRuntimeSnapshot(fileCfg, runtime)
		if err != nil {
			return err
		}
		return fn(next)
	}
}
