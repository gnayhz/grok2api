package registry

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// OpenInvestigation commits the case, both parties and both scheduling holds
// together. The scheduler sees a single immutable snapshot after commit.
func (r *Registry) OpenInvestigation(ctx context.Context, accountID uint64, exit model.EpochKey, now time.Time, evidence string) (uint64, error) {
	if !r.inTransition {
		var id uint64
		err := r.withTransition(ctx, func(w *Registry) error {
			var err error
			id, err = w.OpenInvestigation(ctx, accountID, exit, now, evidence)
			return err
		})
		return id, err
	}
	if existing, err := r.OpenCaseForIncident(ctx, accountID, exit); err != nil || existing != 0 {
		return existing, err
	}

	select {
	case r.transitionMu <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-r.transitionMu }()
	next := r.snapshot.load().clone()
	var id uint64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := qCaseModel{Status: string(model.CaseInvestigating), EvidenceJSON: evidence, OpenedAt: now, UpdatedAt: now}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		id = row.ID
		party := qCasePartyModel{CaseID: id, Kind: string(model.PartyAccount), AccountID: accountID,
			Role: string(model.RoleDefendant), Disposition: string(model.DispositionRemanded), UpdatedAt: now}
		if entry := next.accounts[accountID]; entry.State == model.AccountSentenced {
			party.Disposition = string(model.DispositionSentenced)
		} else {
			state := qAccountStateModel{AccountID: accountID, State: string(model.AccountRemanded), StateSince: now, CurrentCaseID: id, UpdatedAt: now}
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&state).Error; err != nil {
				return err
			}
			next.accounts[accountID] = model.AccountEntry{State: model.AccountRemanded, StateSince: now, CurrentCaseID: id}
		}
		if err := tx.Create(&party).Error; err != nil {
			return err
		}
		if exit.NodeID == 0 {
			return nil
		}
		exitParty := qCasePartyModel{CaseID: id, Kind: string(model.PartyExit), NodeID: exit.NodeID, Epoch: exit.Epoch,
			Role: string(model.RoleCoRemanded), Disposition: string(model.DispositionRemanded), UpdatedAt: now}
		if next.nodeEpoch[exit.NodeID] != exit.Epoch {
			exitParty.Disposition = string(model.DispositionWithdrawn)
		} else if next.exitStates[exit].State == model.ExitBanned {
			exitParty.Disposition = string(model.DispositionSentenced)
		} else {
			state := qExitStateModel{NodeID: exit.NodeID, Epoch: exit.Epoch, State: string(model.ExitRemanded), StateSince: now, CurrentCaseID: id, UpdatedAt: now}
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&state).Error; err != nil {
				return err
			}
			next.exitStates[exit] = model.ExitEntry{State: model.ExitRemanded, StateSince: now, CurrentCaseID: id}
		}
		return tx.Create(&exitParty).Error
	})
	if err == nil {
		r.snapshot.store(next)
	}
	return id, err
}

