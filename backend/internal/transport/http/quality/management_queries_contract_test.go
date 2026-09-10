package qualityhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/gin-gonic/gin"
)

func openManagementHTTP(t *testing.T) (*registry.Registry, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	reg, err := registry.Open(context.Background(), registry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	handler := &Handler{deps: Deps{Queries: testManagementQueries(t, reg, nil)}}
	router := gin.New()
	handler.Register(router.Group(""))
	return reg, router
}

func TestManagementHTTPReadFailuresAreExplicit(t *testing.T) {
	for _, tc := range []struct{ table, path, code string }{
		{"q_case", "/quality/overview", "quality_overview_failed"},
		{"q_observation", "/quality/overview", "quality_overview_failed"},
		{"q_case_party", "/quality/court/cases", "quality_case_parties_failed"},
		{"q_probe_task", "/quality/court/cases", "quality_cases_failed"},
		{"q_probe_task", "/quality/probes", "quality_probes_failed"},
		{"q_probe_task", "/quality/probes?case_id=1", "quality_probes_failed"},
		{"q_state_revision", "/quality/evidence/matrix", "quality_matrix_failed"},
	} {
		t.Run(tc.table+tc.path, func(t *testing.T) {
			reg, router := openManagementHTTP(t)
			if _, err := reg.CreateCase(context.Background(), time.Now().UTC(), "{}"); err != nil {
				t.Fatal(err)
			}
			if err := reg.DB().Exec("DROP TABLE " + tc.table).Error; err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(router)
			defer server.Close()
			res, err := server.Client().Get(server.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var value struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(res.Body).Decode(&value); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusInternalServerError || value.Error.Code != tc.code {
				t.Fatalf("status=%d error=%+v", res.StatusCode, value)
			}
		})
	}
}

func TestManagementHTTPPreservesFullProbeHistoryAndOptionalFields(t *testing.T) {
	reg, router := openManagementHTTP(t)
	ctx := context.Background()
	id, err := reg.CreateCase(ctx, time.Now().UTC(), `{"trigger":"traffic_degraded"}`)
	if err != nil {
		t.Fatal(err)
	}
	tasks := registry.NewProbeTaskStore(reg)
	for i := 0; i < 205; i++ {
		if _, err := tasks.CreateProbeTask(ctx, model.ProbeTask{CaseID: id, Direction: model.ProbeAccountDifferential, DefendantAccountID: 42, DefendantNodeID: uint64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(router)
	defer server.Close()
	get := func(path string) []map[string]any {
		t.Helper()
		res, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var value struct {
			Data struct {
				Items []map[string]any `json:"items"`
			} `json:"data"`
		}
		if err := json.NewDecoder(res.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Fatalf("status=%d payload=%+v", res.StatusCode, value)
		}
		return value.Data.Items
	}
	for _, tc := range []struct {
		query string
		count int
	}{{"?limit=3", 3}, {"?limit=200", 200}, {"?limit=201", 50}, {"?limit=invalid", 50}, {"?case_id=" + strconv.FormatUint(id, 10) + "&limit=1", 205}} {
		rows := get("/quality/probes" + tc.query)
		if len(rows) != tc.count {
			t.Fatalf("%s rows=%d want=%d", tc.query, len(rows), tc.count)
		}
		for _, row := range rows {
			if _, ok := row["finished_at"]; ok {
				t.Fatalf("pending task has finished_at: %+v", row)
			}
		}
	}
	cases := get("/quality/court/cases")
	if len(cases) != 1 {
		t.Fatalf("cases=%+v", cases)
	}
	if _, ok := cases[0]["closed_at"]; ok {
		t.Fatal("open case exposes closed_at")
	}
	live, ok := cases[0]["live"].(map[string]any)
	if !ok || live["pending_probes"] != float64(205) {
		t.Fatalf("live=%+v", live)
	}
	if err := reg.CloseCase(ctx, id, model.CaseDismissed, model.VerdictInsufficient, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cases = get("/quality/court/cases")
	if _, ok := cases[0]["live"]; ok {
		t.Fatal("closed case exposes live")
	}
	if _, ok := cases[0]["closed_at"]; !ok {
		t.Fatal("closed case missing closed_at")
	}
	if rows := get("/quality/probes?case_id=" + strconv.FormatUint(id, 10) + "&limit=1"); len(rows) != 205 {
		t.Fatal("closed case lost task history")
	}
}
