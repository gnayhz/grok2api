package journal

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const CompletionLease = 2 * time.Minute

// CompletionRow is created in the admission transaction, before delivery. Its
// lease belongs to this process incarnation, not a reusable deployment ID.
type CompletionRow struct {
	AttemptID  string    `gorm:"size:128;primaryKey"`
	Owner      string    `gorm:"size:128;not null;index"`
	Payload    string    `gorm:"type:text;not null"`
	LeaseUntil time.Time `gorm:"not null;index"`
}

func (CompletionRow) TableName() string { return "q_guard_completion" }

type completionTracker struct {
	mu      sync.Mutex
	owner   string
	active  map[string]struct{}
	pending map[string]completionRetry
	writing map[string]*admissionWrite
}

// A write token bridges the database commit and local bookkeeping. A concurrent
// finalizer can mark it finished without waiting for the admission caller.
type admissionWrite struct {
	refs     int
	finished bool
}

type completionRetry struct {
	event     Event
	expiresAt time.Time
}

func newCompletionTracker() *completionTracker {
	return &completionTracker{owner: uuid.NewString(), active: make(map[string]struct{}), pending: make(map[string]completionRetry), writing: make(map[string]*admissionWrite)}
}

func (s *Store) recordCompletionObligation(tx *gorm.DB, e Event, payload string) error {
	if e.Stage == "admission" && e.Outcome == "delivered" {
		reservation := tx.Model(&CapacityRow{}).Where("id = 1 AND in_flight < capacity_limit").Update("in_flight", gorm.Expr("in_flight + 1"))
		if reservation.Error != nil {
			return reservation.Error
		}
		if reservation.RowsAffected != 1 {
			return ErrBacklogFull
		}
		return tx.Create(&CompletionRow{AttemptID: e.Attempt.ID, Owner: s.completions.owner, Payload: payload, LeaseUntil: time.Now().UTC().Add(CompletionLease)}).Error
	}
	if e.Stage == "completion" {
		result := tx.Where("attempt_id = ?", e.Attempt.ID).Delete(&CompletionRow{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			return tx.Model(&CapacityRow{}).Where("id = 1 AND in_flight > 0").Update("in_flight", gorm.Expr("in_flight - 1")).Error
		}
	}
	return nil
}

func (s *Store) beginCompletions(events []Event) {
	s.completions.mu.Lock()
	defer s.completions.mu.Unlock()
	for _, e := range events {
		if e.Stage != "admission" || e.Outcome != "delivered" {
			continue
		}
		write := s.completions.writing[e.Attempt.ID]
		if write == nil {
			write = &admissionWrite{}
			s.completions.writing[e.Attempt.ID] = write
		}
		write.refs++
	}
}

func (s *Store) trackCompletions(events, inserted []Event, err error) {
	s.completions.mu.Lock()
	defer s.completions.mu.Unlock()
	if err == nil {
		for _, e := range inserted {
			if e.Stage == "admission" && e.Outcome == "delivered" {
				if write := s.completions.writing[e.Attempt.ID]; write != nil && !write.finished {
					s.completions.active[e.Attempt.ID] = struct{}{}
				}
			}
		}
	}
	for _, e := range events {
		// A failed completion write must stop renewing the obligation. Recovery
		// records uncertainty if storage returns after the caller has finished.
		if e.Stage == "completion" {
			write := s.completions.writing[e.Attempt.ID]
			_, active := s.completions.active[e.Attempt.ID]
			if write != nil {
				write.finished = true
			}
			if err == nil {
				delete(s.completions.pending, e.Attempt.ID)
			} else if (active || write != nil) && len(s.completions.pending) < int(DefaultBacklogLimit) {
				s.completions.pending[e.Attempt.ID] = completionRetry{event: e, expiresAt: time.Now().UTC().Add(CompletionLease)}
			}
			delete(s.completions.active, e.Attempt.ID)
		}
	}
	for _, e := range events {
		if e.Stage != "admission" || e.Outcome != "delivered" {
			continue
		}
		write := s.completions.writing[e.Attempt.ID]
		write.refs--
		if write.refs == 0 {
			delete(s.completions.writing, e.Attempt.ID)
		}
	}
}

// RetryCompletions retains the exact fact through transient write failures.
// This bounded process-local aid complements the durable obligation: after a
// crash or prolonged failure, recovery still records uncertainty explicitly.
func (s *Store) RetryCompletions(ctx context.Context, now time.Time) error {
	s.completions.mu.Lock()
	var retries []Event
	for id, retry := range s.completions.pending {
		if !retry.expiresAt.After(now) {
			delete(s.completions.pending, id)
			continue
		}
		retries = append(retries, retry.event)
	}
	s.completions.mu.Unlock()
	var errs []error
	for _, e := range retries {
		if err := s.Record(ctx, e); err != nil {
			errs = append(errs, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

// RenewCompletions only touches locally active attempts owned by this process.
// Another instance cannot revive a crashed worker's obligations.
func (s *Store) RenewCompletions(ctx context.Context, now time.Time) error {
	s.completions.mu.Lock()
	ids := make([]string, 0, len(s.completions.active))
	for id := range s.completions.active {
		ids = append(ids, id)
	}
	s.completions.mu.Unlock()
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		if err := s.db.WithContext(ctx).Model(&CompletionRow{}).Where("owner = ? AND attempt_id IN ?", s.completions.owner, batch).
			Update("lease_until", now.Add(CompletionLease)).Error; err != nil {
			return err
		}
	}
	return nil
}

// RecoverCompletions records an immutable uncertainty fact, not a fabricated
// delivery outcome. A late real completion remains writable under its own ID.
func (s *Store) RecoverCompletions(ctx context.Context, now time.Time) error {
	for {
		var rows []CompletionRow
		if err := s.db.WithContext(ctx).Where("lease_until <= ?", now).Order("lease_until").Limit(100).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				removed := tx.Where("attempt_id = ? AND owner = ? AND lease_until <= ?", row.AttemptID, row.Owner, now).Delete(&CompletionRow{})
				if removed.Error != nil {
					return removed.Error
				}
				if removed.RowsAffected == 0 {
					return nil
				}
				var e Event
				if err := json.Unmarshal([]byte(row.Payload), &e); err != nil {
					return err
				}
				e.Stage, e.Outcome, e.Rule, e.ErrorCode, e.At, e.HoldUntil = "recovery", "unconfirmed", "", "completion_lease_expired", now, time.Time{}
				if err := s.record(tx, e); err != nil {
					return err
				}
				return tx.Model(&CapacityRow{}).Where("id = 1 AND in_flight > 0").Update("in_flight", gorm.Expr("in_flight - 1")).Error
			})
			if err != nil {
				return err
			}
		}
	}
}

func (s *Store) RunCompletionRecovery(ctx context.Context) error {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		workCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		now := time.Now().UTC()
		err := s.RenewCompletions(workCtx, now)
		if err == nil {
			err = s.RetryCompletions(workCtx, now)
		}
		if err == nil {
			err = s.RecoverCompletions(workCtx, now)
		}
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
