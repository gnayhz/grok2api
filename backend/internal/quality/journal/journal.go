// Package journal persists guard facts and their owned temporary restrictions.
// The request path commits one short transaction; evidence and court work use
// a leased, restartable outbox. Facts are never rewritten by an outbox retry.
package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrConflict = errors.New("guard event identity conflict")
var ErrBacklogFull = errors.New("guard event backlog capacity exhausted")

const DefaultBacklogLimit int64 = 10000

type EventRow struct {
	ID         string    `gorm:"size:160;primaryKey"`
	AttemptID  string    `gorm:"size:128;not null;index"`
	AccountID  uint64    `gorm:"not null;index"`
	Stage      string    `gorm:"size:16;not null"`
	Outcome    string    `gorm:"size:24;not null"`
	Payload    string    `gorm:"type:text;not null"`
	ObservedAt time.Time `gorm:"not null;index"`
}

func (EventRow) TableName() string { return "q_guard_event" }

type RestrictionRow struct {
	Owner      string    `gorm:"size:160;primaryKey"`
	AccountID  uint64    `gorm:"not null;index:idx_q_guard_hold_active,priority:1"`
	Reason     string    `gorm:"size:32;not null"`
	ExpiresAt  time.Time `gorm:"not null;index:idx_q_guard_hold_active,priority:2"`
	ReleasedAt *time.Time
}

func (RestrictionRow) TableName() string { return "q_guard_restriction" }

type OutboxRow struct {
	EventID     string    `gorm:"size:160;primaryKey"`
	ReadyAt     time.Time `gorm:"not null;index:idx_q_guard_outbox_ready"`
	LeaseUntil  *time.Time
	LeaseOwner  string     `gorm:"size:128;not null;default:''"`
	Attempts    int        `gorm:"not null;default:0"`
	ProcessedAt *time.Time `gorm:"index:idx_q_guard_outbox_processed"`
	LastError   string     `gorm:"size:32;not null;default:''"`
}

func (OutboxRow) TableName() string { return "q_guard_outbox" }

type CapacityRow struct {
	ID       uint8 `gorm:"primaryKey"`
	Pending  int64 `gorm:"not null"`
	InFlight int64 `gorm:"not null;default:0"`
	Limit    int64 `gorm:"column:capacity_limit;not null"`
}

func (CapacityRow) TableName() string { return "q_guard_capacity" }

func Models() []any {
	return []any{&EventRow{}, &RestrictionRow{}, &OutboxRow{}, &CapacityRow{}, &CompletionRow{}}
}

type Store struct {
	db          *gorm.DB
	completions *completionTracker
}

func New(db *gorm.DB) *Store { return &Store{db: db, completions: newCompletionTracker()} }

// RecordMany commits related facts under one receipt, with no nested savepoints.
// Admission write tokens track overlapping finalizers; only committed
// admissions become eligible for renewal.
func (s *Store) RecordMany(ctx context.Context, events []qualitymodel.Event) error {
	if len(events) > 128 {
		return errors.New("guard event batch too large")
	}
	s.beginCompletions(events)
	var inserted []qualitymodel.Event
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, event := range events {
			if err := s.record(tx, event, &inserted); err != nil {
				return err
			}
		}
		return nil
	})
	s.trackCompletions(events, inserted, err)
	return err
}

func (s *Store) CheckCapacity(ctx context.Context) error {
	var rows []CapacityRow
	if err := s.db.WithContext(ctx).Where("id = 1").Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) > 0 {
		if rows[0].Pending >= rows[0].Limit || rows[0].InFlight >= rows[0].Limit {
			return ErrBacklogFull
		}
		return nil
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&OutboxRow{}).Where("processed_at IS NULL").Count(&count).Error; err != nil {
		return err
	}
	if count >= DefaultBacklogLimit {
		return ErrBacklogFull
	}
	return nil
}

// Record returns a receipt only after the fact, restriction and outbox commit.
// Replaying an identical event neither extends its TTL nor recreates its hold.
func (s *Store) Record(ctx context.Context, e qualitymodel.Event) error {
	return s.RecordMany(ctx, []qualitymodel.Event{e})
}

