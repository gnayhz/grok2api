package qualityhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"github.com/gin-gonic/gin"
)

type resourceHTTPFixture struct{}

func (resourceHTTPFixture) CreateResourceCheckBatch(_ context.Context, tasks []model.ProbeTask, _ int) ([]model.ResourceSubmission, error) {
	out := []model.ResourceSubmission{}
	for _, t := range tasks {
		out = append(out, model.ResourceSubmission{ResourceID: t.Experiment.ResourceCheck.ResourceID, ID: 41})
	}
	return out, nil
}
func (resourceHTTPFixture) ListResourceChecks(context.Context, string, []uint64) ([]model.ResourceCheck, error) {
	return []model.ResourceCheck{}, nil
}
func (resourceHTTPFixture) PrepareResourceCheck(context.Context, string, uint64, string) (model.ProbeExperiment, error) {
	return model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}, nil
}
func (resourceHTTPFixture) QualityProbeAccounts(context.Context, model.ProbeExperiment) ([]uint64, error) {
	return []uint64{2, 3, 4}, nil
}
func (resourceHTTPFixture) ListProfiles(context.Context) ([]proxy.NodeProfile, error) {
	return []proxy.NodeProfile{{ID: 8, Enabled: true, CanServeFixedTarget: true}}, nil
}
func (resourceHTTPFixture) Profile(context.Context, uint64) (proxy.NodeProfile, bool, error) {
	return proxy.NodeProfile{}, false, nil
}
func TestResourceCheckHTTPBatchAndQueryBounds(t *testing.T) {
	f := resourceHTTPFixture{}
	h := Handler{deps: Deps{ResourceChecks: management.NewResourceChecks(f, f, f)}}
	r := gin.New()
	r.POST("/checks", h.postResourceChecks)
	r.GET("/checks", h.getResourceChecks)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/checks", `{"kind":"account","resource_ids":[1,2],"model":"fictional-model"}`, http.StatusAccepted},
		{"POST", "/checks", `{"kind":"node","resource_ids":[8],"model":"fictional-model"}`, http.StatusAccepted},
		{"POST", "/checks", `{"kind":"account","resource_ids":["9007199254740993"],"model":"fictional-model"}`, http.StatusAccepted},
		{"POST", "/checks", `{"kind":"account","resource_ids":["18446744073709551616"],"model":"fictional-model"}`, http.StatusBadRequest},
		{"POST", "/checks", `{"kind":"account","resource_ids":[null],"model":"fictional-model"}`, http.StatusBadRequest},
		{"POST", "/checks", `{"kind":"unknown","resource_ids":[1],"model":"fictional-model"}`, http.StatusBadRequest},
		{"POST", "/checks", `{"kind":"account","resource_ids":[0],"model":"fictional-model"}`, http.StatusBadRequest},
		{"GET", "/checks?kind=account&resource_ids=1,2", "", http.StatusOK},
		{"GET", "/checks?kind=node&resource_ids=0", "", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s %s status=%d", tc.method, tc.path, w.Code)
		}
	}
}

func TestResourceCheckHTTPKeepsLargeIdentitiesExact(t *testing.T) {
	const id = uint64(9007199254740993)
	const decimal = `"9007199254740993"`
	sample := model.ResourceSample{CredentialGeneration: id, PathBinding: id, Attempt: attemptmeta.Identity{AccountID: id, Revision: id, Path: attemptmeta.Path{NodeID: id, Epoch: id}}}
	check := model.ResourceCheck{ID: id, ResourceID: id, Report: &model.ResourceCheckReport{Revision: id, ResourceID: id, Observations: []model.ResourceObservation{{AccountID: id, NodeID: id, IdentityGroup: id, Sample: sample}}, Results: []model.ResourceProof{{IdentityGroup: id, ResourceTarget: model.ResourceTarget{ResourceID: id}}}}}
	data, err := json.Marshal(checkDTO(check))
	if err != nil {
		t.Fatal(err)
	}
	var visit func(json.RawMessage)
	visit = func(raw json.RawMessage) {
		var object map[string]json.RawMessage
		if len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, &object) == nil {
			for key, value := range object {
				switch key {
				case "resource_id", "account_id", "node_id", "identity_group", "revision", "epoch", "path_binding", "credential_generation":
					if string(value) != decimal {
						t.Errorf("%s lost precision or string encoding: %s", key, value)
					}
				}
				visit(value)
			}
		}
		var array []json.RawMessage
		if len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &array) == nil {
			for _, value := range array {
				visit(value)
			}
		}
	}
	visit(data)
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil || string(result["id"]) != decimal {
		t.Fatalf("task identity lost: %s %v", data, err)
	}
	for _, input := range []string{decimal, "9007199254740993"} {
		var parsed resourceID
		if err := json.Unmarshal([]byte(input), &parsed); err != nil || uint64(parsed) != id {
			t.Fatalf("request identity lost: %d %v", parsed, err)
		}
	}
}
