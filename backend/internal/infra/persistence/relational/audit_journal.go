package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const auditJournalVersion = 1

type auditPendingModel struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement"`
	EventID     string `gorm:"not null;uniqueIndex:uidx_audit_pending_event"`
	ClientKeyID uint64 `gorm:"not null"`
	Payload     []byte `gorm:"not null"`
	Size        int64  `gorm:"not null;check:chk_audit_pending_size,size > 0"`
	Rejected    bool   `gorm:"not null;default:false;index:idx_audit_pending_scan,priority:1"`
	Reason      string `gorm:"not null;default:''"`
	CreatedAt   time.Time
}

func (auditPendingModel) TableName() string { return "audit_pending" }

type auditPendingMetaModel struct {
	ID       int    `gorm:"primaryKey;check:chk_audit_pending_meta_id,id = 1"`
	Version  int    `gorm:"not null"`
	Revision uint64 `gorm:"not null;check:chk_audit_pending_meta_revision,revision >= 0"`
	Records  int    `gorm:"not null;check:chk_audit_pending_meta_records,records >= 0"`
	Rejected int    `gorm:"not null;check:chk_audit_pending_meta_rejected,rejected >= 0 AND rejected <= records"`
	Bytes    int64  `gorm:"not null;check:chk_audit_pending_meta_bytes,bytes >= 0"`
}

func (auditPendingMetaModel) TableName() string { return "audit_pending_meta" }

type auditPendingPayload struct {
	Version int          `json:"version"`
	Record  audit.Record `json:"record"`
}

type AuditJournalOptions struct {
	MaxRecords int
	MaxBytes   int64
}

// AuditJournal uses its own local SQLite file, independent of the billing DB.
// An OS lock excludes a second owner and is released even on process death.
type AuditJournal struct {
	database *Database
	lock     *os.File
	options  AuditJournalOptions
	state    atomic.Pointer[repository.AuditPendingSnapshot]
	close    sync.Once
	closeErr error
}

func OpenAuditJournal(ctx context.Context, path string, options AuditJournalOptions) (_ *AuditJournal, resultErr error) {
	if options.MaxRecords <= 0 || options.MaxBytes <= 0 || strings.TrimSpace(path) == "" {
		return nil, errors.New("audit journal requires a persistent path and positive capacity")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := openAuditJournalFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	if err := lockAuditJournal(lock); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: %v", repository.ErrAuditPendingOwner, err)
	}
	defer func() {
		if resultErr != nil {
			_ = lock.Close()
		}
	}()
	file, err := openAuditJournalFile(path)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	database, err := openSQLite(ctx, path, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			_ = database.Close()
		}
	}()
	journal := &AuditJournal{database: database, lock: lock, options: options}
	if err := journal.initialize(ctx); err != nil {
		return nil, err
	}
	return journal, nil
}

func openAuditJournalFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("audit journal requires a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (j *AuditJournal) initialize(ctx context.Context) error {
	tx := j.database.db.WithContext(ctx)
	if tx.Migrator().HasTable(&auditPendingMetaModel{}) {
		var meta auditPendingMetaModel
		if err := tx.First(&meta, 1).Error; err != nil {
			return err
		}
		if meta.Version != auditJournalVersion {
			return repository.ErrAuditPendingFormat
		}
	} else {
		var tables int
		if err := tx.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'").Scan(&tables).Error; err != nil {
			return err
		}
		if tables != 0 {
			return errors.New("audit journal path contains a different database")
		}
	}
	if err := tx.Transaction(func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(&auditPendingModel{}, &auditPendingMetaModel{}); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&auditPendingMetaModel{ID: 1, Version: auditJournalVersion}).Error
	}); err != nil {
		return err
	}
	// Reconcile persisted counters once on open, including an interrupted
	// acknowledgement or a database recovered from its WAL.
	if err := tx.Transaction(func(tx *gorm.DB) error {
		var totals struct {
			Records  int
			Rejected int
			Bytes    int64
		}
		if err := tx.Model(&auditPendingModel{}).Select("COUNT(*) AS records, COALESCE(SUM(CASE WHEN rejected THEN 1 ELSE 0 END), 0) AS rejected, COALESCE(SUM(size), 0) AS bytes").Scan(&totals).Error; err != nil {
			return err
		}
		return tx.Model(&auditPendingMetaModel{}).Where("id = 1").Updates(map[string]any{"records": totals.Records, "rejected": totals.Rejected, "bytes": totals.Bytes, "revision": gorm.Expr("revision + 1")}).Error
	}); err != nil {
		return err
	}
	return j.refreshSnapshot(ctx)
}

