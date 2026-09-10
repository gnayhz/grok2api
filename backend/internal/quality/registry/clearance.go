package registry

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
)

// ReleaseClearedParties commits per-party exoneration and its explanation
// without closing the experiment or cancelling the other group's tasks.
// State projection is shared with final settlement. Replays are no-ops and
// another case's remand or conviction always remains authoritative.
func (r *Registry) ReleaseClearedParties(ctx context.Context, caseID uint64, account, exit bool, evidence string, now time.Time) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.ReleaseClearedParties(ctx, caseID, account, exit, evidence, now) })
	}
	if !account && !exit {
		return nil
	}
	select {
	case r.transitionMu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.transitionMu }()
	next := r.snapshot.load().clone()
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var c qCaseModel
		if err := tx.First(&c, caseID).Error; err != nil {
			return err
		}
		if c.Status != string(model.CaseInvestigating) {
			return nil
		}
		kinds := []string{}
		if account {
			kinds = append(kinds, string(model.PartyAccount))
		}
		if exit {
			kinds = append(kinds, string(model.PartyExit))
		}
		var parties []qCasePartyModel
		if err := tx.Where("case_id = ? AND kind IN ? AND disposition = ?", caseID, kinds, string(model.DispositionRemanded)).Find(&parties).Error; err != nil {
			return err
		}
		if len(parties) == 0 {
			return nil
		}
		for _, party := range parties {
			if err := tx.Model(&qCasePartyModel{}).Where("id = ?", party.ID).Updates(map[string]any{"disposition": string(model.DispositionReleased), "updated_at": now}).Error; err != nil {
				return err
			}
			if party.Kind == string(model.PartyAccount) {
				if err := releaseUnheldAccount(tx, next, party.AccountID); err != nil {
					return err
				}
			} else {
				if err := releaseUnheldExit(tx, next, model.EpochKey{NodeID: party.NodeID, Epoch: party.Epoch}); err != nil {
					return err
				}
			}
		}
		return tx.Model(&qCaseModel{}).Where("id = ?", caseID).Updates(map[string]any{"evidence_json": evidence, "updated_at": now}).Error
	})
	if err == nil {
		r.snapshot.store(next)
	}
	return err
}

func releaseUnheldAccount(tx *gorm.DB, next *cacheSnapshot, accountID uint64) error {
	if next.accounts[accountID].State == model.AccountSentenced {
		return nil
	}
	var holder struct{ CaseID uint64 }
	if err := tx.Table("q_case_party p").Select("p.case_id").Joins("JOIN q_case c ON c.id=p.case_id").
		Where("p.kind = 'account' AND p.account_id = ? AND p.disposition = 'remanded' AND c.status = 'investigating'", accountID).
		Order("p.case_id").Limit(1).Scan(&holder).Error; err != nil {
		return err
	}
	if holder.CaseID == 0 {
		if err := tx.Delete(&qAccountStateModel{}, "account_id = ?", accountID).Error; err != nil {
			return err
		}
		delete(next.accounts, accountID)
	} else {
		if err := tx.Model(&qAccountStateModel{}).Where("account_id = ?", accountID).Update("current_case_id", holder.CaseID).Error; err != nil {
			return err
		}
		entry := next.accounts[accountID]
		entry.CurrentCaseID = holder.CaseID
		next.accounts[accountID] = entry
	}
	return nil
}

func releaseUnheldExit(tx *gorm.DB, next *cacheSnapshot, key model.EpochKey) error {
	if next.nodeEpoch[key.NodeID] != key.Epoch || next.exitStates[key].State == model.ExitBanned {
		return nil
	}
	var holder struct{ CaseID uint64 }
	if err := tx.Table("q_case_party p").Select("p.case_id").Joins("JOIN q_case c ON c.id=p.case_id").
		Where("p.kind = 'exit' AND p.node_id = ? AND p.epoch = ? AND p.disposition = 'remanded' AND c.status IN ('investigating','exit_guilty')", key.NodeID, key.Epoch).
		Order("p.case_id").Limit(1).Scan(&holder).Error; err != nil {
		return err
	}
	if holder.CaseID == 0 {
		if err := tx.Delete(&qExitStateModel{}, "node_id = ? AND epoch = ?", key.NodeID, key.Epoch).Error; err != nil {
			return err
		}
		delete(next.exitStates, key)
	} else {
		if err := tx.Model(&qExitStateModel{}).Where("node_id = ? AND epoch = ?", key.NodeID, key.Epoch).Update("current_case_id", holder.CaseID).Error; err != nil {
			return err
		}
		entry := next.exitStates[key]
		entry.CurrentCaseID = holder.CaseID
		next.exitStates[key] = entry
	}
	return nil
}
