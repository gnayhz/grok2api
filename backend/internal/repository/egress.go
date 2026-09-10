package repository

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// EgressRepository is the shared node read surface. Administrative configuration
// and runtime observations have separate write contracts.
type EgressRepository interface {
	ListEgressNodes(ctx context.Context, sort SortQuery) ([]egress.Node, error)
	GetEgressNode(ctx context.Context, id uint64) (egress.Node, error)
}

// EgressNodeWriter edits administrator-owned configuration. A fixed-target
// validator is required whenever the candidate is referenced by routing.
type EgressNodeWriter interface {
	CreateEgressNode(ctx context.Context, value egress.Node) (egress.Node, error)
	UpdateEgressNodeConfiguration(ctx context.Context, value egress.Node, validate egress.FixedTargetValidator) (egress.Node, error)
	DeleteEgressNode(ctx context.Context, id uint64) error
}

// EgressRuntimeRepository commits observations against their binding/revision.
// Runtime consumers cannot use administrative whole-node configuration writes.
type EgressRuntimeRepository interface {
	EgressRepository
	ApplyEgressHealthObservation(context.Context, egress.HealthObservation) (egress.HealthState, error)
	ApplyEgressClearance(context.Context, egress.ClearanceUpdate) error
	RecordEgressClearanceError(context.Context, egress.Node) error
}

// EgressNodeFactsReader reads one current operational snapshot without returning
// proxy material or loading membership enrichment, or starting network measurements.
type EgressNodeFactsReader interface {
	ListNodeFacts(context.Context) ([]egress.NodeFacts, error)
	NodeFacts(context.Context, uint64) (egress.NodeFacts, bool, error)
}

// EgressNodePageRepository is the bounded management-list contract. Runtime
// routing repositories only need EgressRepository's full-list operations.
type EgressNodePageRepository interface {
	ListEgressNodePage(ctx context.Context, input EgressNodeListQuery) ([]egress.Node, int64, error)
}

type EgressNodeCleanupPreview struct {
	Nodes               int64
	SubscriptionManaged int64
}

// EgressNodeUnhealthyCleaner provides an atomic cleanup path for nodes whose
// latest IPv4 and IPv6 probes both failed.
type EgressNodeUnhealthyCleaner interface {
	PreviewUnhealthyEgressNodes(context.Context) (EgressNodeCleanupPreview, error)
	DeleteUnhealthyEgressNodes(context.Context) ([]uint64, error)
}

type EgressNodeListFilter struct {
	Enabled     *bool
	ProbeStatus egress.ProbeStatus
}

type EgressNodeListQuery struct {
	Page   PageQuery
	Filter EgressNodeListFilter
}

type EgressSourceListQuery struct {
	Page PageQuery
}
