package settings

import (
	"context"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func MigrateLegacyQualityRotation(ctx context.Context, base settingsdomain.Config, store repository.LegacyQualitySettingsMigration) error {
	return store.MigrateQualityRotation(ctx, base)
}
