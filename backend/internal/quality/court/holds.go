package court

import (
	"context"
	"encoding/json"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func (s *Service) reassertInvestigationHolds(ctx context.Context, defendant uint64, exit model.EpochKey, caseID uint64) error {
	parties, err := s.registry.ListParties(ctx, caseID)
	if err != nil {
		return err
	}
	accountHeld, exitHeld := false, false
	for _, p := range parties {
		if p.Disposition != model.DispositionRemanded {
			continue
		}
		accountHeld = accountHeld || (p.Kind == model.PartyAccount && p.AccountID == defendant)
		exitHeld = exitHeld || (p.Kind == model.PartyExit && p.NodeID == exit.NodeID && p.Epoch == exit.Epoch)
	}
	account := s.registry.AccountState(defendant)
	if accountHeld {
		switch account.State {
		case model.AccountActive:
			if err := s.registry.TransitionAccount(ctx, model.AccountTransitionRequest{
				AccountID: defendant, To: model.AccountRemanded, CaseID: caseID,
			}); err != nil {
				return err
			}
		case model.AccountRemanded, model.AccountSentenced:
			// A serving account can be a defendant in a later case, but is not
			// remanded a second time. The party record remains evidence-linked.
		}
	}
	if !exitHeld || exit.NodeID == 0 {
		return nil
	}
	if s.registry.CurrentEpoch(exit.NodeID) != exit.Epoch {
		return nil
	}
	entry := s.registry.ExitStateOfCurrentEpoch(exit.NodeID)
	switch entry.State {
	case model.ExitAvailable:
		return s.registry.TransitionExit(ctx, model.ExitTransitionRequest{
			NodeID: exit.NodeID, Epoch: exit.Epoch, To: model.ExitRemanded, CaseID: caseID,
		})
	case model.ExitRemanded:
		return nil
	case model.ExitBanned:
		return s.registry.UpdatePartyDisposition(ctx, caseID, model.PartyExit, 0,
			exit.NodeID, exit.Epoch, model.DispositionReleased)
	default:
		return nil
	}
}

// reassertExistingHolds reasserts the hold for a repeated observation of
// the same incident. OpenCaseForIncident guarantees that this case already
// owns the exact baseline exit; a different exit is deliberately a new case.
func (s *Service) reassertExistingHolds(ctx context.Context, caseID, defendant uint64, exit model.EpochKey) error {
	return s.reassertInvestigationHolds(ctx, defendant, exit, caseID)
}

func caseDefendant(parties []model.PartyRecord) uint64 {
	for _, party := range parties {
		if party.Kind == model.PartyAccount && party.Role == model.RoleDefendant {
			return party.AccountID
		}
	}
	return 0
}

func partyBaseline(parties []model.PartyRecord) model.EpochKey {
	for _, party := range parties {
		if party.Kind == model.PartyExit && party.NodeID != 0 {
			return model.EpochKey{NodeID: party.NodeID, Epoch: party.Epoch}
		}
	}
	return model.EpochKey{}
}

func caseBaseline(record model.CaseRecord, parties []model.PartyRecord) model.EpochKey {
	if record.EvidenceJSON != "" {
		var opening struct {
			Exit *struct {
				Node  uint64 `json:"node"`
				Epoch uint64 `json:"epoch"`
			} `json:"exit"`
		}
		if err := json.Unmarshal([]byte(record.EvidenceJSON), &opening); err == nil && opening.Exit != nil {
			return model.EpochKey{NodeID: opening.Exit.Node, Epoch: opening.Exit.Epoch}
		}
	}
	return partyBaseline(parties)
}