func (s *Store) record(tx *gorm.DB, e qualitymodel.Event, inserted ...*[]qualitymodel.Event) error {
	if e.Attempt.ID == "" || len(e.Attempt.ID) > 128 || e.Attempt.AccountID == 0 || e.At.IsZero() || len(e.Rule) > 100 || len(e.ErrorCode) > 128 ||
		(e.Stage != qualitymodel.EventStageAdmission && e.Stage != qualitymodel.EventStageCompletion && e.Stage != qualitymodel.EventStageExchange && e.Stage != qualitymodel.EventStageRecovery) {
		return errors.New("invalid guard event")
	}
	if (e.Stage == qualitymodel.EventStageAdmission && e.Outcome != qualitymodel.EventOutcomeAdmitted && e.Outcome != qualitymodel.EventOutcomeDegraded && e.Outcome != qualitymodel.EventOutcomeRejected) ||
		(e.Stage == qualitymodel.EventStageCompletion && e.Outcome != qualitymodel.EventOutcomeCompleted && e.Outcome != qualitymodel.EventOutcomeInterrupted && e.Outcome != qualitymodel.EventOutcomeCanceled) ||
		(e.Stage == qualitymodel.EventStageRecovery && e.Outcome != qualitymodel.EventOutcomeUnconfirmed) ||
		(e.Stage == qualitymodel.EventStageExchange && (e.Outcome != qualitymodel.EventOutcomeObserved || e.Physical == nil || e.Physical.Attempt != e.Attempt)) {
		return errors.New("invalid guard outcome for stage")
	}
	if e.Stage == "admission" && e.Outcome == "degraded" && (!e.HoldUntil.After(e.At) || e.HoldUntil.Sub(e.At) > 24*time.Hour) {
		return errors.New("guard restriction requires a finite future expiry")
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(payload) > 8192 {
		return errors.New("guard event metadata too large")
	}
	row := EventRow{ID: e.ID(), AttemptID: e.Attempt.ID, AccountID: e.Attempt.AccountID,
		Stage: e.Stage, Outcome: e.Outcome, Payload: string(payload), ObservedAt: e.At}
	insert := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if insert.Error != nil {
		return insert.Error
	}
	if insert.RowsAffected == 0 {
		var existing EventRow
		if err := tx.First(&existing, "id = ?", row.ID).Error; err != nil {
			return err
		}
		if existing.Payload != row.Payload {
			return ErrConflict
		}
		return nil
	}
	if err := ensureCapacity(tx); err != nil {
		return err
	}
	// Only incidents require asynchronous domain processing. Archival facts
	// are already acknowledged and never compete with completion writes.
	outbox := OutboxRow{EventID: e.ID(), ReadyAt: e.At}
	if e.Stage == qualitymodel.EventStageAdmission && e.Outcome == qualitymodel.EventOutcomeDegraded {
		reservation := tx.Model(&CapacityRow{}).Where("id = 1 AND pending < capacity_limit").Update("pending", gorm.Expr("pending + 1"))
		if reservation.Error != nil {
			return reservation.Error
		}
		if reservation.RowsAffected != 1 {
			return ErrBacklogFull
		}
	} else {
		outbox.ProcessedAt = &e.At
	}
	if err := s.recordCompletionObligation(tx, e, string(payload)); err != nil {
		return err
	}
	if e.Stage == qualitymodel.EventStageAdmission && e.Outcome == qualitymodel.EventOutcomeDegraded {
		if err := tx.Create(&RestrictionRow{Owner: e.ID(), AccountID: e.Attempt.AccountID,
			Reason: "admission_rejected", ExpiresAt: e.HoldUntil}).Error; err != nil {
			return err
		}
	}
	if err := tx.Create(&outbox).Error; err != nil {
		return err
	}
	if len(inserted) > 0 {
		*inserted[0] = append(*inserted[0], e)
	}
	return nil
}

// AccountAllowed is the authoritative final admission check. One SQL snapshot
// sees both the temporary holds and the case projection across replicas.
// A hold committed after this read applies to subsequent admissions.
//
// 跨存储域显式合同:本查询联查 registry 拥有的 q_account_state/
// q_case_party 列(字段与状态值是两包的共同合同);registry 改这两张
// 表的列名或状态值时必须同步此 SQL。
func (s *Store) AccountAllowed(ctx context.Context, id uint64, now time.Time) (bool, error) {
	var blocked bool
	err := s.db.WithContext(ctx).Raw(`SELECT EXISTS(
		SELECT 1 FROM q_guard_restriction WHERE account_id = ? AND released_at IS NULL AND expires_at > ?
		UNION ALL SELECT 1 FROM q_account_state WHERE account_id = ? AND state <> 'active'
		UNION ALL SELECT 1 FROM q_case_party p JOIN q_case c ON c.id = p.case_id
		WHERE p.kind = 'account' AND p.account_id = ? AND
		(p.disposition = 'sentenced' OR (p.disposition = 'remanded' AND c.status = 'investigating'))
	)`, id, now, id, id).Scan(&blocked).Error
	return !blocked, err
}

// Release affects this event's hold only. Case and manual owners are separate.
func (s *Store) Release(ctx context.Context, owner string, now time.Time) error {
	return s.db.WithContext(ctx).Model(&RestrictionRow{}).Where("owner = ? AND released_at IS NULL", owner).
		Update("released_at", now).Error
}

type Claim struct {
	Event    qualitymodel.Event
	Owner    string
	Attempts int
}

// Claim uses compare-and-swap for SQLite and PostgreSQL. A dead worker's claim
// expires, and an old worker cannot acknowledge a replacement worker's lease.
func (s *Store) claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]Claim, error) {
	if owner == "" || lease <= 0 || limit < 1 || limit > 64 {
		return nil, errors.New("invalid outbox claim")
	}
	var candidates []OutboxRow
	ready := "processed_at IS NULL AND ready_at <= ? AND (lease_until IS NULL OR lease_until <= ?)"
	if err := s.db.WithContext(ctx).Where(ready, now, now).Order("ready_at, event_id").Limit(limit).Find(&candidates).Error; err != nil {
		return nil, err
	}
	claims := make([]Claim, 0, len(candidates))
	for _, candidate := range candidates {
		until := now.Add(lease)
		result := s.db.WithContext(ctx).Model(&OutboxRow{}).Where("event_id = ?", candidate.EventID).
			Where(ready, now, now).Updates(map[string]any{"lease_owner": owner, "lease_until": until, "attempts": gorm.Expr("attempts + 1")})
		if result.Error != nil {
			return claims, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		var row EventRow
		if err := s.db.WithContext(ctx).First(&row, "id = ?", candidate.EventID).Error; err != nil {
			return claims, err
		}
		var e qualitymodel.Event
		if err := json.Unmarshal([]byte(row.Payload), &e); err != nil {
			return claims, err
		}
		claims = append(claims, Claim{Event: e, Owner: owner, Attempts: candidate.Attempts + 1})
	}
	return claims, nil
}

func (s *Store) complete(ctx context.Context, claim Claim, now time.Time) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureCapacity(tx); err != nil {
			return err
		}
		result := tx.Model(&OutboxRow{}).Where("event_id = ? AND lease_owner = ? AND processed_at IS NULL", claim.Event.ID(), claim.Owner).
			Updates(map[string]any{"processed_at": now, "lease_until": nil, "lease_owner": "", "last_error": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("outbox claim lost")
		}
		return tx.Model(&CapacityRow{}).Where("id = 1 AND pending > 0").Update("pending", gorm.Expr("pending - 1")).Error
	})
}

