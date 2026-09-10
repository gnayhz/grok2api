package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MigrateQualityRotation transfers the old capacity override exactly once.
// Legacy startup applied quality's value after gateway's value, so it wins this
// migration. Both clocks advance in one transaction; an explicit marker records
// the transfer, including old documents with an implicit capacity default. Future saves/reset use only gateway's capacity policy.
func (r *RuntimeSettingsRepository) MigrateQualityRotation(ctx context.Context, baseline settingsdomain.Config) error {
	return r.database.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var quality runtimeSettingsModel
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", repository.QualitySettingsKey).First(&quality).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(quality.ValueJSON), &fields); err != nil {
			return fmt.Errorf("decode legacy quality settings: %w", err)
		}
		var migrated bool
		if raw, exists := fields["_network_capacity_migrated"]; exists {
			if err := json.Unmarshal(raw, &migrated); err != nil {
				return err
			}
		}
		if migrated {
			return nil
		}
		// Historical quality documents treated absent or zero capacity as 6. This
		// frozen migration value is not a second source of runtime network policy.
		limit := 6
		if raw, exists := fields["max_rotations_per_hour"]; exists {
			if err := json.Unmarshal(raw, &limit); err != nil {
				return fmt.Errorf("decode legacy rotation capacity: %w", err)
			}
			if limit == 0 {
				limit = 6
			}
		}
		if limit < 0 {
			return errors.New("legacy rotation capacity must be positive")
		}

		var gateway runtimeSettingsModel
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", runtimeSettingsKey).First(&gateway).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var payload runtimeSettingsPayload
		if err == nil {
			if err := json.Unmarshal([]byte(gateway.ValueJSON), &payload); err != nil {
				return err
			}
		}
		if gateway.Revision == 0 || payload.UseFileDefaults {
			encrypted, err := r.cipher.Encrypt(baseline.ProviderWeb.StatsigManualValue)
			if err != nil {
				return err
			}
			baseline.ProviderWeb.StatsigManualValue = ""
			payload = runtimeSettingsPayload{Config: baseline, EncryptedStatsigManualValue: encrypted}
		}
		if payload.Config.EgressRotation == nil {
			if baseline.EgressRotation == nil {
				return errors.New("missing rotation baseline for migration")
			}
			rotation := *baseline.EgressRotation
			payload.Config.EgressRotation = &rotation
		}
		payload.Config.EgressRotation.MaxGlobalPerHour = limit
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, _, err := writeSettingsDocument(tx, runtimeSettingsKey, string(encoded), gateway.Revision); err != nil {
			return err
		}
		delete(fields, "max_rotations_per_hour")
		fields["_network_capacity_migrated"] = json.RawMessage(`true`)
		encoded, err = json.Marshal(fields)
		if err != nil {
			return err
		}
		_, _, err = writeSettingsDocument(tx, repository.QualitySettingsKey, string(encoded), quality.Revision)
		return err
	})
}
