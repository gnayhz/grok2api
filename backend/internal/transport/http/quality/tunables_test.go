package qualityhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/gin-gonic/gin"
)

func TestQualityTunablesVersionedHTTP(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := management.New(relational.NewSettingsDocumentRepository(db, management.SettingsKey), nil, nil)
	h := &Handler{deps: Deps{Tunables: service}}
	router := gin.New()
	router.GET("/", h.getSettings)
	router.PUT("/", h.putSettings)
	call := func(method string, input any) (*httptest.ResponseRecorder, QualityTunables) {
		if snapshot, ok := input.(QualityTunables); ok {
			input = struct {
				management.Config
				Revision string `json:"revision"`
			}{snapshot.Config, strconv.FormatUint(snapshot.Revision, 10)}
		}
		payload, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, "/", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var body struct {
			Data QualityTunables `json:"data"`
		}
		if rec.Code < 300 {
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
		}
		return rec, body.Data
	}
	rec, base := call(http.MethodGet, nil)
	if rec.Code != 200 || base.Revision != 0 || base.ApplyPending {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	rec, _ = call(http.MethodPut, base.Config)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing revision=%d", rec.Code)
	}
	rec, saved := call(http.MethodPut, base)
	if rec.Code != 200 || saved.Revision != 1 || saved.AppliedRevision != 1 {
		t.Fatalf("save=%d %s", rec.Code, rec.Body)
	}
	rec, _ = call(http.MethodPut, base)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale=%d", rec.Code)
	}
	for _, field := range []string{"probe_budget", "jurors_per_exit", "exit_need_n", "exit_need_k", "account_need_exits", "account_span_nodes", "differential_exits", "max_rotations_per_hour"} {
		rec, _ = call(http.MethodPut, map[string]any{"revision": "1", field: 99})
		if rec.Code != 400 || service.Snapshot().Revision != 1 {
			t.Fatalf("retired field %s accepted: %d", field, rec.Code)
		}
	}
	invalid := saved
	invalid.InvestigationTimeout = "invalid"
	rec, _ = call(http.MethodPut, invalid)
	if rec.Code != 400 || service.Snapshot().Revision != 1 {
		t.Fatal("invalid policy changed settings")
	}
	// A failure represented by the real service is accepted as durable intent,
	// with the previous applied revision and an explicit pending status.
	peer := management.New(relational.NewSettingsDocumentRepository(db, management.SettingsKey), func(context.Context, management.Config) error { return context.DeadlineExceeded }, nil)
	h.deps.Tunables = peer
	rec, pending := call(http.MethodPut, saved)
	if rec.Code != http.StatusAccepted || !pending.ApplyPending || pending.Revision != 2 || pending.AppliedRevision != 0 {
		t.Fatalf("pending=%d %s", rec.Code, rec.Body)
	}
	rec, pending = call(http.MethodGet, nil)
	if rec.Code != 200 || !pending.ApplyPending || pending.ApplyError == "" {
		t.Fatalf("pending GET=%d %s", rec.Code, rec.Body)
	}
}
