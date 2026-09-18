package qualityhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/gin-gonic/gin"
)

type checkPreparer struct{}

func (checkPreparer) PrepareAccountCheck(_ context.Context, id uint64, requested string) (model.ProbeExperiment, error) {
	return model.ProbeExperiment{Version: model.AccountCheckVersion, Sample: "brief-confirmation", Baseline: attemptmeta.Identity{AccountID: id, Provider: "grok_build", Model: requested, RuleVersion: "fictional-rule"}}, nil
}

func TestAccountCheckHTTPPersistsSubmissionAndReportsExistingWork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg, err := registry.Open(context.Background(), registry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "check.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	h := &Handler{deps: Deps{AccountChecks: management.NewAccountChecks(registry.NewProbeTaskStore(reg), checkPreparer{})}}
	router := gin.New()
	h.Register(router.Group("/api/admin/v1"))
	request := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/admin/v1/quality/accounts/7/checks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		return w
	}
	if w := request(http.MethodPost, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing model: %d", w.Code)
	}
	first := request(http.MethodPost, `{"model":"grok-4.6"}`)
	second := request(http.MethodPost, `{"model":"grok-4.6"}`)
	if first.Code != http.StatusAccepted || first.Body.String() != second.Body.String() {
		t.Fatalf("duplicate submission: %s / %s", first.Body, second.Body)
	}
	w := request(http.MethodGet, "")
	var payload struct {
		Data struct {
			Items []model.AccountCheck `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(payload.Data.Items) != 1 || payload.Data.Items[0].State != model.ProbePending || payload.Data.Items[0].AccountID != 7 || payload.Data.Items[0].Report != nil {
		t.Fatalf("history=%s", w.Body)
	}
}
