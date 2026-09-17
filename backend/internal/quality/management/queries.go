package management

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

// QueryRegistry exposes quality records and a single state revision. It cannot
// mutate account or exit eligibility and exposes no database handle.
type QueryRegistry interface {
	ListRecentCases(context.Context, int) ([]model.CaseRecord, error)
	CountCases(context.Context) (int64, error)
	CountOpenCases(context.Context) (int64, error)
	ListParties(context.Context, uint64) ([]model.PartyRecord, error)
	ListNodeIPArchives(context.Context, int) ([]model.NodeQualityView, error)
	ManagementState(context.Context) (model.ManagementState, error)
}

type QueryEvidence interface {
	SnapshotWindow(time.Time) model.Snapshot
	Count(context.Context) (int64, error)
}

type SelfChecker interface{ SelfCheck() error }

type LiveCases interface {
	LiveCaseViews(context.Context, time.Time) ([]court.LiveCaseView, error)
}

type QueryProbes interface {
	ListProbeTasks(context.Context, int) ([]model.ProbeTaskView, error)
	ListProbeTasksForCase(context.Context, uint64) ([]model.ProbeTaskView, error)
}

type NodeProfiles interface {
	ListProfiles(context.Context) ([]proxy.NodeProfile, error)
}

type QueryDependencies struct {
	Registry         QueryRegistry
	Evidence         QueryEvidence
	Guard            SelfChecker
	Court            LiveCases
	Probes           QueryProbes
	Nodes            NodeProfiles
	ObservationDrops func() int64
}

// Queries owns the operator-facing composition of quality records, evidence
// and current eligibility. Settings CAS remains the independent Service above.
type Queries struct{ deps QueryDependencies }

func NewQueries(deps QueryDependencies) *Queries {
	var missing []string
	if deps.Registry == nil {
		missing = append(missing, "Registry")
	}
	if deps.Evidence == nil {
		missing = append(missing, "Evidence")
	}
	if deps.Court == nil {
		missing = append(missing, "Court")
	}
	if deps.Probes == nil {
		missing = append(missing, "Probes")
	}
	if deps.Guard == nil {
		missing = append(missing, "Guard")
	}
	if deps.Nodes == nil {
		missing = append(missing, "Nodes")
	}
	if len(missing) > 0 {
		panic("quality management queries: missing " + strings.Join(missing, ", "))
	}
	return &Queries{deps: deps}
}

type SelfCheck struct{ Outcome, Detail string }
type EvidenceSummary struct {
	Decidable, Degraded, Nodes, Accounts int
	Incidence                            float64
}
type Overview struct {
	CasesTotal, CasesOpen int
	// Verdicts 是最近 200 案的裁决分布(近期窗口口径,非全量)。
	Verdicts map[string]int
	// VerdictWindow 标识分布的近期窗口大小;API/UI 据此显式标注。
	VerdictWindow     int
	Evidence          EvidenceSummary
	ObservationsTotal int64
	GuardSelfCheck    SelfCheck
	ObservationDrops  *int64
}

func (q *Queries) Overview(ctx context.Context) (Overview, error) {
	var value Overview
	records, err := q.deps.Registry.ListRecentCases(ctx, 200)
	if err != nil {
		return value, err
	}
	total, err := q.deps.Evidence.Count(ctx)
	if err != nil {
		return value, fmt.Errorf("read observation total: %w", err)
	}
	if caseTotal, caseErr := q.deps.Registry.CountCases(ctx); caseErr != nil {
		return value, fmt.Errorf("read case total: %w", caseErr)
	} else {
		value.CasesTotal = int(caseTotal)
	}
	if openTotal, openErr := q.deps.Registry.CountOpenCases(ctx); openErr != nil {
		return value, fmt.Errorf("read open case total: %w", openErr)
	} else {
		value.CasesOpen = int(openTotal)
	}
	// Verdicts 是最近 200 案的裁决分布(近期窗口口径,非全量)。
	value.Verdicts = make(map[string]int)
	value.VerdictWindow = len(records)
	for _, record := range records {
		if record.Verdict != model.VerdictNone {
			value.Verdicts[string(record.Verdict)]++
		}
	}
	snapshot := q.deps.Evidence.SnapshotWindow(time.Now().UTC())
	rate, nodes, accounts := snapshot.Incidence()
	value.Evidence = EvidenceSummary{Decidable: snapshot.Decidable, Degraded: snapshot.Degraded, Nodes: nodes, Accounts: accounts, Incidence: rate}
	value.ObservationsTotal = total
	value.GuardSelfCheck = SelfCheck{Outcome: "ok"}
	if err := q.deps.Guard.SelfCheck(); err != nil {
		value.GuardSelfCheck = SelfCheck{Outcome: "error", Detail: err.Error()}
	}
	if q.deps.ObservationDrops != nil {
		n := q.deps.ObservationDrops()
		value.ObservationDrops = &n
	}
	return value, nil
}

const matrixCap = 40

type MatrixAccount struct {
	ID     uint64
	State  model.AccountState
	LastAt time.Time
}
type MatrixExit struct {
	Key    model.EpochKey
	State  model.ExitState
	LastAt time.Time
}
type MatrixCell struct {
	AccountID       uint64
	Exit            model.EpochKey
	Clean, Degraded int
}
type Matrix struct {
	Accounts []MatrixAccount
	Exits    []MatrixExit
	Cells    []MatrixCell
}

