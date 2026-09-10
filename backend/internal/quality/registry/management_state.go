package registry

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// ManagementState is one immutable quality-state revision for a management
// projection. Exit states retain their epoch key; a current-node projection
// cannot describe the historical columns of an evidence matrix.
type ManagementState struct {
	Accounts map[uint64]AccountEntry
	Exits    map[model.EpochKey]ExitEntry
}

func (r *Registry) ManagementState(ctx context.Context) (ManagementState, error) {
	if err := r.RefreshState(ctx); err != nil {
		return ManagementState{}, err
	}
	snapshot := r.snapshot.load()
	state := ManagementState{Accounts: make(map[uint64]AccountEntry, len(snapshot.accounts)), Exits: make(map[model.EpochKey]ExitEntry, len(snapshot.exitStates))}
	for id, entry := range snapshot.accounts {
		state.Accounts[id] = entry
	}
	for key, entry := range snapshot.exitStates {
		state.Exits[key] = entry
	}
	return state, nil
}
