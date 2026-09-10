package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestHTTPReportsSavedPendingAndAuthoritativeReads(t *testing.T) {
	fail := true
	service, repo, router := settingsHTTPFixture(t, settingsapp.ApplyTarget{Name: "network", Apply: func(context.Context, config.Config) error {
		if fail {
			panic("private-token")
		}
		return nil
	}})
	configJSON, err := json.Marshal(newSettingsResponse(service.Get()).Config)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"revision":"0","config":` + string(configJSON) + `}`
	result := httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(body)))
	if result.Code != http.StatusAccepted || !strings.Contains(result.Body.String(), `"applyPending":true`) || !strings.Contains(result.Body.String(), `"appliedRevision":"0"`) || strings.Contains(result.Body.String(), "private-token") {
		t.Fatalf("save=%d %s", result.Code, result.Body.String())
	}
	// Read retries and truthfully reports pending, without turning the successful
	// durable read into a 500 or claiming a stale process snapshot is current.
	result = httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"applyPending":true`) {
		t.Fatalf("read=%s", result.Body.String())
	}
	fail = false
	result = httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"appliedRevision":"1"`) || service.Get().ApplyPending {
		t.Fatalf("retry=%s", result.Body.String())
	}
	fail = true
	result = httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest(http.MethodDelete, "/settings", strings.NewReader(`{"revision":"1"}`)))
	if result.Code != http.StatusAccepted || service.Get().Revision != 2 {
		t.Fatalf("reset=%d %s", result.Code, result.Body.String())
	}
	if _, _, err := repo.Reset(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	fail = false
	result = httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if result.Code != http.StatusOK || service.Get().AppliedRevision != 3 || !strings.Contains(result.Body.String(), `"state":"disabled"`) {
		t.Fatalf("durable read=%s", result.Body.String())
	}
}

func TestAllApplicationVersionsUseDecimalStrings(t *testing.T) {
	const revision uint64 = 9007199254740993
	snapshot := settingsapp.Snapshot{Revision: revision, AppliedRevision: revision - 1,
		ApplyTargets: []settingsapp.ApplyStatus{{Name: "consumer", AppliedRevision: revision - 1, Pending: true}},
		Notification: settingsapp.NotificationStatus{Revision: revision, State: "published"}}
	encoded, err := json.Marshal(newSettingsResponse(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["revision"] != "9007199254740993" || decoded["appliedRevision"] != "9007199254740992" {
		t.Fatalf("versions=%s", encoded)
	}
	if decoded["applyTargets"].([]any)[0].(map[string]any)["appliedRevision"] != "9007199254740992" || decoded["notification"].(map[string]any)["revision"] != "9007199254740993" {
		t.Fatalf("nested versions=%s", encoded)
	}
}

func TestRotationResetDoesNotResetOtherGatewaySettings(t *testing.T) {
	service, _, router := settingsHTTPFixture(t)
	baseline := service.Get().Config
	input := baseline
	input.Server.MaxConcurrentRequests = baseline.Server.MaxConcurrentRequests + 1
	input.EgressRotation.MaxGlobalPerHour = baseline.EgressRotation.MaxGlobalPerHour + 1
	if _, err := service.Update(context.Background(), 0, input); err != nil {
		t.Fatal(err)
	}
	request := func(revision string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/settings/egress-rotation/reset", strings.NewReader(`{"revision":"`+revision+`"}`)))
		return rec
	}
	if rec := request("0"); rec.Code != http.StatusConflict {
		t.Fatalf("stale scoped reset=%d", rec.Code)
	}
	if rec := request("1"); rec.Code != http.StatusOK {
		t.Fatalf("scoped reset=%d %s", rec.Code, rec.Body.String())
	}
	after := service.Get()
	if after.Revision != 2 || after.Config.EgressRotation.MaxGlobalPerHour != baseline.EgressRotation.MaxGlobalPerHour || after.Config.Server.MaxConcurrentRequests != input.Server.MaxConcurrentRequests {
		t.Fatal("rotation reset crossed scope")
	}
}
