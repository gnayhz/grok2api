package selector

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestSelectionSessionHasAvailableCandidate(t *testing.T) {
	t.Parallel()
	session := &SelectionSession{
		values: []accountdomain.RoutingCandidate{
			{Credential: accountdomain.Credential{ID: 1}},
			{Credential: accountdomain.Credential{ID: 2}},
			{Credential: accountdomain.Credential{ID: 3}},
		},
		normalCandidates: []int{0, 1},
		probeCandidates:  []int{2},
		staleCandidates:  make(map[uint64]bool),
	}
	if !session.hasAvailableCandidate(map[uint64]bool{1: true}, false) {
		t.Fatal("second normal account should be available")
	}
	if session.hasAvailableCandidate(map[uint64]bool{1: true, 2: true}, false) {
		t.Fatal("routing attempt budget must not invent another normal account")
	}
	if !session.hasAvailableCandidate(map[uint64]bool{1: true, 2: true}, true) {
		t.Fatal("quota probe should be available when allowed")
	}
	if session.hasAvailableCandidate(map[uint64]bool{1: true, 2: true, 3: true}, true) {
		t.Fatal("fully excluded session should be exhausted")
	}
}
