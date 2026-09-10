package app

import (
	"bufio"
	"bytes"
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
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonvalue"
)

// TestLiveInferenceSnapshotMatrix is opt-in: its config and consistent SQLite
// snapshot must reside together as config.yaml and backend.db. It copies the
// database again into t.TempDir before constructing the real HTTP application.
// Only request-driven work runs; account refresh, proxy rotation and judicial
// maintenance workers are not started. No live deployment settings are changed.
func TestLiveInferenceSnapshotMatrix(t *testing.T) {
	configPath := os.Getenv("GROK2API_LIVE_SNAPSHOT_CONFIG")
	if configPath == "" {
		t.Skip("set GROK2API_LIVE_SNAPSHOT_CONFIG to opt into real upstream inference")
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
	path := filepath.Join(dir, "backend.db")
	if err := os.WriteFile(path, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Database.Driver, cfg.Database.SQLite.Path = "sqlite", path
	cfg.Audit.JournalDirectory = filepath.Join(filepath.Dir(path), "audit-pending")
	cfg.RuntimeStore.Driver = "memory"
	cfg.Media.Local.Path = filepath.Join(dir, "media")
	cfg.Deployment.Replicas, cfg.Deployment.InstanceID = 1, "inference-live-test"
	cfg.Frontend.StaticPath = ""
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	application, err := New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Error(err)
		}
	})
	if id, _ := strconv.ParseUint(os.Getenv("GROK2API_LIVE_COMPRESSION_ACCOUNT"), 10, 64); id != 0 {
		runLiveCompressionProbe(t, application, cfg, id)
		return
	}
	if id, _ := strconv.ParseUint(os.Getenv("GROK2API_LIVE_CACHE_ACCOUNT"), 10, 64); id != 0 {
		runLiveCacheProbe(t, application, cfg, id)
		return
	}
	application.startup.setPhase("running")
	if err := application.audits.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := application.audits.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(application.server.Handler)
	t.Cleanup(server.Close)
	baseURL := server.URL
	if target := os.Getenv("GROK2API_LIVE_TARGET_URL"); target != "" {
		baseURL = strings.TrimRight(target, "/")
		if os.Getenv("GROK2API_LIVE_BLOCKED_ACCOUNT") != "" {
			t.Fatal("blocked-account route mutation is restricted to snapshot tests")
		}
	}
	client := &http.Client{Timeout: 100 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	admin := liveJSON(t, client, baseURL+"/api/admin/v1/auth/login", "", http.MethodPost,
		map[string]any{"username": cfg.BootstrapAdmin.Username, "password": cfg.BootstrapAdmin.Password})
	token := admin["data"].(map[string]any)["tokens"].(map[string]any)["accessToken"].(string)
	var rows []liveInferenceRow
	var blockedRequestID string
	t.Cleanup(func() {
		if out := os.Getenv("GROK2API_LIVE_REPORT"); out != "" {
			data, err := json.MarshalIndent(rows, "", "  ")
			if err == nil {
				err = os.WriteFile(out, append(data, '\n'), 0600)
			}
			if err != nil {
				t.Error(err)
			}
		}
	})
	for _, channel := range []struct{ provider, model string }{
		{"grok_build", "Build/grok-4.6"},
		{"grok_web", "Web/grok-chat-fast"},
		{"grok_console", "Console/grok-4.3"},
	} {
		t.Run(channel.provider, func(t *testing.T) {
			key := liveJSON(t, client, baseURL+"/api/admin/v1/client-keys", token, http.MethodPost,
				map[string]any{"name": "inference-live-test", "enabled": true, "rpmLimit": 180, "maxConcurrent": 4, "providerScope": []string{channel.provider}, "allowModelAliases": true})["data"].(map[string]any)
			id := key["key"].(map[string]any)["id"].(string)
			t.Cleanup(func() {
				result := liveJSON(t, client, baseURL+"/api/admin/v1/client-keys", token, http.MethodDelete, map[string]any{"ids": []string{id}})
				if result["data"].(map[string]any)["deleted"] != float64(1) {
					t.Error("temporary key not deleted")
				}
			})
			for _, endpoint := range []struct{ operation, path string }{{"chat", "/v1/chat/completions"}, {"responses", "/v1/responses"}, {"messages", "/v1/messages"}} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%v", endpoint.operation, stream), func(t *testing.T) {
						const prompt = "Compute 17 * 19 carefully. Reply with only the result, 323."
						body := map[string]any{"model": channel.model, "stream": stream}
						if endpoint.operation == "responses" {
							body["input"], body["max_output_tokens"] = prompt, 2048
						} else {
							body["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
							if endpoint.operation == "messages" {
								body["max_tokens"] = 2048
								body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
							} else {
								body["max_completion_tokens"] = 2048
							}
						}
						row := liveInferenceRow{Provider: channel.provider, Model: channel.model, Operation: endpoint.operation, Streaming: stream}
						err := readLiveInference(client, baseURL+endpoint.path, key["secret"].(string), body, &row)
						rows = append(rows, row)
						encoded, _ := json.Marshal(row)
						t.Log(string(encoded))
						if err != nil {
							t.Error(err)
						} else if row.Status != 200 || !row.Terminal || !strings.Contains(row.Answer, "323") {
							t.Errorf("inference failed: status=%d terminal=%v answer=%q", row.Status, row.Terminal, row.Answer)
						}
					})
				}
			}
			if os.Getenv("GROK2API_LIVE_EXTENDED") == "1" {
				t.Run("workloads", func(t *testing.T) {
					runLiveWorkloads(t, client, baseURL, token, key["secret"].(string), channel.provider, channel.model, &rows)
				})
			}
			if blocked := os.Getenv("GROK2API_LIVE_BLOCKED_ACCOUNT"); blocked != "" && channel.provider == "grok_build" {
				t.Run("sentenced_account_excluded", func(t *testing.T) {
					accountID, err := strconv.ParseUint(blocked, 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					if application.quality.AccountEligible(accountID) {
						t.Fatal("expected blocked snapshot account is eligible")
					}
					const model = "Build/grok-4.6"
					routes := liveJSON(t, client, baseURL+"/api/admin/v1/models?pageSize=100", token, http.MethodGet, nil)
					var routeID string
					for _, raw := range routes["data"].(map[string]any)["items"].([]any) {
						route := raw.(map[string]any)
						if route["upstreamModel"] == model {
							routeID, _ = route["id"].(string)
						}
					}
					if routeID == "" {
						t.Fatal("Build route not found")
					}
					liveJSON(t, client, baseURL+"/api/admin/v1/models/"+routeID, token, http.MethodPatch, map[string]any{"accountIds": []string{blocked}})
					row := liveInferenceRow{Provider: channel.provider, Model: model, Operation: "chat"}
					_ = readLiveInference(client, baseURL+"/v1/chat/completions", key["secret"].(string), map[string]any{
						"model": model, "messages": []any{map[string]any{"role": "user", "content": "Reply 323"}},
					}, &row)
					if row.Status != http.StatusServiceUnavailable {
						t.Fatalf("blocked account admission status=%d", row.Status)
					}
					blockedRequestID = row.RequestID
					t.Logf("blocked account %d state=%s rejected with 503", accountID, application.quality.AccountState(accountID).State)
				})
			}
		})
	}
	// Closing the writer drains every completed request into the copied database
	// before checking the independent audit representation of each delivery.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer flushCancel()
	if err := application.audits.Close(flushCtx); err != nil {
		t.Fatal(err)
	}
	byRequest := make(map[string]map[string]any)
	deadline := time.Now().Add(5 * time.Second)
	for {
		listed := liveJSON(t, client, baseURL+"/api/admin/v1/request-audits?pageSize=100", token, http.MethodGet, nil)
		for _, item := range listed["data"].(map[string]any)["items"].([]any) {
			audit := item.(map[string]any)
			byRequest[audit["requestId"].(string)] = audit
		}
		complete := true
		for _, row := range rows {
			if byRequest[row.RequestID] == nil {
				complete = false
			}
		}
		if complete || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := range rows {
		row := &rows[i]
		audit := byRequest[row.RequestID]
		if audit == nil {
			t.Errorf("missing audit for %s", row.RequestID)
			continue
		}
		row.Audit = make(map[string]any)
		for _, field := range []string{"accountId", "egressNodeId", "statusCode", "errorCode", "attemptCount", "qualityRule", "qualityExempt", "deliveredBytes", "firstTokenMs", "reasoningTokens", "cachedInputTokens", "historyOutcome", "historyScopeHash", "historyGeneration", "historyRestoredItems", "historyNormalizer", "historyCommit"} {
			if value, ok := audit[field]; ok {
				row.Audit[field] = value
			}
		}
		if row.Status == 200 && !row.Cancelled && (audit["statusCode"] != float64(200) || audit["errorCode"] != nil && audit["errorCode"] != "" || audit["accountId"] == nil) {
			t.Errorf("delivery audit disagrees for %s: %#v", row.RequestID, row.Audit)
		}
	}
	if blockedRequestID != "" {
		audit := byRequest[blockedRequestID]
		if audit == nil || audit["statusCode"] != float64(503) || audit["accountId"] != nil {
			t.Errorf("blocked request audit did not confirm rejection before account submission: %v", audit != nil)
		} else {
			t.Log("blocked account audit: status=503, no submitted account")
		}
	}
}

