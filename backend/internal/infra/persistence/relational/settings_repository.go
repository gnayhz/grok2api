package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"gorm.io/gorm"
)

const runtimeSettingsKey = "gateway"

type runtimeSettingsPayload struct {
	UseFileDefaults             bool                  `json:"useFileDefaults,omitempty"`
	Config                      settingsdomain.Config `json:"config"`
	EncryptedStatsigManualValue string                `json:"encryptedStatsigManualValue,omitempty"`
}

type RuntimeSettingsRepository struct {
	database *Database
	cipher   security.Cryptor
}

func NewRuntimeSettingsRepository(database *Database, cipher security.Cryptor) *RuntimeSettingsRepository {
	return &RuntimeSettingsRepository{database: database, cipher: cipher}
}

// Reset keeps a durable tombstone so restarted instances cannot restart the clock.
func (r *RuntimeSettingsRepository) Reset(ctx context.Context, expectedRevision uint64) (time.Time, uint64, error) {
	return r.write(ctx, `{"useFileDefaults":true}`, expectedRevision)
}

func (r *RuntimeSettingsRepository) Get(ctx context.Context) (settingsdomain.Config, time.Time, uint64, bool, error) {
	var row runtimeSettingsModel
	err := r.database.db.WithContext(ctx).Where("key = ?", runtimeSettingsKey).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return settingsdomain.Config{}, time.Time{}, 0, false, nil
	}
	if err != nil {
		return settingsdomain.Config{}, time.Time{}, 0, false, err
	}
	var payload runtimeSettingsPayload
	if err := json.Unmarshal([]byte(row.ValueJSON), &payload); err != nil {
		return settingsdomain.Config{}, time.Time{}, 0, false, fmt.Errorf("解析运行设置: %w", err)
	}
	if payload.UseFileDefaults {
		return settingsdomain.Config{}, row.UpdatedAt, row.Revision, false, nil
	}
	manualValue, err := r.cipher.Decrypt(payload.EncryptedStatsigManualValue)
	if err != nil {
		return settingsdomain.Config{}, time.Time{}, 0, false, fmt.Errorf("解密 Statsig 手动值: %w", err)
	}
	payload.Config.ProviderWeb.StatsigManualValue = manualValue
	return payload.Config, row.UpdatedAt, row.Revision, true, nil
}

func (r *RuntimeSettingsRepository) Save(ctx context.Context, value settingsdomain.Config, expectedRevision uint64) (time.Time, uint64, error) {
	manualValue, err := r.cipher.Encrypt(value.ProviderWeb.StatsigManualValue)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("加密 Statsig 手动值: %w", err)
	}
	value.ProviderWeb.StatsigManualValue = ""
	payload, err := json.Marshal(runtimeSettingsPayload{Config: value, EncryptedStatsigManualValue: manualValue})
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("编码运行设置: %w", err)
	}
	return r.write(ctx, string(payload), expectedRevision)
}

func (r *RuntimeSettingsRepository) write(ctx context.Context, payload string, expectedRevision uint64) (time.Time, uint64, error) {
	return writeSettingsDocument(r.database.db.WithContext(ctx), runtimeSettingsKey, payload, expectedRevision)
}
