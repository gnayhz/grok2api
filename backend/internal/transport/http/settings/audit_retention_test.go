package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuditRetentionHTTPCompatibility(t *testing.T) {
	service, repo, router := settingsHTTPFixture(t)
	put := func(config settingsConfigDTO, want int) {
		t.Helper()
		data, err := json.Marshal(updateRequest{Revision: service.Get().Revision, Config: config})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/settings", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
		}
	}
	current := func() settingsConfigDTO { return newSettingsResponse(service.Get()).Config }
	// Legacy explicit zero becomes the sole canonical persisted value.
	input := current()
	input.Audit.RetentionPeriod = nil
	input.Audit.RetentionDays = intPointer(0)
	put(input, http.StatusOK)
	stored, _, _, _, err := repo.Get(context.Background())
	if err != nil || stored.Audit.RetentionPeriod == nil || *stored.Audit.RetentionPeriod != 0 || stored.Audit.RetentionDays != nil {
		t.Fatalf("zero persisted=%+v err=%v", stored.Audit, err)
	}
	input = current()
	input.Audit.RetentionPeriod, input.Audit.RetentionDays = stringPointer("36h0m0.000000001s"), nil
	put(input, http.StatusOK)
	response := current().Audit
	if response.RetentionDays != nil || response.RetentionSource != "runtime" || response.FileRetentionPeriod != "168h" {
		t.Fatalf("fractional projection=%+v", response)
	}
	policy, err := service.AuditRetentionPolicy(context.Background())
	if err != nil || policy.Period != 36*time.Hour+time.Nanosecond {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	before := service.Get().Revision
	input = current()
	input.Audit.RetentionPeriod, input.Audit.RetentionDays = nil, intPointer(1)
	put(input, http.StatusBadRequest)
	input = current()
	input.Audit.RetentionDays = intPointer(1)
	put(input, http.StatusBadRequest)
	for _, period := range []string{"", "-24h", "23h", "8761h", "bogus"} {
		input = current()
		input.Audit.RetentionPeriod, input.Audit.RetentionDays = stringPointer(period), nil
		put(input, http.StatusBadRequest)
	}
	if service.Get().Revision != before {
		t.Fatal("invalid update advanced revision")
	}
	// Clients predating both fields preserve duration on an unrelated save.
	input = current()
	input.Audit.RetentionPeriod, input.Audit.RetentionDays = nil, nil
	input.Audit.RetentionSource = "untrusted"
	put(input, http.StatusOK)
	if current().Audit.RetentionSource != "runtime" || *current().Audit.RetentionPeriod != *response.RetentionPeriod {
		t.Fatal("omitted retention or client metadata changed policy")
	}
}