func (q *Queries) Matrix(ctx context.Context) (Matrix, error) {
	if err := ctx.Err(); err != nil {
		return Matrix{}, err
	}
	snapshot := q.deps.Evidence.SnapshotWindow(time.Now().UTC())
	state, err := q.deps.Registry.ManagementState(ctx)
	if err != nil {
		return Matrix{}, fmt.Errorf("read quality state: %w", err)
	}
	value := Matrix{Accounts: make([]MatrixAccount, 0, len(snapshot.Accounts)), Exits: make([]MatrixExit, 0, len(snapshot.Exits)), Cells: make([]MatrixCell, 0)}
	for id, stats := range snapshot.Accounts {
		entry := state.Accounts[id].State
		if entry == "" {
			entry = model.AccountActive
		}
		value.Accounts = append(value.Accounts, MatrixAccount{ID: id, State: entry, LastAt: stats.LastAt.UTC()})
	}
	sort.Slice(value.Accounts, func(i, j int) bool {
		if value.Accounts[i].State != value.Accounts[j].State {
			return value.Accounts[i].State < value.Accounts[j].State
		}
		return value.Accounts[i].ID < value.Accounts[j].ID
	})
	if len(value.Accounts) > matrixCap {
		value.Accounts = value.Accounts[:matrixCap]
	}
	for key, stats := range snapshot.Exits {
		entry := state.Exits[key].State
		if entry == "" {
			entry = model.ExitAvailable
		}
		value.Exits = append(value.Exits, MatrixExit{Key: key, State: entry, LastAt: stats.LastAt.UTC()})
	}
	sort.Slice(value.Exits, func(i, j int) bool {
		if value.Exits[i].State != value.Exits[j].State {
			return value.Exits[i].State < value.Exits[j].State
		}
		if value.Exits[i].Key.NodeID != value.Exits[j].Key.NodeID {
			return value.Exits[i].Key.NodeID < value.Exits[j].Key.NodeID
		}
		return value.Exits[i].Key.Epoch < value.Exits[j].Key.Epoch
	})
	if len(value.Exits) > matrixCap {
		value.Exits = value.Exits[:matrixCap]
	}
	accountSet := make(map[uint64]bool, len(value.Accounts))
	exitSet := make(map[model.EpochKey]bool, len(value.Exits))
	for _, row := range value.Accounts {
		accountSet[row.ID] = true
	}
	for _, col := range value.Exits {
		exitSet[col.Key] = true
	}
	for _, pair := range snapshot.PairStats() {
		if accountSet[pair.AccountID] && exitSet[pair.Exit] {
			value.Cells = append(value.Cells, MatrixCell{AccountID: pair.AccountID, Exit: pair.Exit, Clean: pair.Clean(), Degraded: pair.Degraded})
		}
	}
	return value, nil
}

// ErrCaseParties distinguishes an unreadable party record for the existing
// management error contract; no partial case list is returned on failure.
var ErrCaseParties = court.ErrReadCaseParties

type Case struct {
	ID       uint64
	Status   model.CaseStatus
	Verdict  model.Verdict
	OpenedAt time.Time
	ClosedAt *time.Time
	Parties  []model.PartyRecord
	Evidence map[string]any
	Live     *court.LiveCaseView
}

func (q *Queries) Cases(ctx context.Context) ([]Case, error) {
	records, err := q.deps.Registry.ListRecentCases(ctx, 100)
	if err != nil {
		return nil, err
	}
	liveByID := make(map[uint64]court.LiveCaseView)
	views, err := q.deps.Court.LiveCaseViews(ctx, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("read live cases: %w", err)
	}
	for _, view := range views {
		liveByID[view.CaseID] = view
	}
	values := make([]Case, 0, len(records))
	for _, record := range records {
		parties, err := q.deps.Registry.ListParties(ctx, record.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: case %d: %w", ErrCaseParties, record.ID, err)
		}
		value := Case{ID: record.ID, Status: record.Status, Verdict: record.Verdict, OpenedAt: record.OpenedAt, ClosedAt: record.ClosedAt, Parties: parties}
		// Legacy records may contain non-object evidence. Preserve their
		// readable case/party history without inventing replacement evidence.
		if record.EvidenceJSON != "" {
			var data map[string]any
			if err := json.Unmarshal([]byte(record.EvidenceJSON), &data); err == nil && len(data) > 0 {
				value.Evidence = data
			}
		}
		if record.Status == model.CaseInvestigating {
			if live, ok := liveByID[record.ID]; ok {
				value.Live = &live
			}
		}
		values = append(values, value)
	}
	return values, nil
}

type EgressNode struct {
	model.NodeQualityView
	Webhook bool
}

func (q *Queries) Egress(ctx context.Context) ([]EgressNode, error) {
	archive, err := q.deps.Registry.ListNodeIPArchives(ctx, 100)
	if err != nil {
		return nil, err
	}
	profiles, err := q.deps.Nodes.ListProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("read node profiles: %w", err)
	}
	rotatable := make(map[uint64]bool, len(profiles))
	for _, node := range profiles {
		rotatable[node.ID] = node.RotationWebhook
	}
	values := make([]EgressNode, 0, len(archive))
	for _, view := range archive {
		values = append(values, EgressNode{NodeQualityView: view, Webhook: rotatable[view.NodeID]})
	}
	return values, nil
}

type Probe = model.ProbeTaskView

// Probes keeps the entire case history; only the global recent list is capped.
func (q *Queries) Probes(ctx context.Context, caseID uint64, limit int) ([]Probe, error) {
	if caseID != 0 {
		return q.deps.Probes.ListProbeTasksForCase(ctx, caseID)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return q.deps.Probes.ListProbeTasks(ctx, limit)
}