func (j *AuditJournal) Append(ctx context.Context, value audit.Record) (repository.AuditPendingEntry, error) {
	var entry repository.AuditPendingEntry
	if value.EventID == "" {
		return entry, errors.New("audit pending identity is required")
	}
	payload, err := json.Marshal(auditPendingPayload{Version: auditJournalVersion, Record: value})
	if err != nil {
		return entry, err
	}
	size := int64(len(payload))
	if size > j.options.MaxBytes {
		return entry, repository.ErrAuditPendingFull
	}
	var meta auditPendingMetaModel
	err = j.database.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&meta, 1).Error; err != nil {
			return err
		}
		var existing auditPendingModel
		if err := tx.Where("event_id = ?", value.EventID).First(&existing).Error; err == nil {
			if existing.ClientKeyID != value.ClientKeyID {
				return repository.ErrConflict
			}
			entry = decodeAuditPending(existing)
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if meta.Records >= j.options.MaxRecords || size > j.options.MaxBytes-meta.Bytes {
			return repository.ErrAuditPendingFull
		}
		row := auditPendingModel{EventID: value.EventID, ClientKeyID: value.ClientKeyID, Payload: payload, Size: size, CreatedAt: time.Now().UTC()}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		meta.Records++
		meta.Bytes += size
		meta.Revision++
		if err := tx.Save(&meta).Error; err != nil {
			return err
		}
		entry = repository.AuditPendingEntry{ID: row.ID, Record: value}
		return nil
	})
	if err == nil {
		j.cacheSnapshot(meta)
	}
	return entry, err
}

func (j *AuditJournal) ReadPending(ctx context.Context, after uint64, limit int) ([]repository.AuditPendingEntry, error) {
	var rows []auditPendingModel
	if err := j.database.db.WithContext(ctx).Where("id > ? AND rejected = ?", after, false).Order("id").Limit(auditJournalReadLimit(limit)).Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := make([]repository.AuditPendingEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, decodeAuditPending(row))
	}
	return entries, j.refreshSnapshot(ctx)
}

func (j *AuditJournal) PendingEventIDs(ctx context.Context, after uint64, limit int) ([]repository.AuditPendingEntry, error) {
	var rows []auditPendingModel
	if err := j.database.db.WithContext(ctx).Select("id", "event_id", "client_key_id", "rejected").Where("id > ?", after).Order("id").Limit(auditJournalReadLimit(limit)).Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := make([]repository.AuditPendingEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, repository.AuditPendingEntry{ID: row.ID, Record: audit.Record{EventID: row.EventID, ClientKeyID: row.ClientKeyID}, Rejected: row.Rejected})
	}
	return entries, nil
}

func decodeAuditPending(row auditPendingModel) repository.AuditPendingEntry {
	entry := repository.AuditPendingEntry{ID: row.ID, Record: audit.Record{EventID: row.EventID, ClientKeyID: row.ClientKeyID}, Rejected: row.Rejected}
	var value auditPendingPayload
	if err := json.Unmarshal(row.Payload, &value); err != nil {
		entry.DecodeError = fmt.Errorf("decode pending audit: %w", err)
	} else if value.Version != auditJournalVersion {
		entry.DecodeError = repository.ErrAuditPendingFormat
	} else if value.Record.EventID != row.EventID || value.Record.ClientKeyID != row.ClientKeyID {
		entry.DecodeError = errors.New("pending audit payload identity mismatch")
	} else {
		entry.Record = value.Record
	}
	return entry
}

