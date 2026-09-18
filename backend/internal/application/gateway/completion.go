package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestdiag"
)

// completionState serializes the success barrier and its receipts. A later
// ownership or delivery failure never rewrites an acknowledged history commit.
// The transport must invoke the barrier after protocol validation and before
// publishing a success terminator, then finalize with the actual write result.
type completionReceipt struct {
	generation, history, providerState, ownership string
}

type completionState struct {
	mu            sync.Mutex
	attempted     bool
	finalized     bool
	err           error
	usage         Usage
	responseID    string
	generation    string
	history       string
	providerState string
	ownership     string
}

func (d *deliverySession) requiresOwnership() bool {
	return d.plan.storeResponse && d.plan.operation == audit.OperationResponses
}

func (d *deliverySession) commitCompletion(facts Completion) error {
	usage, responseID := facts.Usage, facts.ResponseID
	c := &d.completion
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempted {
		if responseID != c.responseID {
			return fmt.Errorf("%w: response identity changed after completion", inferencedomain.ErrCompletionCommit)
		}
		return c.err
	}
	if c.finalized {
		return fmt.Errorf("%w: response already finalized", inferencedomain.ErrCompletionCommit)
	}
	if d.imageFacts != nil {
		_, outcome := d.imageFacts.snapshot()
		if outcome != "completed" {
			return &UpstreamFailure{HTTPStatus: 502, Code: "image_generation_incomplete", PublicMessage: "上游没有返回完整的图片结果"}
		}
	}
	c.attempted = true
	c.usage, c.responseID, c.generation = usage, responseID, "completed"
	c.history, c.providerState, c.ownership = "not_required", "not_required", "not_required"
	if d.response.CommitOutput != nil {
		c.history = "not_committed"
	}
	if d.response.CommitResponseState != nil {
		c.providerState = "not_committed"
	}
	if d.requiresOwnership() {
		c.ownership = "not_committed"
	}
	if d.requiresOwnership() && (responseID == "" || facts.NativeResponseID != responseID) {
		c.ownership = "failed"
		c.err = fmt.Errorf("%w: %w: missing or inconsistent native response identity", inferencedomain.ErrCompletionCommit, inferencedomain.ErrResponseOwnershipCommit)
		return c.err
	}
	if err := d.ctx.Err(); err != nil {
		c.err = fmt.Errorf("%w: %w", inferencedomain.ErrCompletionCommit, err)
		return c.err
	}
	if d.response.CommitOutput != nil {
		started := time.Now()
		err := d.response.CommitOutput()
		requestdiag.Stage(d.ctx, "history_commit", started)
		if err != nil {
			stage, reason := historydomain.FailureDiagnostic(err)
			requestdiag.Failure(d.ctx, "history", stage, reason)
			c.history = "failed"
			c.err = fmt.Errorf("%w: %w: %w", inferencedomain.ErrCompletionCommit, historydomain.ErrHistoryCommit, err)
			return c.err
		}
		c.history = "committed"
	}
	if d.response.CommitResponseState != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), finalizationOwnershipBudget)
		err := d.response.CommitResponseState(ctx)
		cancel()
		if err != nil {
			c.providerState = "failed"
			c.err = fmt.Errorf("%w: %w: %w", inferencedomain.ErrCompletionCommit, inferencedomain.ErrProviderStateCommit, err)
			return c.err
		}
		c.providerState = "committed"
	}
	if d.requiresOwnership() {
		if err := d.saveOwnership(responseID); err != nil {
			c.ownership = "failed"
			c.err = fmt.Errorf("%w: %w: %w", inferencedomain.ErrCompletionCommit, inferencedomain.ErrResponseOwnershipCommit, err)
			return c.err
		}
		c.ownership = "committed"
	}
	return nil
}

func (d *deliverySession) saveOwnership(responseID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), finalizationOwnershipBudget)
	defer cancel()
	now := time.Now().UTC()
	value := inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: d.credential.ID,
		ClientKeyID: d.plan.clientKeyID, ModelRouteID: d.plan.route.ID, Provider: d.plan.route.Provider,
		PromptCacheKey: d.plan.promptCacheKey, ReasoningReplayKey: d.plan.reasoningReplayKey}
	if err := d.service.responses.Record(ctx, value, now); err != nil {
		return err
	}
	return nil
}

// finishCompletion merges the metadata observed at the success barrier with
// the final transport observation. Close/cancel cannot erase already reported
// usage, response identity or acknowledged commits.
func (d *deliverySession) finishCompletion(usage Usage, responseID, code string) (Usage, string, string, completionReceipt) {
	c := &d.completion
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finalized = true
	if !usage.Reported && c.usage.Reported {
		usage = c.usage
	}
	if responseID == "" {
		responseID = c.responseID
	}
	if c.history == "" {
		c.history = "not_required"
		if d.response.CommitOutput != nil {
			c.history = "not_committed"
		}
	}
	if c.providerState == "" {
		c.providerState = "not_required"
		if d.response.CommitResponseState != nil {
			c.providerState = "not_committed"
		}
	}
	if c.ownership == "" {
		c.ownership = "not_required"
		if d.requiresOwnership() {
			c.ownership = "not_committed"
		}
	}
	if c.generation == "" {
		c.generation = "unconfirmed"
		if isUpstreamStreamFailure(code) {
			c.generation = "failed"
		}
	}
	if code == "" && auditRequestSucceeded(d.response.StatusCode, code) {
		switch {
		case errors.Is(c.err, inferencedomain.ErrResponseOwnershipCommit):
			code = "response_ownership_commit_failed"
		case errors.Is(c.err, inferencedomain.ErrProviderStateCommit):
			code = "provider_state_commit_failed"
		case errors.Is(c.err, historydomain.ErrHistoryCommit):
			code = "history_commit_failed"
		case c.err != nil || !c.attempted:
			code = "completion_commit_failed"
		}
	}
	// Return only immutable values, never copy the mutex.
	return usage, responseID, code, completionReceipt{generation: c.generation, history: c.history, providerState: c.providerState, ownership: c.ownership}
}
