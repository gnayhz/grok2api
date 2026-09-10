package repository

import (
	"context"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

// RuntimeSettingsRepository owns the durable settings revision across all instances.
type RuntimeSettingsRepository interface {
	// Get reports whether an override is active. A reset returns false with its
	// durable revision and timestamp; only a never-written store has revision 0.
	Get(ctx context.Context) (settingsdomain.Config, time.Time, uint64, bool, error)
	Save(ctx context.Context, value settingsdomain.Config, expectedRevision uint64) (time.Time, uint64, error)
	// Reset removes the override while advancing the same CAS clock as Save.
	Reset(ctx context.Context, expectedRevision uint64) (time.Time, uint64, error)
}

// LegacyQualitySettingsMigration performs the one-time transfer of network
// capacity from the retired quality writer to the gateway settings clock.
type LegacyQualitySettingsMigration interface {
	MigrateQualityRotation(context.Context, settingsdomain.Config) error
}