func auditJournalReadLimit(limit int) int {
	if limit < 1 || limit > 4096 {
		return 256
	}
	return limit
}

func (j *AuditJournal) Acknowledge(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	var meta auditPendingMetaModel
	err := j.database.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&meta, 1).Error; err != nil {
			return err
		}
		for start := 0; start < len(ids); start += auditLookupBatchSize {
			end := min(start+auditLookupBatchSize, len(ids))
			var rows []auditPendingModel
			if err := tx.Select("id", "size", "rejected").Where("id IN ?", ids[start:end]).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				continue
			}
			result := tx.Where("id IN ?", ids[start:end]).Delete(&auditPendingModel{})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != int64(len(rows)) {
				return repository.ErrConflict
			}
			for _, row := range rows {
				meta.Records--
				meta.Bytes -= row.Size
				if row.Rejected {
					meta.Rejected--
				}
			}
		}
		meta.Revision++
		return tx.Save(&meta).Error
	})
	if err == nil {
		j.cacheSnapshot(meta)
	}
	return err
}

func (j *AuditJournal) Reject(ctx context.Context, id uint64, reason string) error {
	var meta auditPendingMetaModel
	err := j.database.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&meta, 1).Error; err != nil {
			return err
		}
		result := tx.Model(&auditPendingModel{}).Where("id = ? AND rejected = ?", id, false).Updates(map[string]any{"rejected": true, "reason": truncate(reason, 128)})
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		meta.Rejected++
		meta.Revision++
		return tx.Save(&meta).Error
	})
	if err == nil {
		j.cacheSnapshot(meta)
	}
	return err
}

// RetryRejected is invoked once at writer startup, allowing a repaired binary
// or storage schema to retry retained facts. Persistent invalidity parks again.
func (j *AuditJournal) RetryRejected(ctx context.Context) error {
	var meta auditPendingMetaModel
	err := j.database.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&meta, 1).Error; err != nil {
			return err
		}
		if meta.Rejected == 0 {
			return nil
		}
		if err := tx.Model(&auditPendingModel{}).Where("rejected = ?", true).Updates(map[string]any{"rejected": false, "reason": ""}).Error; err != nil {
			return err
		}
		meta.Rejected = 0
		meta.Revision++
		return tx.Save(&meta).Error
	})
	if err == nil {
		j.cacheSnapshot(meta)
	}
	return err
}

func (j *AuditJournal) refreshSnapshot(ctx context.Context) error {
	var meta auditPendingMetaModel
	if err := j.database.db.WithContext(ctx).First(&meta, 1).Error; err != nil {
		return err
	}
	j.cacheSnapshot(meta)
	return nil
}

func (j *AuditJournal) cacheSnapshot(meta auditPendingMetaModel) {
	next := &repository.AuditPendingSnapshot{Revision: meta.Revision, Records: meta.Records, Rejected: meta.Rejected, Bytes: meta.Bytes, MaxRecords: j.options.MaxRecords, MaxBytes: j.options.MaxBytes}
	for {
		before := j.state.Load()
		if before != nil && before.Revision >= next.Revision {
			return
		}
		if j.state.CompareAndSwap(before, next) {
			return
		}
	}
}

func (j *AuditJournal) Snapshot() repository.AuditPendingSnapshot {
	if value := j.state.Load(); value != nil {
		return *value
	}
	return repository.AuditPendingSnapshot{MaxRecords: j.options.MaxRecords, MaxBytes: j.options.MaxBytes}
}

func (j *AuditJournal) Close() error {
	j.close.Do(func() {
		j.closeErr = errors.Join(j.database.Close(), j.lock.Close())
	})
	return j.closeErr
}
