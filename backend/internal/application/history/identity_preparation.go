package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// authorizePriorContext never imports an ambiguous scope. A read-only snapshot
// tells us whether separating it leaves opaque context absent from this input.
// Old hashes are recomputed in memory using the current visible-item contract;
// legacy rows and generations remain unchanged.
func (r *ReasoningReplay) authorizePriorContext(ctx context.Context, model string, root map[string]json.RawMessage, raw []json.RawMessage, prefixes []string, options []historydomain.ReplayPreparation) error {
	for _, option := range options {
		if len(option.PriorKeys) == 0 || len(prefixes) == 0 {
			continue
		}
		scopes := make([]repository.JournalScope, 0, len(option.PriorKeys))
		for _, key := range option.PriorKeys {
			scopes = append(scopes, journalScope(model, key))
		}
		snapshots, err := r.journal.Inspect(ctx, scopes, r.now().UTC())
		if err != nil {
			return err
		}
		unavailable, err := priorContextUnavailable(root, raw, prefixes, snapshots)
		if err != nil {
			return err
		}
		if !unavailable {
			continue
		}
		if option.Authorizer == nil {
			return historydomain.ErrIdentityLossNotAuthorized
		}
		if err := option.Authorizer.PrepareInput(ctx, historydomain.InputPreparation{IdentityContextUnavailable: true}); err != nil {
			return err
		}
	}
	return nil
}

type inspectedTurn struct {
	turn           repository.JournalTurn
	parent         *inspectedTurn
	hash           string
	count          int
	opaque         bool
	visiting, done bool
}

func priorContextUnavailable(root map[string]json.RawMessage, raw []json.RawMessage, prefixes []string, snapshots []repository.JournalSnapshot) (bool, error) {
	candidates := make(map[int][]*inspectedTurn)
	for _, snapshot := range snapshots {
		nodes := make(map[string]*inspectedTurn, len(snapshot.Turns))
		for _, turn := range snapshot.Turns {
			nodes[turn.ID] = &inspectedTurn{turn: turn}
		}
		var visit func(*inspectedTurn) error
		visit = func(n *inspectedTurn) error {
			if n.done {
				return nil
			}
			if n.visiting {
				return fmt.Errorf("journal parent cycle")
			}
			n.visiting = true
			n.hash = journalHash("conversation-visible-v1")
			if n.turn.Parent != "" {
				n.parent = nodes[n.turn.Parent]
				if n.parent == nil {
					return historydomain.ErrHistoryMissing
				}
				if err := visit(n.parent); err != nil {
					return err
				}
				n.hash, n.count, n.opaque = n.parent.hash, n.parent.count, n.parent.opaque
			}
			consume := func(items [][]byte) error {
				for _, item := range items {
					hash, reasoning, err := CanonicalHistoryItemHash(item)
					if err != nil {
						return err
					}
					if reasoning {
						n.opaque = true
						continue
					}
					n.hash = journalHash(n.hash + hash)
					n.count++
				}
				return nil
			}
			if err := consume(n.turn.Input); err != nil {
				return err
			}
			n.turn.InputCount = n.count
			if err := consume(n.turn.Output); err != nil {
				return err
			}
			n.turn.TotalCount = n.count
			n.visiting, n.done = false, true
			return nil
		}
		for _, n := range nodes {
			if err := visit(n); err != nil {
				return false, err
			}
			if n.count == 0 || n.count > len(prefixes) || prefixes[n.count-1] != n.hash {
				continue
			}
			candidates[n.count] = append(candidates[n.count], n)
		}
	}

	// Old configuration-bound roots can contain full visible input without
	// parent links or earlier opaque. Check every matching prefix length, so
	// carrying only the last root's cipher cannot masquerade as a full history.
	// At one length, an exact client-carried candidate resolves old siblings.
	for _, group := range candidates {
		missing, covered := false, false
		for _, candidate := range group {
			var chain []repository.JournalTurn
			for n := candidate; n != nil; n = n.parent {
				chain = append(chain, n.turn)
			}
			for a, b := 0, len(chain)-1; a < b; a, b = a+1, b-1 {
				chain[a], chain[b] = chain[b], chain[a]
			}
			copyRoot := make(map[string]json.RawMessage, len(root))
			for key, value := range root {
				copyRoot[key] = value
			}
			// Never forward this proposal; it may contain another identity's opaque.
			_, restored, err := restoreJournalItems(copyRoot, raw, chain)
			if err != nil && !errors.Is(err, historydomain.ErrHistoryAmbiguous) {
				return false, err
			}
			covered = covered || err == nil && restored == 0 && candidate.opaque
			missing = missing || restored > 0 || err != nil
		}
		if missing && !covered {
			return true, nil
		}
	}
	return false, nil
}

// The optional legacy cache has no durable lineage. Its existing anchor matcher
// can still establish missing cached context, without injecting its proposal or
// turning a failed old-scope read into a cache miss.
func (r *ReasoningReplay) authorizePriorCache(ctx context.Context, model, key string, body []byte, options []historydomain.ReplayPreparation) error {
	if !r.Enabled() || previousResponseIDPresent(body) {
		return nil
	}
	hasPrior := false
	for _, option := range options {
		hasPrior = hasPrior || len(option.PriorKeys) > 0
	}
	if !hasPrior {
		return nil
	}
	cfg := r.cfg.Load()
	current, found, err := r.store.Get(ctx, model, key, r.now().UTC(), cfg.TTL)
	if err != nil {
		return err
	}
	if found {
		var input struct {
			Input []map[string]json.RawMessage `json:"input"`
		}
		if json.Unmarshal(body, &input) == nil {
			for _, turn := range groupReplayTurns(current) {
				if turn.anchorIndex(input.Input, 0) >= 0 {
					return nil
				}
			}
		}
	}
	for _, option := range options {
		for _, key := range option.PriorKeys {
			if key == "" {
				continue
			}
			items, found, err := r.store.Get(ctx, model, key, r.now().UTC(), cfg.TTL)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			normalized, valid := normalizeReplayItems(items)
			if !valid {
				continue
			}
			_, changed := insertReplayItems(body, normalized)
			if !changed {
				continue
			}
			if option.Authorizer == nil {
				return historydomain.ErrIdentityLossNotAuthorized
			}
			if err := option.Authorizer.PrepareInput(ctx, historydomain.InputPreparation{IdentityContextUnavailable: true}); err != nil {
				return err
			}
			break
		}
	}
	return nil
}