type liveInferenceRow struct {
	Output          []any          `json:"-"`
	Scenario        string         `json:"scenario,omitempty"`
	Tools           []liveToolCall `json:"tools,omitempty"`
	CancelAfterText bool           `json:"-"`
	Cancelled       bool           `json:"cancelled,omitempty"`
	toolIndexes     map[string]int
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	Operation       string         `json:"operation"`
	Streaming       bool           `json:"streaming"`
	Status          int            `json:"status"`
	RequestID       string         `json:"request_id"`
	ElapsedMS       int64          `json:"elapsed_ms"`
	FirstDataMS     int64          `json:"first_data_ms,omitempty"`
	LastTextMS      int64          `json:"last_text_ms,omitempty"`
	FirstTextMS     int64          `json:"first_text_ms,omitempty"`
	Events          int            `json:"events"`
	EventTypes      map[string]int `json:"event_types,omitempty"`
	Answer          string         `json:"answer"`
	FinalAnswer     string         `json:"final_answer,omitempty"`
	Terminal        bool           `json:"terminal"`
	Usage           map[string]any `json:"usage,omitempty"`
	Audit           map[string]any `json:"audit,omitempty"`
}

func liveJSON(t *testing.T, client *http.Client, url, token, method string, body any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("%s %s returned %d", method, req.URL.Path, response.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func readLiveInference(client *http.Client, url, key string, body any, row *liveInferenceRow, headers ...http.Header) error {
	start := time.Now()
	defer func() { row.ElapsedMS = time.Since(start).Milliseconds() }()
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	for _, values := range headers {
		for name, value := range values {
			req.Header[name] = append([]string(nil), value...)
		}
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	row.Status, row.RequestID = response.StatusCode, response.Header.Get("X-Request-ID")
	if row.Status != 200 {
		return fmt.Errorf("upstream status %d", row.Status)
	}
	if !row.Streaming {
		var event map[string]any
		decoder := json.NewDecoder(response.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&event); err != nil {
			return err
		}
		row.observe(event)
		return nil
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("unexpected SSE content type %q", response.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if row.Events == 0 {
			row.FirstDataMS = time.Since(start).Milliseconds()
		}
		row.Events++
		if bytes.Equal(data, []byte("[DONE]")) {
			if row.Operation == "chat" {
				row.Terminal = true
			}
			continue
		}
		var event map[string]any
		if err := jsonvalue.Unmarshal(data, &event); err != nil {
			return err
		}
		if row.EventTypes == nil {
			row.EventTypes = make(map[string]int)
		}
		kind, _ := event["type"].(string)
		row.EventTypes[kind]++
		if event["error"] != nil || event["type"] == "error" || event["type"] == "response.failed" || event["type"] == "response.incomplete" {
			return fmt.Errorf("SSE failure event %v", event["type"])
		}
		previousLength := len(row.Answer)
		row.observe(event)
		if len(row.Answer) > previousLength {
			row.LastTextMS = time.Since(start).Milliseconds()
		}
		if row.Answer != "" && row.FirstTextMS == 0 {
			row.FirstTextMS = time.Since(start).Milliseconds()
		}
		if row.CancelAfterText && row.Answer != "" {
			row.Cancelled = true
			return nil
		}
	}
	return scanner.Err()
}

func (r *liveInferenceRow) observe(event map[string]any) {
	r.observeTools(event)
	if usage, ok := event["usage"].(map[string]any); ok {
		r.Usage = usage
	}
	text := func(value any) string { s, _ := value.(string); return s }
	content := func(value any) string {
		var result strings.Builder
		if parts, ok := value.([]any); ok {
			for _, part := range parts {
				if p, ok := part.(map[string]any); ok {
					result.WriteString(text(p["text"]))
				}
			}
		}
		return result.String()
	}
	switch r.Operation {
	case "chat":
		choices, _ := event["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				r.Answer += text(delta["content"])
			}
			if message, ok := choice["message"].(map[string]any); ok {
				r.Answer += text(message["content"])
				r.Terminal = choice["finish_reason"] == "stop" || choice["finish_reason"] == "tool_calls"
			}
		}
	case "responses":
		if r.Streaming {
			if event["type"] == "response.output_text.delta" {
				r.Answer += text(event["delta"])
			}
			if event["type"] == "response.completed" {
				r.Terminal = true
				if response, ok := event["response"].(map[string]any); ok {
					r.Usage, _ = response["usage"].(map[string]any)
					output, _ := response["output"].([]any)
					r.Output = output
					for _, raw := range output {
						if item, ok := raw.(map[string]any); ok && item["type"] == "message" {
							r.FinalAnswer += content(item["content"])
						}
					}
				}
			}
		} else {
			output, _ := event["output"].([]any)
			r.Output = output
			for _, raw := range output {
				if item, ok := raw.(map[string]any); ok && item["type"] == "message" {
					r.Answer += content(item["content"])
				}
			}
			r.Terminal = event["status"] == "completed"
		}
	case "messages":
		if !r.Streaming {
			r.Answer = content(event["content"])
			r.Terminal = event["stop_reason"] == "end_turn" || event["stop_reason"] == "tool_use"
		} else {
			if delta, ok := event["delta"].(map[string]any); ok {
				r.Answer += text(delta["text"])
			}
			r.Terminal = r.Terminal || event["type"] == "message_stop"
		}
	}
}
