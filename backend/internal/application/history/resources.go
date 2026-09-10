package history

import (
	"context"
	"errors"
	"fmt"
	"time"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const responseOwnershipTTL = 30 * 24 * time.Hour

// ResponseResources owns local resource identity and the necessary commit after
// an upstream deletion. Network execution and credential recovery stay in Gateway.
type ResponseResources struct{ store repository.ResponseRepository }

func NewResponseResources(store repository.ResponseRepository) *ResponseResources {
	return &ResponseResources{store: store}
}

func (r *ResponseResources) Lookup(ctx context.Context, id string, clientKeyID uint64, now time.Time) (inferencedomain.ResponseOwnership, error) {
	value, err := r.store.Get(ctx, id, clientKeyID, now.UTC())
	if errors.Is(err, repository.ErrNotFound) {
		return inferencedomain.ResponseOwnership{}, historydomain.ErrResponseNotFound
	}
	if err != nil {
		return inferencedomain.ResponseOwnership{}, fmt.Errorf("%w: %w", historydomain.ErrResponseRead, err)
	}
	return value, nil
}

func (r *ResponseResources) Record(ctx context.Context, value inferencedomain.ResponseOwnership, now time.Time) error {
	if value.ResponseID == "" {
		return errors.New("missing response ID")
	}
	now = now.UTC()
	value.ExpiresAt, value.CreatedAt, value.UpdatedAt = now.Add(responseOwnershipTTL), now, now
	return r.store.Save(ctx, value)
}

// Forget acknowledges an already absent row, including another caller's
// completed deletion. Other failures preserve the cause and the retryable ID;
// they cannot undo an upstream deletion or acknowledge local completion.
func (r *ResponseResources) Forget(ctx context.Context, id string, clientKeyID uint64) error {
	err := r.store.Delete(ctx, id, clientKeyID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", historydomain.ErrResponseDelete, err)
	}
	return nil
}

// LookupWeb and DeleteWeb own the native compatibility-state failure contract.
// A native 404 is a fact only when the store confirms absence. Unlike Forget's
// local completion acknowledgement, native Delete retains its public 404 shape.
func (r *ResponseResources) LookupWeb(ctx context.Context, id string, now time.Time) (inferencedomain.WebResponseState, error) {
	value, err := r.store.GetWebState(ctx, id, now.UTC())
	if errors.Is(err, repository.ErrNotFound) {
		return inferencedomain.WebResponseState{}, historydomain.ErrResponseNotFound
	}
	if err != nil {
		return inferencedomain.WebResponseState{}, fmt.Errorf("%w: %w", historydomain.ErrResponseRead, err)
	}
	return value, nil
}
func (r *ResponseResources) DeleteWeb(ctx context.Context, id string) error {
	err := r.store.DeleteWebState(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return historydomain.ErrResponseNotFound
	}
	if err != nil {
		return fmt.Errorf("%w: %w", historydomain.ErrResponseDelete, err)
	}
	return nil
}

func (r *ResponseResources) RecordWeb(ctx context.Context, value inferencedomain.WebResponseState, observedAt time.Time) error {
	if value.ResponseID == "" || value.ConversationID == "" || value.UpstreamParentResponseID == "" {
		return errors.New("incomplete upstream continuation identity")
	}
	observedAt = observedAt.UTC()
	value.CreatedAt, value.UpdatedAt, value.ExpiresAt = observedAt, observedAt, observedAt.Add(responseOwnershipTTL)
	return r.store.SaveWebState(ctx, value)
}
