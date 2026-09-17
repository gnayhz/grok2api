package physical

import (
	"context"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

func trimSpace(value string) string { return strings.TrimSpace(value) }

// Journal is the per-request physical accounting contract. One logical
// execution owns exactly one journal; transports reserve entries before I/O
// and report facts through it. The mutable implementation lives outside this
// contract layer; the composition root injects it.
type Journal interface {
	// Begin reserves a bounded ledger entry before a transport submission.
	// It is idempotent per attempt identity and enforces the call ceiling.
	Begin(ctx context.Context) error
	// RecordExchange records the header-level outcome of one exchange.
	// A missing response reports status < 0.
	RecordExchange(ctx context.Context, status int, err error)
	// RecordMetric emits the per-call transport metric.
	RecordMetric(ctx context.Context, status int, err error)
	// ObserveBody accrues body bytes and the first terminal outcome.
	ObserveBody(id string, n int, outcome string)
	// FinalizeBody closes an entry's body accounting.
	FinalizeBody(id string, outcome string, startedAt time.Time)
	// MarkUpgraded records a protocol switch (WebSocket) exchange.
	MarkUpgraded(id string)
	// ObserveUsage records token usage; canonical observations win.
	ObserveUsage(ctx context.Context, id string, usage jsonpeek.TokenUsage, canonical bool)
	// ObserveGeneration records the adapter's protocol outcome.
	ObserveGeneration(ctx context.Context, id, outcome string)
	// Facts returns finalized, unconfirmed facts excluding a pending attempt.
	Facts(ctx context.Context, pendingID ...string) []attemptmeta.PhysicalFact
	// Observations returns all recorded facts regardless of finalization.
	Observations(ctx context.Context) []attemptmeta.PhysicalFact
	// Confirm marks facts persisted; batched executions may prune them.
	Confirm(ctx context.Context, facts []attemptmeta.PhysicalFact)
	// ConfigureBatches opts a long execution into acknowledged-entry pruning.
	// It must not replace an existing owner or change mode after I/O.
	ConfigureBatches(deadline time.Time, before func(context.Context) error)
	// MarkExecutionError separates an expired owner deadline from shorter
	// transport timeouts.
	MarkExecutionError(ctx context.Context, err error) error
	// Count reports started physical calls, including pruned entries.
	Count() uint64
}

// JournalFactory builds one journal per logical execution.
type JournalFactory interface {
	NewPhysicalJournal() Journal
}

// CallMeta labels the accounting scope of the current context.
type CallMeta struct {
	Provider  string
	Operation string
	Plane     string
	Stage     string
}

type callContextKey struct{}

type callContextValue struct {
	journal Journal
	meta    CallMeta
}

// WithPhysicalCallTrace starts bounded physical-call accounting for one
// downstream request. The journal comes from the execution owner; context
// only carries the contract, so no module can substitute a second ledger.
func WithPhysicalCallTrace(ctx context.Context, journal Journal, provider, operation string) context.Context {
	if ctx == nil {
		return ctx
	}
	if current := callFromContext(ctx); current.journal != nil {
		return ctx
	}
	provider = normalizeProvider(provider)
	return context.WithValue(ctx, callContextKey{}, callContextValue{
		journal: journal,
		meta:    CallMeta{Provider: provider, Operation: normalizeOperation(operation), Plane: defaultPlane(provider), Stage: "primary"},
	})
}

// WithPhysicalCallPlane annotates a bounded upstream plane while preserving
// the request-wide journal and current stage.
func WithPhysicalCallPlane(ctx context.Context, plane string) context.Context {
	value := callFromContext(ctx)
	if value.journal == nil {
		return ctx
	}
	value.meta.Plane = normalizePlane(plane)
	return context.WithValue(ctx, callContextKey{}, value)
}

// WithPhysicalCallStage annotates an internal retry or preparation stage while
// preserving the request-wide journal and upstream plane.
func WithPhysicalCallStage(ctx context.Context, stage string) context.Context {
	value := callFromContext(ctx)
	if value.journal == nil {
		return ctx
	}
	value.meta.Stage = normalizeStage(stage)
	return context.WithValue(ctx, callContextKey{}, value)
}

// JournalFromContext returns the execution-owned journal, or nil.
func JournalFromContext(ctx context.Context) Journal {
	return callFromContext(ctx).journal
}

// MetaFromContext reports the current accounting labels for implementations
// and metric attribution.
func MetaFromContext(ctx context.Context) CallMeta {
	return callFromContext(ctx).meta
}

func callFromContext(ctx context.Context) callContextValue {
	if ctx == nil {
		return callContextValue{}
	}
	value, _ := ctx.Value(callContextKey{}).(callContextValue)
	return value
}

func normalizeProvider(value string) string {
	switch trimSpace(value) {
	case "grok_build", "grok_web", "grok_console":
		return trimSpace(value)
	default:
		return "unknown"
	}
}

func normalizeOperation(value string) string {
	switch trimSpace(value) {
	case "responses", "chat", "messages", "compaction", "response_get", "response_delete", "image", "image_edit", "video", "tts", "stt", "realtime", "voice":
		return trimSpace(value)
	case "responses_compact":
		return "compaction"
	default:
		return "other"
	}
}

func defaultPlane(provider string) string {
	switch provider {
	case "grok_build":
		return "build"
	case "grok_web":
		return "web"
	case "grok_console":
		return "console"
	default:
		return "unknown"
	}
}

func normalizePlane(value string) string {
	switch trimSpace(value) {
	case "build", "xai", "web", "console":
		return trimSpace(value)
	default:
		return "unknown"
	}
}

func normalizeStage(value string) string {
	switch trimSpace(value) {
	case "primary", "plane_fallback", "reasoning_replay", "reasoning_session_reset", "compaction", "compaction_retry", "anti_bot_retry", "statsig_meta", "connection_retry", "credential_prepare", "authorization_retry", "video_poll", "asset_download":
		return trimSpace(value)
	default:
		return "other"
	}
}
