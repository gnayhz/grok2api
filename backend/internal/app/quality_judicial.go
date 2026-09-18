package app

import (
	"context"
	"log/slog"
	"time"

	qualitycourt "github.com/chenyme/grok2api/backend/internal/quality/court"
	qualityevidence "github.com/chenyme/grok2api/backend/internal/quality/evidence"
	qualityinvestigator "github.com/chenyme/grok2api/backend/internal/quality/investigator"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// qualityDispatcher 适配 court.Dispatcher → investigator.Service。
type qualityDispatcher struct {
	service *qualityinvestigator.Service
}

func (d qualityDispatcher) DispatchForCase(ctx context.Context, spec qualitycourt.DispatchSpec) (int, error) {
	return d.service.DispatchForCase(ctx, qualityinvestigator.DispatchSpec{
		ControlAccounts: spec.ControlAccounts, ControlExits: spec.ControlExits,
		CaseID:          spec.CaseID,
		Defendant:       spec.Defendant,
		BaselineExit:    spec.BaselineExit,
		HealthyExits:    spec.HealthyExits,
		CoRemandedExits: spec.CoRemandedExits,
		Jurors:          spec.Jurors,
	})
}

// qualityEvidenceSource 适配 court.EvidenceSource → evidence.Store。
type qualityEvidenceSource struct {
	store *qualityevidence.Store
}

func (s qualityEvidenceSource) SnapshotWindow(now time.Time) model.Snapshot {
	return s.store.AttributionWindow(now)
}

func (s qualityEvidenceSource) CrossValidate(snapshot model.Snapshot) model.Estimate {
	return s.store.CrossValidate(snapshot)
}

// bootstrapJudicialLayer wires the experiment queue and evaluator.
func bootstrapJudicialLayer(qualityRegistry *qualityregistry.Registry, evidenceStore *qualityevidence.Store, logger *slog.Logger) (*qualitycourt.Service, *qualityinvestigator.Service, *qualityregistry.ProbeTaskStore) {
	courtCfg := qualitycourt.DefaultConfig()
	courtCfg.Logger = logger
	probeStore := qualityregistry.NewProbeTaskStore(qualityRegistry)
	investigatorService := qualityinvestigator.New(qualityinvestigator.DefaultConfig(), probeStore, evidenceRecorderAdapter{store: evidenceStore})
	courtService := qualitycourt.New(courtCfg, qualityRegistry,
		qualityEvidenceSource{store: evidenceStore}, qualityDispatcher{service: investigatorService}, probeStore)
	return courtService, investigatorService, probeStore
}

// evidenceRecorderAdapter 适配 investigator.Recorder → evidence.Store。
type evidenceRecorderAdapter struct {
	store *qualityevidence.Store
}

func (a evidenceRecorderAdapter) Record(ctx context.Context, obs model.Observation) error {
	return a.store.Record(ctx, obs)
}
