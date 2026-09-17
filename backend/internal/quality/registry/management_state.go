package registry

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func (r *Registry) ManagementState(ctx context.Context) (model.ManagementState, error) {
	if err := r.RefreshState(ctx); err != nil {
		return model.ManagementState{}, err
	}
	snapshot := r.snapshot.load()
	state := model.ManagementState{Accounts: make(map[uint64]model.AccountEntry, len(snapshot.accounts)), Exits: make(map[model.EpochKey]model.ExitEntry, len(snapshot.exitStates))}
	for id, entry := range snapshot.accounts {
		state.Accounts[id] = entry
	}
	for key, entry := range snapshot.exitStates {
		state.Exits[key] = entry
	}
	return state, nil
}
