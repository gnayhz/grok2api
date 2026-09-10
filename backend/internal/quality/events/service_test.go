package events

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	qualitycourt "github.com/chenyme/grok2api/backend/internal/quality/court"
	qualityevidence "github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestDurableEventSurvivesWorkerRestartWithoutDuplicateVotes(t *testing.T) {
	ctx := context.Background()
	r, err := qualityregistry.Open(ctx, qualityregistry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	evidence, err := qualityevidence.New(ctx, r.DB(), qualityevidence.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := qualitycourt.DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	court := qualitycourt.New(cfg, r, qualityEvidenceSource{store: evidence}, nil)
	t.Cleanup(func() { _ = court.Close(context.Background()) })
	store := journal.New(r.DB())
	sink := New(store, evidence, court)
	at := time.Now().UTC()
	obs := Receipt{Attempt: attemptmeta.Identity{ID: "request/1", AccountID: 7, Provider: string(accountdomain.ProviderBuild),
		Path: attemptmeta.Path{NodeID: 3, Epoch: 0, Status: attemptmeta.PathRegistered}}, At: at, Outcome: Degraded, Rule: "terminal"}
	if err := sink.RecordQualityEvent(ctx, obs, time.Minute); err != nil {
		t.Fatal(err)
	}
	if cases, err := r.ListOpenCases(ctx); err != nil || len(cases) != 0 {
		t.Fatalf("request path ran the court: cases=%d err=%v", len(cases), err)
	}
	if allowed, err := store.AccountAllowed(ctx, 7, at); err != nil || allowed {
		t.Fatalf("receipt missing restriction: allowed=%v err=%v", allowed, err)
	}
	claims, err := store.Claim(ctx, "old-worker", at, time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%v err=%v", claims, err)
	}
	// Crash after downstream effects, before acknowledging the durable outbox.
	if err := sink.handle(ctx, claims[0].Event); err != nil {
		t.Fatal(err)
	}
	resumed, err := journal.New(r.DB()).Claim(ctx, "new-worker", at.Add(time.Second), time.Second, 1)
	if err != nil || len(resumed) != 1 {
		t.Fatalf("resumed=%v err=%v", resumed, err)
	}
	if err := sink.handle(ctx, resumed[0].Event); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, resumed[0], at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if count, err := evidence.Count(ctx); err != nil || count != 1 {
		t.Fatalf("duplicate evidence: count=%d err=%v", count, err)
	}
	if cases, err := r.ListOpenCases(ctx); err != nil || len(cases) != 1 {
		t.Fatalf("cases=%d err=%v", len(cases), err)
	}
	if r.AccountState(7).State != model.AccountRemanded || r.ExitStateOfCurrentEpoch(3).State != model.ExitRemanded {
		t.Fatal("case ownership did not replace temporary hold")
	}
	if allowed, err := store.AccountAllowed(ctx, 7, at.Add(time.Hour)); err != nil || allowed {
		t.Fatalf("case hold lost after temporary expiry: allowed=%v err=%v", allowed, err)
	}

	// A completion fact is archived without manufacturing a second admission vote.
	obs.Outcome = Interrupted
	obs.ErrorCode = "quality_degraded"
	obs.Rule = ""
	if err := sink.RecordQualityEvent(ctx, obs, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessOne(ctx, "completion-worker", sink.handle); err != nil {
		t.Fatal(err)
	}
	if count, err := evidence.Count(ctx); err != nil || count != 1 {
		t.Fatalf("completion voted: count=%d err=%v", count, err)
	}
}

func TestUnknownPathNeverBecomesExitEvidence(t *testing.T) {
	ctx := context.Background()
	r, err := qualityregistry.Open(ctx, qualityregistry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	evidence, err := qualityevidence.New(ctx, r.DB(), qualityevidence.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	sink := New(journal.New(r.DB()), evidence, (*qualitycourt.Service)(nil))
	e := journal.Event{Attempt: attemptmeta.Identity{ID: "unknown-path", AccountID: 7, Provider: string(accountdomain.ProviderBuild),
		Path: attemptmeta.Path{NodeID: 91, Epoch: 9, Status: attemptmeta.PathUnknown, Rotating: true}}, Stage: "admission", Outcome: "degraded", At: time.Now().UTC()}
	if err := sink.handle(ctx, e); err != nil {
		t.Fatal(err)
	}
	var row struct {
		NodeID      uint64
		Epoch       uint64
		AttemptJSON string
	}
	if err := r.DB().Table("q_observation").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.NodeID != 0 || row.Epoch != 0 {
		t.Fatalf("unknown path acquired exit evidence: %+v", row)
	}
	if row.AttemptJSON == "" {
		t.Fatal("raw attribution uncertainty lost")
	}
}

func TestAdmissionAndCompletionNeverManufactureHealthyComparisonSamples(t *testing.T) {
	ctx := context.Background()
	r, err := qualityregistry.Open(ctx, qualityregistry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	evidence, err := qualityevidence.New(ctx, r.DB(), qualityevidence.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	store := journal.New(r.DB())
	sink := New(store, evidence, (*qualitycourt.Service)(nil))
	id := attemptmeta.Identity{ID: "admitted-then-interrupted", AccountID: 7, Provider: string(accountdomain.ProviderBuild)}
	at := time.Now().UTC()
	for _, outcome := range []Outcome{Admitted, Interrupted} {
		if err := sink.RecordQualityEvent(ctx, Receipt{Attempt: id, At: at, Outcome: outcome}, 0); err != nil {
			t.Fatal(err)
		}
		if worked, err := store.ProcessOne(ctx, "sample-worker", sink.handle); err != nil || worked {
			t.Fatalf("work=%v err=%v", worked, err)
		}
	}
	if count, err := evidence.Count(ctx); err != nil || count != 0 {
		t.Fatalf("admission became a healthy sample: count=%d err=%v", count, err)
	}
	var facts int64
	if err := r.DB().Model(&journal.EventRow{}).Count(&facts).Error; err != nil || facts != 2 {
		t.Fatalf("separate durable facts lost: count=%d err=%v", facts, err)
	}
	// Old releases used traffic/delivered to mean admission. Preserve history
	// while excluding that ambiguous outcome from the production court input.
	for _, source := range []model.Source{model.SourceTraffic, model.SourceProbe} {
		if err := evidence.Record(ctx, model.Observation{At: at, AccountID: 9, Source: source, Outcome: model.OutcomeDelivered, Exit: model.EpochKey{NodeID: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := evidence.Record(ctx, model.Observation{At: at, AccountID: 9, Source: model.SourceProbe, Outcome: model.OutcomeDelivered, Exit: model.EpochKey{NodeID: 2}, Attempt: attemptmeta.Identity{ID: "completed-probe/1"}}); err != nil {
		t.Fatal(err)
	}
	if snapshot := (qualityEvidenceSource{store: evidence}).SnapshotWindow(at); snapshot.Decidable != 1 {
		t.Fatalf("historical admission promoted to a witness: %+v", snapshot)
	}
	if snapshot := evidence.SnapshotWindow(at); snapshot.Decidable != 3 {
		t.Fatal("historical diagnostics were removed")
	}
}

// Adapts only the evidence view; case decisions remain in the real Court.
type qualityEvidenceSource struct{ store *qualityevidence.Store }

func (s qualityEvidenceSource) SnapshotWindow(now time.Time) qualityevidence.Snapshot {
	return s.store.AttributionWindow(now)
}
func (s qualityEvidenceSource) CrossValidate(snapshot qualityevidence.Snapshot) qualityevidence.Estimate {
	return s.store.CrossValidate(snapshot)
}
