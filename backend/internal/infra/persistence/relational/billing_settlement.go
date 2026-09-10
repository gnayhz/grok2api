package relational

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A settlement is the permanent identity of the first committed audit. It has
// no foreign keys: deleting diagnostic details or a Key cannot reopen billing.
type billingSettlementModel struct {
	EventID     string    `gorm:"size:64;primaryKey;check:chk_billing_settlements_event_id,length(event_id) BETWEEN 16 AND 64"`
	ClientKeyID uint64    `gorm:"not null;check:chk_billing_settlements_key,client_key_id > 0"`
	Amount      int64     `gorm:"not null;check:chk_billing_settlements_amount,amount >= 0"`
	RecordedAt  time.Time `gorm:"not null"`
}

func (billingSettlementModel) TableName() string { return "billing_settlements" }

// Lock Key rows before claiming event identities. Reservation, settlement and
// cancellation share this order; independent Keys do not need a global lock.
func lockAuditClientKeys(tx *gorm.DB, prepared []preparedAudit) error {
	keys := make(map[uint64]struct{}, len(prepared))
	for _, value := range prepared {
		keys[value.row.ClientKeyID] = struct{}{}
	}
	ids := make([]uint64, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		// Audits retain their original identity even after a Key is deleted.
		if err := lockClientKey(tx, id); err != nil && !errors.Is(err, repository.ErrNotFound) {
			return err
		}
	}
	return nil
}

func claimAuditSettlements(tx *gorm.DB, prepared []preparedAudit) ([]preparedAudit, error) {
	first := make(map[string]preparedAudit, len(prepared))
	ids := make([]string, 0, len(prepared))
	for _, value := range prepared {
		id := value.row.EventID
		if original, exists := first[id]; exists {
			if original.row.ClientKeyID != value.row.ClientKeyID {
				return nil, settlementOwnerConflict(value)
			}
			continue
		}
		first[id] = value
		ids = append(ids, id)
	}
	// Keep conflicting inserts in the same order, including deleted-Key events
	// for which there is no longer a Key row available to lock.
	sort.Strings(ids)
	claimed := make(map[string]struct{}, len(ids))
	now := time.Now().UTC()
	for start := 0; start < len(ids); start += auditInsertBatchSize {
		end := min(start+auditInsertBatchSize, len(ids))
		args := make([]any, 0, 4*(end-start))
		values := make([]string, 0, end-start)
		for _, id := range ids[start:end] {
			row := first[id].row
			args = append(args, id, row.ClientKeyID, auditBillingAmount(row), now)
			values = append(values, "(?, ?, ?, ?)")
		}
		// Read RETURNING rows directly. GORM's input slice can still contain
		// unchanged candidates when ON CONFLICT skips some entries in a batch.
		rows, err := tx.Raw("INSERT INTO billing_settlements (event_id, client_key_id, amount, recorded_at) VALUES "+strings.Join(values, ",")+" ON CONFLICT (event_id) DO NOTHING RETURNING event_id", args...).Rows()
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			claimed[id] = struct{}{}
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil || closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
	}
	if len(claimed) != len(ids) {
		for start := 0; start < len(ids); start += auditLookupBatchSize {
			end := min(start+auditLookupBatchSize, len(ids))
			var stored []billingSettlementModel
			if err := tx.Select("event_id", "client_key_id").Where("event_id IN ?", ids[start:end]).Find(&stored).Error; err != nil {
				return nil, err
			}
			for _, receipt := range stored {
				if first[receipt.EventID].row.ClientKeyID != receipt.ClientKeyID {
					return nil, settlementOwnerConflict(first[receipt.EventID])
				}
			}
		}
	}
	result := make([]preparedAudit, 0, len(claimed))
	for _, id := range ids {
		if _, ok := claimed[id]; ok {
			result = append(result, first[id])
		}
	}
	// Event locks have a stable order; diagnostic rows retain the caller's
	// original order so existing ID/cursor behavior remains compatible.
	sort.Slice(result, func(i, j int) bool { return result[i].index < result[j].index })
	return result, nil
}

func settlementOwnerConflict(value preparedAudit) error {
	return &repository.InvalidBatchRecordError{Index: value.index, Err: fmt.Errorf("%w: audit event belongs to another client Key", repository.ErrConflict)}
}

// preserveAuditSettlements adopts already committed legacy rows without
// changing Key counters. It is also used inside the retention transaction.
func preserveAuditSettlements(tx *gorm.DB, query *gorm.DB) error {
	var rows []requestAuditModel
	if err := query.Select("id", "event_id", "request_id", "client_key_id", "model_route_id", "cost_in_usd_ticks", "estimated_cost_in_usd_ticks", "created_at").Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	values := make([]billingSettlementModel, 0, len(rows))
	now := time.Now().UTC()
	for _, row := range rows {
		id := row.EventID
		if id == "" {
			id = legacyAuditEventID(row.RequestID, row.ClientKeyID, row.ModelRouteID, row.CreatedAt)
		}
		values = append(values, billingSettlementModel{EventID: id, ClientKeyID: row.ClientKeyID, Amount: auditBillingAmount(row), RecordedAt: now})
	}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}}, DoNothing: true}).CreateInBatches(&values, auditInsertBatchSize).Error
}

func (d *Database) migrateBillingSettlements(ctx context.Context) error {
	// Schema migration already runs in a transaction before this instance can
	// serve traffic. Stream bounded pages; never retain all old audits in Go.
	var cursor uint64
	for {
		var ids []uint64
		if err := d.db.WithContext(ctx).Model(&requestAuditModel{}).Where("id > ?", cursor).Order("id").Limit(auditRetentionBatchSize).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		tx := d.db.WithContext(ctx)
		if err := preserveAuditSettlements(tx, tx.Model(&requestAuditModel{}).Where("id IN ?", ids)); err != nil {
			return err
		}
		cursor = ids[len(ids)-1]
	}
}
