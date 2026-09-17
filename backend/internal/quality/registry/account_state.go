package registry

import (
	"context"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm/clause"
)

// AccountEligible is the single quality-axis scheduling predicate. The base
// account repository still applies enabled/auth/quota/concurrency checks.
func (r *Registry) AccountEligible(accountID uint64) bool {
	return r.accountState(accountID).State.Schedulable()
}

// AccountState returns the quality state; an absent row means active.
func (r *Registry) AccountState(accountID uint64) model.AccountEntry {
	return r.accountState(accountID)
}

func (r *Registry) accountState(accountID uint64) model.AccountEntry {
	snap := r.snapshot.load()
	if entry, ok := snap.accounts[accountID]; ok {
		return entry
	}
	return model.AccountEntry{State: model.AccountActive}
}

// TransitionAccount persists a direct-loop account state before publishing
// the immutable cache snapshot. Every non-active state is case-backed.
func (r *Registry) TransitionAccount(ctx context.Context, req model.AccountTransitionRequest) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.TransitionAccount(ctx, req) })
	}
	if req.To != model.AccountActive && req.CaseID == 0 {
		return ErrCaseRequired
	}
	r.transitionMu <- struct{}{}
	defer func() { <-r.transitionMu }()

	snap := r.snapshot.load()
	current := model.AccountEntry{State: model.AccountActive}
	hasRow := false
	if entry, ok := snap.accounts[req.AccountID]; ok {
		current, hasRow = entry, true
	}
	if !model.CanTransitionAccount(current.State, req.To) {
		return fmt.Errorf("%w: %s → %s (account %d)", ErrIllegalTransition, current.State, req.To, req.AccountID)
	}
	if req.To == model.AccountActive {
		if !hasRow {
			return nil
		}
		if err := r.db.WithContext(ctx).Delete(&qAccountStateModel{}, "account_id = ?", req.AccountID).Error; err != nil {
			return err
		}
		next := snap.clone()
		delete(next.accounts, req.AccountID)
		r.snapshot.store(next)
		return nil
	}

	now := time.Now().UTC()
	row := qAccountStateModel{
		AccountID: req.AccountID, State: string(req.To), StateSince: now,
		CurrentCaseID: req.CaseID, UpdatedAt: now,
	}
	if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"state", "state_since", "current_case_id", "updated_at"}),
	}).Create(&row).Error; err != nil {
		return err
	}
	next := snap.clone()
	next.accounts[req.AccountID] = model.AccountEntry{
		State: req.To, StateSince: now, CurrentCaseID: req.CaseID,
	}
	r.snapshot.store(next)
	return nil
}

// ReleaseAccountErase releases an account hold. It is retained as a narrow
// compatibility name for callers that explicitly perform an acquittal; it
// does not represent a second state machine.
func (r *Registry) ReleaseAccountErase(ctx context.Context, accountID uint64) error {
	return r.TransitionAccount(ctx, model.AccountTransitionRequest{AccountID: accountID, To: model.AccountActive})
}

// ReleaseAccountIfUnheld removes a temporary hold after its case party has
// been released and no other investigating case still holds the account.
func (r *Registry) ReleaseAccountIfUnheld(ctx context.Context, accountID uint64) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.ReleaseAccountIfUnheld(ctx, accountID) })
	}
	if accountID == 0 {
		return nil
	}
	r.transitionMu <- struct{}{}
	defer func() { <-r.transitionMu }()

	snap := r.snapshot.load()
	entry, ok := snap.accounts[accountID]
	if !ok || entry.State != model.AccountRemanded {
		return nil
	}
	var holders []struct{ CaseID uint64 }
	if err := r.db.WithContext(ctx).Table("q_case_party AS party").
		Select("party.case_id AS case_id").
		Joins("JOIN q_case AS cases ON cases.id = party.case_id").
		Where("party.kind = ? AND party.account_id = ? AND party.disposition = ? AND cases.status = ?",
			string(model.PartyAccount), accountID, string(model.DispositionRemanded),
			string(model.CaseInvestigating)).
		Order("party.case_id DESC").Limit(1).Find(&holders).Error; err != nil {
		return err
	}
	if len(holders) > 0 {
		// An account can be held by multiple independent incidents. Keep the
		// cache's explanatory case id pointed at a surviving holder when one
		// case releases; the state itself must remain remanded.
		if entry.CurrentCaseID != holders[0].CaseID {
			res := r.db.WithContext(ctx).Model(&qAccountStateModel{}).
				Where("account_id = ? AND state = ?", accountID, string(model.AccountRemanded)).
				Update("current_case_id", holders[0].CaseID)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected > 0 {
				next := snap.clone()
				entry.CurrentCaseID = holders[0].CaseID
				next.accounts[accountID] = entry
				r.snapshot.store(next)
			}
		}
		return nil
	}
	res := r.db.WithContext(ctx).Where("account_id = ? AND state = ?", accountID, string(model.AccountRemanded)).
		Delete(&qAccountStateModel{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		next := snap.clone()
		delete(next.accounts, accountID)
		r.snapshot.store(next)
	}
	return nil
}
