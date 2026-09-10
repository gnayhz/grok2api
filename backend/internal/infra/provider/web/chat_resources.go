package web

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

const maxWebRetainedBytes = 64 << 20

// Web keeps text for its final aggregate and optional response history. Charge
// those copies and retained metadata to the same request/process pool as Build
// and Console. The factor includes builders, tool capture and protocol output.
// This is a conservative capacity policy, not an exact allocator measurement.
type webResponseResources struct {
	budget *responsebuffer.Budget
	state  *responsebuffer.State
}

func newWebResponseResources(ctx context.Context) *webResponseResources {
	budget := responsebuffer.FromContext(ctx)
	return &webResponseResources{budget: budget, state: responsebuffer.NewState(budget, maxWebRetainedBytes)}
}

func (r *webResponseResources) retainFrame(data []byte, kind, delta string) error {
	if r == nil {
		return nil
	}
	bytes := len(delta) * 8
	if kind != "text" && kind != "reasoning" {
		// Metadata can outlive its decode workspace (cards, citations, tool results).
		// Count the structural amplification as well as its string payload.
		bytes = responsebuffer.JSONWorkspaceSize(data) - 4096
	}
	return r.state.Grow(bytes, 0)
}

func (r *webResponseResources) Close() {
	if r != nil {
		r.state.Close()
	}
}
