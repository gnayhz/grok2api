package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// Opt-in real HTTP verification uses a copied database, shuts down the entire
// application between requests, and sends only visible history after restart.
func TestLiveConversationHistoryRestart(t *testing.T) {
	configPath := os.Getenv("GROK2API_HISTORY_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("GROK2API_HISTORY_LIVE_CONFIG not configured")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	snapshot, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "backend.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.Driver = "sqlite"
	cfg.Database.SQLite.Path = filepath.Join(dir, "backend.db")
	cfg.Audit.JournalDirectory = filepath.Join(filepath.Dir(cfg.Database.SQLite.Path), "audit-pending")
	if err = os.WriteFile(cfg.Database.SQLite.Path, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeStore.Driver = "memory"
	cfg.Deployment.Replicas = 1
	cfg.Frontend.StaticPath = ""
	cfg.Media.Local.Path = filepath.Join(dir, "media")
	history := []any{map[string]any{"role": "user", "content": "Reply exactly 323."}}
	session := fmt.Sprintf("durable-restart-%d", time.Now().UnixNano())
	var secret, keyID string
	var rows []liveInferenceRow
	var states []map[string]any
	for turn := 0; turn < 2; turn++ {
		func() {
			application, err := New(t.Context(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			application.startup.setPhase("running")
			if err := application.audits.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := application.audits.Close(ctx); err != nil {
					t.Error(err)
				}
			}()
			server := httptest.NewServer(application.server.Handler)
			defer server.Close()
			client := &http.Client{Timeout: 100 * time.Second}
			defer client.CloseIdleConnections()
			admin := liveJSON(t, client, server.URL+"/api/admin/v1/auth/login", "", http.MethodPost, map[string]any{"username": cfg.BootstrapAdmin.Username, "password": cfg.BootstrapAdmin.Password})
			token := admin["data"].(map[string]any)["tokens"].(map[string]any)["accessToken"].(string)
			if turn == 0 {
				key := liveJSON(t, client, server.URL+"/api/admin/v1/client-keys", token, http.MethodPost, map[string]any{"name": "history-restart-test", "enabled": true, "providerScope": []string{"grok_build"}, "allowModelAliases": true})["data"].(map[string]any)
				secret = key["secret"].(string)
				keyID = key["key"].(map[string]any)["id"].(string)
				// Isolate restart recovery from intentional cross-account cipher isolation.
				// Pin only this disposable snapshot to an eligible, non-exhausted account.
				account := os.Getenv("GROK2API_HISTORY_LIVE_ACCOUNT")
				if account == "" {
					account = "4103"
				}
				id, _ := strconv.ParseUint(account, 10, 64)
				if !application.quality.AccountEligible(id) {
					t.Fatal("requested account is quality-ineligible")
				}
				listed := liveJSON(t, client, server.URL+"/api/admin/v1/models?pageSize=100", token, http.MethodGet, nil)
				pinned := false
				for _, raw := range listed["data"].(map[string]any)["items"].([]any) {
					route := raw.(map[string]any)
					if route["upstreamModel"] == "grok-4.6" && route["provider"] == "grok_build" || route["upstreamModel"] == "Build/grok-4.6" {
						liveJSON(t, client, server.URL+"/api/admin/v1/models/"+route["id"].(string), token, http.MethodPatch, map[string]any{"accountIds": []string{account}})
						pinned = true
					}
				}
				if !pinned {
					t.Fatal("no Build route to pin")
				}
			}
			body := map[string]any{"model": "Build/grok-4.6", "input": history, "instructions": "Reply to each request with exactly 323.", "prompt_cache_key": session, "store": false, "include": []string{"reasoning.encrypted_content"}, "reasoning": map[string]any{"effort": "low", "summary": "detailed"}, "stream": true}
			row := liveInferenceRow{Provider: "grok_build", Model: "Build/grok-4.6", Operation: "responses", Streaming: true, Scenario: fmt.Sprintf("restart_turn_%d", turn+1)}
			if err := readLiveInference(client, server.URL+"/v1/responses", secret, body, &row); err != nil {
				t.Fatal(err)
			}
			if !row.Terminal || !strings.Contains(row.Answer, "323") {
				t.Fatalf("unsuccessful response: %#v", row)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := application.audits.Close(ctx); err != nil {
				t.Fatal(err)
			}
			audits, _, err := application.audits.List(t.Context(), 1, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, record := range audits {
				if record.RequestID == row.RequestID {
					found = true
					state := map[string]any{"turn": turn + 1, "outcome": record.HistoryOutcome, "generation": record.HistoryGeneration, "restoredItems": record.HistoryRestoredItems, "commit": record.HistoryCommit, "accountId": record.AccountID}
					states = append(states, state)
					if record.HistoryCommit != "committed" || turn == 1 && (record.HistoryOutcome != "append_ok" || record.HistoryRestoredItems < 1) {
						t.Fatalf("restart continuity failed: %+v", state)
					}
				}
			}
			if !found {
				t.Fatal("missing durable history audit")
			}
			rows = append(rows, row)
			for _, raw := range row.Output {
				item, _ := raw.(map[string]any)
				if item["type"] != "reasoning" {
					history = append(history, raw)
				}
			}
			history = append(history, map[string]any{"role": "user", "content": "Again, reply exactly 323."})
			if turn == 1 {
				liveJSON(t, client, server.URL+"/api/admin/v1/client-keys", token, http.MethodDelete, map[string]any{"ids": []string{keyID}})
			}
		}()
	}
	report := map[string]any{"requests": rows, "continuity": states, "full_application_restart": true}
	data, _ := json.MarshalIndent(report, "", "  ")
	if path := os.Getenv("GROK2API_HISTORY_LIVE_REPORT"); path != "" {
		if err = os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range states {
		t.Logf("continuity: %+v", state)
	}
}
