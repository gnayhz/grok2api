package qualityhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/guard"
	"github.com/gin-gonic/gin"
)

type guardHTTPStore struct {
	cfg         guard.Config
	found, fail bool
}

func (s *guardHTTPStore) LoadGuard(context.Context) (guard.Config, bool, error) {
	if s.fail {
		return guard.Config{}, false, errors.New("secret storage address")
	}
	return s.cfg, s.found, nil
}
func (s *guardHTTPStore) SaveGuard(_ context.Context, cfg guard.Config) error {
	if s.fail {
		return errors.New("secret storage address")
	}
	if s.found && cfg.Revision != s.cfg.Revision+1 {
		return guard.ErrConflict
	}
	s.cfg, s.found = cfg, true
	return nil
}
func TestGuardHTTPRevisionDefaultsAndFailureContract(t *testing.T) {
	base := guard.DefaultConfig()
	base.AccountCooldown = 9 * time.Minute
	store := &guardHTTPStore{}
	service := guard.New(base, store)
	handler := &Handler{deps: Deps{Guard: service}}
	router := gin.New()
	router.GET("/guard", handler.getGuard)
	router.PUT("/guard", handler.putGuard)
	router.DELETE("/guard", handler.resetGuard)
	request := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, "/guard", strings.NewReader(body)))
		return rec
	}
	decoded := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	rec := request("PUT", `{"revision":0,"enabled":true,"guarded_models":["grok_build:grok-4.5"],"created_timeout":"0s","max_attempts":100}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy numeric=%d %s", rec.Code, rec.Body.String())
	}
	got := decoded(rec)
	if got["revision"] != "1" || got["updated"] != true || got["created_timeout"] != "5s" || got["max_attempts"] != float64(100) {
		t.Fatalf("normalized response=%v", got)
	}
	if got["file_defaults"].(map[string]any)["account_cooldown"] != "9m0s" {
		t.Fatalf("file defaults=%v", got)
	}
	for _, body := range []string{
		`{"revision":"1","enabled":true,"guarded_models":["bad:model"]}`,
		`{"revision":"1","enabled":true,"guarded_models":[]}`,
		`{"revision":"1","enabled":true,"created_timeout":"-1s"}`,
		`{"revision":"1","enabled":true,"max_attempts":0}`,
		`{"revision":9007199254740993,"enabled":false}`,
		`{"revision":null,"enabled":false}`,
	} {
		if rec := request("PUT", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid input=%s -> %d %s", body, rec.Code, rec.Body.String())
		}
		if service.Config().Revision != 1 {
			t.Fatal("invalid input changed policy")
		}
	}
	if rec := request("DELETE", `{"revision":"0"}`); rec.Code != http.StatusConflict {
		t.Fatalf("stale reset=%d", rec.Code)
	}
	rec = request("DELETE", `{"revision":"1"}`)
	if rec.Code != http.StatusOK || service.Config().Revision != 2 || service.Config().MaxAttempts != base.MaxAttempts {
		t.Fatalf("reset=%s", rec.Body.String())
	}
	// GET must load a remote write and preserve exact decimal revision.
	store.cfg.Revision, store.cfg.MaxAttempts = 9007199254740993, 7
	rec = request("GET", "")
	if rec.Code != http.StatusOK || decoded(rec)["revision"] != "9007199254740993" || service.Config().MaxAttempts != 7 {
		t.Fatalf("durable read=%s", rec.Body.String())
	}
	rec = request("PUT", `{"revision":"9007199254740993","enabled":true,"max_attempts":8}`)
	if rec.Code != http.StatusOK || decoded(rec)["revision"] != "9007199254740994" {
		t.Fatalf("big CAS=%s", rec.Body.String())
	}
	store.fail = true
	rec = request("GET", "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("failed read=%d %s", rec.Code, rec.Body.String())
	}
	rec = request("PUT", `{"revision":"9007199254740994","enabled":false}`)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") || service.Config().Revision != 9007199254740994 {
		t.Fatalf("failed write=%d %s", rec.Code, rec.Body.String())
	}
}
