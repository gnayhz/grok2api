package investigator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// TestResolveBaselineForLegacyTask prevents a pre-migration pending task from
// falling back to the retired two-request implementation.  The baseline is
// recoverable from the immutable case opening evidence.
func TestResolveBaselineForLegacyTask(t *testing.T) {
	reg, err := registry.Open(context.Background(), registry.Options{
		Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	caseID, err := reg.CreateCase(context.Background(), time.Now().UTC(), `{"exit":{"node":112,"epoch":0}}`)
	if err != nil {
		t.Fatal(err)
	}
	task := model.ProbeTask{CaseID: caseID, Direction: model.ProbeAccountDifferential, DefendantNodeID: 108, DefendantEpoch: 0}
	resolved, ok, err := NewProbeExecutor(reg, nil, nil).resolveBaseline(context.Background(), task)
	if err != nil || !ok {
		t.Fatalf("legacy baseline resolution failed: task=%+v ok=%v err=%v", resolved, ok, err)
	}
	if resolved.BaselineNodeID != 112 || resolved.BaselineEpoch != 0 {
		t.Fatalf("legacy task baseline=%+v", resolved)
	}
}