// Initialization includes pre-migration pending work. The primary-key insert
// serializes the first writer across replicas; later reservations are atomic.
func ensureCapacity(tx *gorm.DB) error {
	var count int64
	if err := tx.Model(&CapacityRow{}).Where("id = 1").Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if err := tx.Model(&OutboxRow{}).Where("processed_at IS NULL").Count(&count).Error; err != nil {
		return err
	}
	var active int64
	if err := tx.Model(&CompletionRow{}).Count(&active).Error; err != nil {
		return err
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&CapacityRow{ID: 1, Pending: count, InFlight: active, Limit: DefaultBacklogLimit}).Error
}

func (s *Store) Stats(ctx context.Context) (qualitymodel.BacklogStats, error) {
	stats := qualitymodel.BacklogStats{Limit: DefaultBacklogLimit}
	db := s.db.WithContext(ctx)
	var capacity []CapacityRow
	if err := db.Where("id = 1").Find(&capacity).Error; err != nil {
		return stats, err
	}
	if len(capacity) > 0 {
		stats.Limit = capacity[0].Limit
		stats.InFlight = capacity[0].InFlight
	}
	if err := db.Model(&OutboxRow{}).Where("processed_at IS NULL").Count(&stats.Pending).Error; err != nil {
		return stats, err
	}
	var oldest []EventRow
	if err := db.Table("q_guard_event e").Select("e.*").Joins("JOIN q_guard_outbox o ON o.event_id = e.id").Where("o.processed_at IS NULL").Order("e.observed_at").Limit(1).Find(&oldest).Error; err != nil {
		return stats, err
	}
	if len(oldest) > 0 {
		stats.OldestAt = &oldest[0].ObservedAt
	}
	if err := db.Model(&OutboxRow{}).Where("processed_at IS NULL AND lease_until > ?", time.Now().UTC()).Count(&stats.Leased).Error; err != nil {
		return stats, err
	}
	if err := db.Model(&OutboxRow{}).Where("processed_at IS NULL AND attempts > 0").Count(&stats.Retrying).Error; err != nil {
		return stats, err
	}
	if err := db.Model(&EventRow{}).Where("stage = ? AND NOT EXISTS (SELECT 1 FROM q_guard_event c WHERE c.attempt_id = q_guard_event.attempt_id AND c.stage = ?)", qualitymodel.EventStageRecovery, qualitymodel.EventStageCompletion).Count(&stats.Unconfirmed).Error; err != nil {
		return stats, err
	}
	var expired []OutboxRow
	if err := db.Where("processed_at < ?", time.Now().UTC().Add(-7*24*time.Hour)).Order("processed_at").Limit(1).Find(&expired).Error; err != nil {
		return stats, err
	}
	if len(expired) > 0 {
		stats.OldestExpiredAt = expired[0].ProcessedAt
	}
	return stats, nil
}

