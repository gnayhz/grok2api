package relational

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type SettingsDocumentRepository struct {
	database *Database
	key      string
}

func NewSettingsDocumentRepository(database *Database, key string) *SettingsDocumentRepository {
	return &SettingsDocumentRepository{database: database, key: key}
}

func (r *SettingsDocumentRepository) Load(ctx context.Context) (repository.SettingsDocument, error) {
	var row runtimeSettingsModel
	err := r.database.db.WithContext(ctx).Where("key = ?", r.key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return repository.SettingsDocument{}, nil
	}
	if err != nil {
		return repository.SettingsDocument{}, err
	}
	return repository.SettingsDocument{Payload: []byte(row.ValueJSON), Revision: row.Revision, UpdatedAt: row.UpdatedAt}, nil
}

func (r *SettingsDocumentRepository) Save(ctx context.Context, payload []byte, expected uint64) (repository.SettingsDocument, error) {
	at, revision, err := writeSettingsDocument(r.database.db.WithContext(ctx), r.key, string(payload), expected)
	if err != nil {
		return repository.SettingsDocument{}, err
	}
	return repository.SettingsDocument{Payload: append([]byte(nil), payload...), Revision: revision, UpdatedAt: at}, nil
}

func writeSettingsDocument(db *gorm.DB, key, payload string, expected uint64) (time.Time, uint64, error) {
	if expected >= math.MaxInt64 {
		return time.Time{}, 0, errors.New("runtime settings revision exhausted")
	}
	now, next := time.Now().UTC(), expected+1
	if expected == 0 {
		row := runtimeSettingsModel{Key: key, ValueJSON: payload, Revision: next, UpdatedAt: now}
		if err := db.Create(&row).Error; err != nil {
			return time.Time{}, 0, mapError(err)
		}
		return now, next, nil
	}
	result := db.Model(&runtimeSettingsModel{}).Where("key = ? AND revision = ?", key, expected).
		Updates(map[string]any{"value_json": payload, "revision": next, "updated_at": now})
	if result.Error != nil {
		return time.Time{}, 0, result.Error
	}
	if result.RowsAffected != 1 {
		return time.Time{}, 0, repository.ErrConflict
	}
	return now, next, nil
}
