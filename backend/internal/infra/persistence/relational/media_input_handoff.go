package relational

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *MediaJobRepository) createMediaJobWithInputs(ctx context.Context, value media.Job) error {
	inputs, err := media.LocalInputAssets(value.InputJSON)
	if err != nil {
		return err
	}
	if len(inputs) == 0 {
		return r.db.db.WithContext(ctx).Create(mediaJobFromDomain(value)).Error
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// LocalAssets supplies a stable order for jobs sharing several inputs.
		// Release uses the same asset lock before observing current active jobs.
		var earliestExpiry time.Time
		for _, input := range inputs {
			var asset mediaAssetModel
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "kind", "expires_at").Where("id = ?", input.ID).First(&asset).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return media.ErrVideoInputUnavailable
				}
				return err
			}
			if asset.Kind != input.Kind || asset.ExpiresAt == nil {
				return media.ErrVideoInputUnavailable
			}
			if earliestExpiry.IsZero() || asset.ExpiresAt.Before(earliestExpiry) {
				earliestExpiry = *asset.ExpiresAt
			}
		}
		if !earliestExpiry.After(time.Now().UTC()) {
			return media.ErrVideoInputUnavailable
		}
		if err := tx.Create(mediaJobFromDomain(value)).Error; err != nil {
			return err
		}
		// An insert may wait on a foreign key or storage. Do not knowingly commit
		// a job whose input expired during that wait; hard TTL still applies after
		// acceptance and does not promise availability for the whole execution.
		if !earliestExpiry.After(time.Now().UTC()) {
			return media.ErrVideoInputUnavailable
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

func (r *MediaAssetRepository) ExpireMediaInputIfUnreferenced(ctx context.Context, id string, expiresAt time.Time) (bool, error) {
	if !media.IsInputAssetID(id) {
		return false, nil
	}
	expired := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var asset mediaAssetModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "kind", "expires_at").Where("id = ?", id).First(&asset).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if asset.ExpiresAt == nil || (asset.Kind != "image" && asset.Kind != "video") {
			return nil
		}
		// A separate statement after the lock observes a creation that committed
		// while we waited (PostgreSQL READ COMMITTED). A single UPDATE with a
		// NOT EXISTS predicate would retain its earlier statement snapshot.
		referenced, err := activeMediaInputReference(tx, id)
		if err != nil || referenced {
			return err
		}
		result := tx.Model(&mediaAssetModel{}).Where("id = ?", id).Update("expires_at", expiresAt)
		expired = result.RowsAffected > 0
		return result.Error
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return expired && err == nil, err
}

func activeMediaInputReference(tx *gorm.DB, id string) (bool, error) {
	// SQL only narrows candidates; the domain parser decides actual references.
	// Escaped JSON can conceal any part of an ID, so include those rows too.
	// Stream one payload at a time rather than materializing all active inputs.
	pattern := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(id) + "%"
	rows, err := tx.Model(&mediaJobModel{}).Select("input_json").
		Where("status IN ? AND (input_json LIKE ? ESCAPE '!' OR input_json LIKE ? ESCAPE '!')", []media.Status{media.StatusQueued, media.StatusInProgress}, pattern, "%\\%").Rows()
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		for _, reference := range media.DecodeVideoInput(raw).References() {
			if inputID, ok := media.ParseInputReference(reference); ok && inputID == id {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}
