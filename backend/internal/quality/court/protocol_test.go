package court

import (
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

func experimentIdentity(id string, accountID, nodeID, epoch uint64) attemptmeta.Identity {
	return attemptmeta.Identity{ID: id, AccountID: accountID, Provider: "grok_build", Model: "grok-4.6", Revision: 1, RuleVersion: "r1", Path: attemptmeta.Path{NodeID: nodeID, Epoch: epoch, Status: attemptmeta.PathRegistered}}
}
