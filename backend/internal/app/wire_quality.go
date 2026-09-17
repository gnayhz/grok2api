package app

import (
	"context"
	"log/slog"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
	qualityevidence "github.com/chenyme/grok2api/backend/internal/quality/evidence"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// bootstrapQualityLayer 构建新质量层地基(重写批1:羁押登记处+证据局)
// 与底座同库、自带连接池。请求观测由durable events队列接收。
func bootstrapQualityLayer(ctx context.Context, cfg config.Config, logger *slog.Logger, links qualityregistry.AccountLinks) (*qualityregistry.Registry, *qualityevidence.Store, error) {
	opts := qualityregistry.Options{Logger: logger, AccountLinks: links}
	switch cfg.Database.Driver {
	case "postgres":
		opts.Driver, opts.PostgresDSN = "postgres", cfg.Database.Postgres.DSN
	default:
		opts.Driver, opts.SQLitePath = "sqlite", cfg.Database.SQLite.Path
	}
	registry, err := qualityregistry.Open(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	evidenceStore, err := qualityevidence.New(ctx, registry.DB(), qualitymodel.DefaultEvidenceConfig())
	if err != nil {
		_ = registry.Close()
		return nil, nil, err
	}
	return registry, evidenceStore, nil
}
