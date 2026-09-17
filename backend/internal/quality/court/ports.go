package court

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// StateStore exposes the atomic operations this use case requires.
// Implementations must validate current coordination ownership and generations.
type StateStore interface {
	Coordinate(ctx context.Context, name string) (context.Context, func(), error)
	RefreshState(ctx context.Context) error
	CurrentEpoch(nodeID uint64) uint64
	OpenCaseForIncident(ctx context.Context, accountID uint64, exit model.EpochKey) (uint64, error)
	LastClosedAtForIncidents(ctx context.Context, incidents []model.IncidentKey) (map[model.IncidentKey]time.Time, error)
	ListOpenCases(ctx context.Context) ([]model.CaseRecord, error)
	ListParties(ctx context.Context, caseID uint64) ([]model.PartyRecord, error)
	GetCase(ctx context.Context, caseID uint64) (model.CaseRecord, bool, error)
	OpenInvestigation(ctx context.Context, accountID uint64, exit model.EpochKey, now time.Time, evidence string) (uint64, error)
	SettleInvestigation(ctx context.Context, caseID uint64, verdict model.Verdict, evidence string, now time.Time, banExit bool, manualRelease ...bool) error
	UpdateInvestigationEvidence(ctx context.Context, caseID uint64, evidence string) error
	ReleaseClearedParties(ctx context.Context, caseID uint64, account, exit bool, evidence string, now time.Time) error
	EvaluationCursors(ctx context.Context) (expired, active uint64, err error)
	AdvanceEvaluationCursor(ctx context.Context, caseID uint64, expired bool) error
	ReconcileStaleExitParties(ctx context.Context) (int, error)
	AccountEligible(accountID uint64) bool
	ExitEligible(nodeID uint64) bool
	AccountState(accountID uint64) model.AccountEntry
	ExitStateOfCurrentEpoch(nodeID uint64) model.ExitEntry
	CurrentExitStates() map[uint64]model.ExitEntry
	IdentityGroupOf(accountID uint64) (groupID uint64, members []uint64)
	TransitionAccount(ctx context.Context, req model.AccountTransitionRequest) error
	TransitionExit(ctx context.Context, req model.ExitTransitionRequest) error
	UpdatePartyDisposition(ctx context.Context, caseID uint64, kind model.PartyKind, accountID, nodeID, epoch uint64, disposition model.PartyDisposition) error
}

// ProbeReader reads persisted, finite experiment results without acquiring a worker lease.
type ProbeReader interface {
	ListProbeTasksForCase(context.Context, uint64) ([]model.ProbeTaskView, error)
}
