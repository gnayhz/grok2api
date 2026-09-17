package account

import (
	"context"
	"slices"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// QualityState is a read projection; the quality subsystem owns restriction decisions.
type QualityState struct {
	State  string
	CaseID uint64
}

// SetQualityStates installs the quality projection before serving requests.
func (s *Service) SetQualityStates(read func(context.Context) (map[uint64]QualityState, error)) {
	s.qualityStates = read
}

func (s *Service) readQualityStates(ctx context.Context) (map[uint64]QualityState, error) {
	if s.qualityStates == nil {
		return nil, nil
	}
	return s.qualityStates(ctx)
}

func qualityState(states map[uint64]QualityState, id uint64) *QualityState {
	state, ok := states[id]
	if !ok || state.State == "active" {
		return nil
	}
	return &state
}

// Restriction filtering composes with other account criteria before COUNT and pagination.
func applyQualityFilter(filter *repository.AccountListFilter, value string, states map[uint64]QualityState) {
	if value == "" {
		return
	}
	ids := make([]uint64, 0, len(states))
	for id, state := range states {
		if state.State != "active" && (value == "restricted" || value == "clear" || state.State == value) {
			ids = append(ids, id)
		}
	}
	if value == "clear" {
		filter.ExcludeIDs = append(filter.ExcludeIDs, ids...)
		return
	}
	if filter.RestrictIDs {
		existing := make(map[uint64]struct{}, len(filter.AccountIDs))
		for _, id := range filter.AccountIDs {
			existing[id] = struct{}{}
		}
		ids = slices.DeleteFunc(ids, func(id uint64) bool { _, ok := existing[id]; return !ok })
	}
	filter.AccountIDs, filter.RestrictIDs = ids, true
}

// Identities resolves a bounded selection without credential, quota, billing or audit reads.
func (s *Service) Identities(ctx context.Context, ids []uint64) ([]accountdomain.Identity, error) {
	ids, err := normalizeIDs(ids, 500)
	if err != nil {
		return nil, err
	}
	return s.accounts.ListIdentities(ctx, ids)
}