func (s *Store) retry(ctx context.Context, claim Claim, now time.Time) error {
	backoff := time.Second * time.Duration(1<<min(claim.Attempts, 6))
	return s.db.WithContext(ctx).Model(&OutboxRow{}).
		Where("event_id = ? AND lease_owner = ? AND processed_at IS NULL", claim.Event.ID(), claim.Owner).
		Updates(map[string]any{"ready_at": now.Add(backoff), "lease_until": nil, "lease_owner": "", "last_error": "processing_failed"}).Error
}

// ProcessOne deliberately claims one item: its processing timeout must remain
// below the lease even when the downstream court is slow.
func (s *Store) ProcessOne(ctx context.Context, owner string, handle func(context.Context, qualitymodel.Event) error) (bool, error) {
	claims, err := s.claim(ctx, owner, time.Now().UTC(), 15*time.Second, 1)
	if err != nil || len(claims) == 0 {
		return false, err
	}
	claim := claims[0]
	workCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = handle(workCtx, claim.Event)
	cancel()
	if err != nil {
		if retryErr := s.retry(ctx, claim, time.Now().UTC()); retryErr != nil {
			return true, errors.Join(err, retryErr)
		}
		return true, fmt.Errorf("process guard event: %w", err)
	}
	return true, s.complete(ctx, claim, time.Now().UTC())
}
