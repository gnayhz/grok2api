package settings

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"time"
)

func newTestService(cfg config.Config, updatedAt time.Time, revision uint64, repo repository.RuntimeSettingsRepository, notify func(context.Context), apply func(config.Config)) *Service {
	var publish func(context.Context) error
	if notify != nil {
		publish = func(ctx context.Context) error { notify(ctx); return nil }
	}
	var targets []ApplyTarget
	if apply != nil {
		targets = []ApplyTarget{{Name: "test", Apply: func(_ context.Context, next config.Config) error { apply(next); return nil }}}
	}
	return NewService(cfg, updatedAt, revision, repo, publish, targets)
}
