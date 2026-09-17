package qualityhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/gin-gonic/gin"
)

func TestQualityManagementProjectionKeepsEpochAndReadFailure(t *testing.T) {
	for _, scenario := range []string{"matrix_epoch", "overview_read_failure"} {
		t.Run(scenario, func(t *testing.T) {
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
			h := &Handler{deps: Deps{Queries: testManagementQueries(t, reg, func(deps *management.QueryDependencies) { deps.Evidence = observations })}}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.GET("/quality/overview", h.getOverview)
			router.GET("/quality/evidence/matrix", h.getEvidenceMatrix)
			path := "/quality/overview"
			if scenario == "matrix_epoch" {
				first, _, err := reg.AdvanceEpoch(ctx, 7, model.ExitIdentityFromAggregate("198.51.100.1"))
				if err != nil {
					t.Fatal(err)
				}
				if err := observations.Record(ctx, model.Observation{AccountID: 42, Exit: model.EpochKey{NodeID: 7, Epoch: first}, At: time.Now().UTC(), Source: model.SourceTraffic, Outcome: model.OutcomeDegraded}); err != nil {
					t.Fatal(err)
				}
				second, _, err := reg.AdvanceEpoch(ctx, 7, model.ExitIdentityFromAggregate("198.51.100.2"))
				if err != nil {
					t.Fatal(err)
				}
				if err := reg.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: 7, Epoch: second, To: model.ExitRemanded, CaseID: 1}); err != nil {
					t.Fatal(err)
				}
				if err := observations.Record(ctx, model.Observation{AccountID: 42, Exit: model.EpochKey{NodeID: 7, Epoch: second}, At: time.Now().UTC(), Source: model.SourceTraffic, Outcome: model.OutcomeDegraded}); err != nil {
					t.Fatal(err)
				}
				path = "/quality/evidence/matrix"
			} else {
				if err := reg.DB().Exec("DROP TABLE q_observation").Error; err != nil {
					t.Fatal(err)
				}
			}
			out := httptest.NewRecorder()
			router.ServeHTTP(out, httptest.NewRequest(http.MethodGet, path, nil))
			if scenario == "overview_read_failure" {
				if out.Code != http.StatusInternalServerError {
					t.Fatalf("failed observation count reported as valid overview: status=%d body=%s", out.Code, out.Body.String())
				}
				return
			}
			var payload struct {
				Data struct {
					Exits []struct {
						Node  uint64 `json:"node"`
						Epoch uint64 `json:"epoch"`
						State string `json:"state"`
					} `json:"exits"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if out.Code != 200 || len(payload.Data.Exits) != 2 {
				t.Fatalf("matrix=%s", out.Body.String())
			}
			for _, exit := range payload.Data.Exits {
				want := string(model.ExitAvailable)
				if exit.Epoch == 2 {
					want = string(model.ExitRemanded)
				}
				if exit.State != want {
					t.Fatalf("epoch %d displayed state %s want %s", exit.Epoch, exit.State, want)
				}
			}
		})
	}
}
