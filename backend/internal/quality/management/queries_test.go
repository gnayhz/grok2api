package management

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/guard"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type noNodeProfiles struct{}

func (noNodeProfiles) ListProfiles(context.Context) ([]proxy.NodeProfile, error) { return nil, nil }

func TestMatrixOwnsBoundsOrderingAndEvidenceQualification(t *testing.T) {
	ctx := context.Background()
	reg, err := registry.Open(ctx, registry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	observations, err := evidence.New(ctx, reg.DB(), model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := court.DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := court.New(cfg, reg, observations, nil, registry.NewProbeTaskStore(reg))
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	query := NewQueries(QueryDependencies{Registry: reg, Evidence: observations, Court: service, Probes: registry.NewProbeTaskStore(reg), Guard: guard.New(guard.DefaultConfig(), nil), Nodes: noNodeProfiles{}})
	empty, err := query.Matrix(ctx)
	if err != nil || empty.Accounts == nil || empty.Exits == nil || empty.Cells == nil || len(empty.Cells) != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	now := time.Now().UTC().Add(-time.Second)
	for id := uint64(1); id <= 45; id++ {
		obs := model.Observation{AccountID: id, Exit: model.EpochKey{NodeID: id}, At: now, Source: model.SourceTraffic, Outcome: model.OutcomeDelivered}
		if err := observations.Record(ctx, obs); err != nil {
			t.Fatal(err)
		}
		obs.Outcome = model.OutcomeError
		if err := observations.Record(ctx, obs); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.TransitionAccount(ctx, model.AccountTransitionRequest{AccountID: 45, To: model.AccountRemanded, CaseID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := reg.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: 1, To: model.ExitRemanded, CaseID: 1}); err != nil {
		t.Fatal(err)
	}
	matrix, err := query.Matrix(ctx)
	if err != nil || len(matrix.Accounts) != 40 || len(matrix.Exits) != 40 || len(matrix.Cells) != 39 {
		t.Fatalf("matrix sizes=%d/%d/%d err=%v", len(matrix.Accounts), len(matrix.Exits), len(matrix.Cells), err)
	}
	for i, row := range matrix.Accounts {
		if row.ID != uint64(i+1) || row.State != model.AccountActive || !row.LastAt.Equal(now) {
			t.Fatalf("account ordering/state/time: %+v", row)
		}
	}
	for i, col := range matrix.Exits {
		if col.Key.NodeID != uint64(i+2) || col.State != model.ExitAvailable {
			t.Fatalf("exit ordering/state: %+v", col)
		}
	}
	for _, cell := range matrix.Cells {
		if cell.AccountID < 2 || cell.AccountID > 40 || cell.Exit.NodeID != cell.AccountID || cell.Clean != 1 || cell.Degraded != 0 {
			t.Fatalf("out of bounds or transport-error vote: %+v", cell)
		}
	}
}
