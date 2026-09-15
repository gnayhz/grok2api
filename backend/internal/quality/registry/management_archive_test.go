package registry

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"path/filepath"
	"testing"
)

func TestNodeArchivesUseDurableStateOfSelectedEpoch(t *testing.T) {
	for _, scenario := range []string{"peer_remand", "peer_new_epoch"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			opts := Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")}
			a, err := Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			b, err := Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			epoch, _, err := a.AdvanceEpoch(ctx, 7, model.ExitIdentityFromAggregate("198.51.100.1"))
			if err != nil {
				t.Fatal(err)
			}
			if err := a.TransitionExit(ctx, ExitTransitionRequest{NodeID: 7, Epoch: epoch, To: model.ExitRemanded, CaseID: 1}); err != nil {
				t.Fatal(err)
			}
			want := model.ExitRemanded
			if scenario == "peer_new_epoch" {
				if err := b.RefreshState(ctx); err != nil {
					t.Fatal(err)
				}
				epoch, _, err = a.AdvanceEpoch(ctx, 7, model.ExitIdentityFromAggregate("198.51.100.2"))
				if err != nil {
					t.Fatal(err)
				}
				want = model.ExitAvailable
			}
			views, err := b.ListNodeIPArchives(ctx, 100)
			if err != nil || len(views) != 1 || views[0].CurrentEpoch != epoch || views[0].State != want {
				t.Fatalf("archive=%+v want_epoch=%d want_state=%s err=%v", views, epoch, want, err)
			}
		})
	}
}