// SettleInvestigation is the only automatic closing operation. A crash or
// failed write cannot publish half a verdict or release half an experiment.
// Replays are no-ops, including late worker results racing with settlement.
func (r *Registry) SettleInvestigation(ctx context.Context, caseID uint64, verdict model.Verdict, evidence string, now time.Time, banExit bool, manualRelease ...bool) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error {
			return w.SettleInvestigation(ctx, caseID, verdict, evidence, now, banExit, manualRelease...)
		})
	}
	manual := len(manualRelease) > 0 && manualRelease[0]
	// 判决到案件终态映射由领域纯规则唯一解释；事务在锁内调用。
	status, statusErr := model.CaseStatusForVerdict(verdict)
	if statusErr != nil {
		return statusErr
	}
	select {
	case r.transitionMu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.transitionMu }()
	next := r.snapshot.load().clone()
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&qCaseModel{}).Where("id = ?", caseID)
		if !manual {
			query = query.Where("status = ?", string(model.CaseInvestigating))
		}
		res := query.Updates(map[string]any{
			"status": string(status), "verdict": string(verdict), "evidence_json": evidence, "closed_at": now, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		var parties []qCasePartyModel
		if err := tx.Where("case_id = ?", caseID).Find(&parties).Error; err != nil {
			return err
		}
		for _, party := range parties {
			// Manual review revokes this case's restriction only. A separate
			// conviction or open investigation retains its own protection.
			if manual {
				if party.Kind == "account" && next.accounts[party.AccountID].CurrentCaseID == caseID && next.accounts[party.AccountID].State == model.AccountSentenced {
					var holder struct{ CaseID uint64 }
					if err := tx.Table("q_case_party p").Select("p.case_id").Joins("JOIN q_case c ON c.id=p.case_id").Where("p.kind='account' AND p.account_id=? AND p.case_id<>? AND p.disposition='sentenced' AND c.status IN ('account_guilty','both_guilty')", party.AccountID, caseID).Limit(1).Scan(&holder).Error; err != nil {
						return err
					}
					entry := next.accounts[party.AccountID]
					entry.State, entry.CurrentCaseID = model.ReviewReleaseAccount(holder.CaseID, entry.CurrentCaseID)
					next.accounts[party.AccountID] = entry
					if err := tx.Model(&qAccountStateModel{}).Where("account_id=?", party.AccountID).Updates(map[string]any{"state": string(entry.State), "current_case_id": entry.CurrentCaseID}).Error; err != nil {
						return err
					}
				}
				key := model.EpochKey{NodeID: party.NodeID, Epoch: party.Epoch}
				if party.Kind == "exit" && next.exitStates[key].CurrentCaseID == caseID && next.exitStates[key].State == model.ExitBanned {
					var holder struct{ CaseID uint64 }
					if err := tx.Table("q_case_party p").Select("p.case_id").Joins("JOIN q_case c ON c.id=p.case_id").Where("p.kind='exit' AND p.node_id=? AND p.epoch=? AND p.case_id<>? AND p.disposition='sentenced' AND c.status IN ('exit_guilty','both_guilty')", party.NodeID, party.Epoch, caseID).Limit(1).Scan(&holder).Error; err != nil {
						return err
					}
					entry := next.exitStates[key]
					entry.State, entry.CurrentCaseID = model.ReviewReleaseExit(holder.CaseID, entry.CurrentCaseID)
					next.exitStates[key] = entry
					if err := tx.Model(&qExitStateModel{}).Where("node_id=? AND epoch=?", party.NodeID, party.Epoch).Updates(map[string]any{"state": string(entry.State), "current_case_id": entry.CurrentCaseID}).Error; err != nil {
						return err
					}
				}
			}

			disposition := model.DispositionReleased
			key := model.EpochKey{NodeID: party.NodeID, Epoch: party.Epoch}
			if party.Kind == string(model.PartyAccount) && verdict.RestrictsAccount() {
				disposition = model.DispositionSentenced
			}
			if party.Kind == string(model.PartyExit) {
				if next.nodeEpoch[party.NodeID] != party.Epoch || party.Disposition == string(model.DispositionWithdrawn) {
					disposition = model.DispositionWithdrawn
				} else if party.ReviewReleased {
					disposition = model.DispositionReleased
				} else if verdict.RestrictsExit() {
					disposition = model.DispositionSentenced
					if !banExit {
						disposition = model.DispositionRemanded
					}
				}
			}
			if err := tx.Model(&qCasePartyModel{}).Where("id = ?", party.ID).Updates(map[string]any{"disposition": string(disposition), "updated_at": now}).Error; err != nil {
				return err
			}
			if party.Kind == string(model.PartyAccount) {
				if disposition == model.DispositionSentenced {
					row := qAccountStateModel{AccountID: party.AccountID, State: string(model.AccountSentenced), StateSince: now, CurrentCaseID: caseID, UpdatedAt: now}
					if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
						return err
					}
					next.accounts[party.AccountID] = model.AccountEntry{State: model.AccountSentenced, StateSince: now, CurrentCaseID: caseID}
				} else if next.accounts[party.AccountID].State != model.AccountSentenced {
					if err := releaseUnheldAccount(tx, next, party.AccountID); err != nil {
						return err
					}
				}
			} else if next.nodeEpoch[party.NodeID] == party.Epoch {
				if verdict.RestrictsExit() && (disposition == model.DispositionSentenced || disposition == model.DispositionRemanded) {
					state := model.ExitRemanded
					if banExit {
						state = model.ExitBanned
					}
					row := qExitStateModel{NodeID: party.NodeID, Epoch: party.Epoch, State: string(state), StateSince: now, CurrentCaseID: caseID, UpdatedAt: now}
					if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
						return err
					}
					next.exitStates[key] = model.ExitEntry{State: state, StateSince: now, CurrentCaseID: caseID}
				} else if next.exitStates[key].State != model.ExitBanned {
					if err := releaseUnheldExit(tx, next, key); err != nil {
						return err
					}
				}
			}
		}
		if err := recordIncidentClosures(tx, &caseID); err != nil {
			return err
		}
		return tx.Model(&qProbeTaskModel{}).Where("case_id = ? AND state IN ?", caseID, []string{"pending", "running"}).
			Updates(map[string]any{"state": "cancelled", "detail": "case_closed", "finished_at": now, "updated_at": now}).Error
	})
	if err == nil {
		r.snapshot.store(next)
	}
	return err
}

func (r *Registry) UpdateInvestigationEvidence(ctx context.Context, caseID uint64, evidence string) error {
	return r.withTransition(ctx, func(w *Registry) error {
		return w.db.WithContext(ctx).Model(&qCaseModel{}).Where("id = ? AND status = ?", caseID, "investigating").Update("evidence_json", evidence).Error
	})
}
