package settings

import (
	"context"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func runtimeOf(cfg config.Config) settingsdomain.Config {
	return config.ToRuntimeSettings(cfg)
}

func attachValidator(s *Service, base config.Config) *Service {
	s.SetRuntimeValidator(func(value settingsdomain.Config) error {
		_, err := config.ApplyRuntimeSnapshot(base, value)
		return err
	})
	s.SetPersistedResolver(func(value settingsdomain.Config) (settingsdomain.Config, error) {
		return config.ResolveRuntimeSettings(base, value)
	})
	return s
}

func newTestService(cfg config.Config, updatedAt time.Time, revision uint64, repo repository.RuntimeSettingsRepository, notify func(context.Context), apply func(settingsdomain.Config)) *Service {
	var publish func(context.Context) error
	if notify != nil {
		publish = func(ctx context.Context) error { notify(ctx); return nil }
	}
	var targets []ApplyTarget
	if apply != nil {
		targets = []ApplyTarget{{Name: "test", Apply: func(_ context.Context, next settingsdomain.Config) error { apply(next); return nil }}}
	}
	return attachValidator(NewService(runtimeOf(cfg), updatedAt, revision, repo, publish, targets), cfg)
}

func loadPersistedConfig(ctx context.Context, base config.Config, repo repository.RuntimeSettingsRepository) (config.Config, time.Time, uint64, error) {
	value, updatedAt, revision, found, err := LoadPersisted(ctx, repo)
	if err != nil {
		return config.Config{}, updatedAt, revision, err
	}
	if !found {
		return base, updatedAt, revision, nil
	}
	loaded, err := config.ApplyRuntimeSettings(base, value)
	if err != nil {
		return config.Config{}, updatedAt, revision, err
	}
	if err := loaded.Validate(); err != nil {
		return config.Config{}, updatedAt, revision, err
	}
	return loaded, updatedAt, revision, nil
}

func applyInfra(base config.Config, next settingsdomain.Config) config.Config {
	loaded, err := config.ApplyRuntimeSnapshot(base, next)
	if err != nil {
		return config.Config{}
	}
	return loaded
}
