package web

import (
	"context"
	"errors"
	"sync"
	"time"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// responseStateCommit retains the native continuation identity and response
// resource until the gateway acknowledges the required completion barrier.
// Reading, converting or closing an upstream body does not authorize this write.
type responseStateCommit struct {
	mu        sync.Mutex
	store     historydomain.NativeResponseState
	budget    *responsebuffer.Budget
	retention *responsebuffer.Lease
	value     inferencedomain.WebResponseState
	ready     bool
	discarded bool
	attempted bool
	err       error
}

func (a *Adapter) pendingResponseState(ctx context.Context, operation string, store *bool) *responseStateCommit {
	if operation != conversation.OperationResponses || !historydomain.ResponseStorageRequested(store) {
		return nil
	}
	return &responseStateCommit{store: a.states, budget: responsebuffer.FromContext(ctx)}
}

func (p *responseStateCommit) prepare(accountID uint64, responseID string, parsed parsedChat, data []byte) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discarded {
		return context.Canceled
	}
	if p.store == nil {
		return nil // Commit reports the missing authority without retaining bytes.
	}
	retention, err := p.budget.Reserve(len(data))
	if err != nil {
		return err
	}
	p.retention.Release()
	p.retention = retention
	now := time.Now().UTC()
	p.value = inferencedomain.WebResponseState{ResponseID: responseID, AccountID: accountID,
		ConversationID: parsed.ConversationID, UpstreamParentResponseID: parsed.ParentID,
		ResponseJSON: string(data), Status: "completed", CreatedAt: now}
	p.ready = true
	return nil
}

func (p *responseStateCommit) commit(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempted {
		return p.err
	}
	p.attempted = true
	switch {
	case p.discarded || p.store == nil || !p.ready:
		p.err = errors.New("response state unavailable")
	default:
		p.err = p.store.RecordWeb(ctx, p.value, p.value.CreatedAt)
	}
	return p.err
}

func (p *responseStateCommit) discard() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.discarded = true
	p.value = inferencedomain.WebResponseState{}
	p.retention.Release()
	p.retention = nil
}

func (p *responseStateCommit) attach(response *provider.Response) *provider.Response {
	if p != nil {
		response.CommitResponseState = p.commit
		response.DiscardOutput = p.discard
	}
	return response
}
