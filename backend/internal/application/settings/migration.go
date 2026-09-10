package settings

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// MigrateLegacyQualityRotation runs before constructing runtime consumers.
// The repository preserves unrelated overrides and commits both documents atomically.
func MigrateLegacyQualityRotation(ctx context.Context, base config.Config, store repository.LegacyQualitySettingsMigration) error {
	return store.MigrateQualityRotation(ctx, toDomainConfig(base))
}
