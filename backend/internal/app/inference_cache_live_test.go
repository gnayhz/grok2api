package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/google/uuid"
)

// Opt-in upstream experiment. Both arms retain the complete output and use the
// same credential and egress lease. Reports usage, never tokens or user data.
func runLiveCacheProbe(t *testing.T, application *Application, cfg config.Config, accountID uint64) {
	if os.Getenv("GROK2API_LIVE_CACHE_DIAGNOSTIC") != "" {
		runLiveCacheTransportProbe(t, application, cfg, accountID)
		return
	}
	credential, err := application.accountRepo.Get(t.Context(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewVersionedCipher(cfg.Secrets.CredentialEncryptionKey, cfg.Secrets.LegacyEncryptionKeys)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := application.egress.AcquireCredential(t.Context(), domainegress.ScopeBuild, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	agent := uuid.NewString()
	for _, ping := range []bool{false, true} {
		if os.Getenv("GROK2API_LIVE_CACHE_NO_PING_ONLY") == "1" && ping {
			continue
		}
		key := uuid.NewString()
		history := []any{map[string]any{"type": "message", "role": "user", "content": "For a cache experiment, explain why stable conversation prefixes help prompt caching in about 80 words."}}
		if os.Getenv("GROK2API_LIVE_CACHE_LONG_PREFIX") == "1" {
			prefix := strings.Repeat("Reference: Preserve stable conversation prefixes, ordered tool results, and encrypted reasoning across turns. ", 180)
			if os.Getenv("GROK2API_LIVE_CACHE_UNIQUE_PREFIX") == "1" {
				prefix = "Experiment " + key + ".\n" + prefix
			}
			history = append([]any{map[string]any{"type": "message", "role": "system", "content": prefix}}, history...)
		}
		post := func(input []any, turn int, limit int) map[string]any {
			payload := map[string]any{"model": "grok-4.6", "input": input, "prompt_cache_key": key, "store": false, "stream": false, "include": []string{"reasoning.encrypted_content"}, "reasoning": map[string]any{"effort": "low", "summary": "detailed"}, "tools": []any{map[string]any{"type": "web_search"}, map[string]any{"type": "x_search"}}, "tool_choice": "none", "max_output_tokens": limit}
			if os.Getenv("GROK2API_LIVE_CACHE_NO_TOOLS") == "1" {
				delete(payload, "tools")
				delete(payload, "tool_choice")
			}
			body, _ := json.Marshal(payload)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.Provider.Build.BaseURL, "/")+"/responses", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"Authorization": "Bearer " + token, "X-XAI-Token-Auth": cfg.Provider.Build.TokenAuth, "x-grok-client-version": cfg.Provider.Build.ClientVersion, "x-grok-client-identifier": cfg.Provider.Build.ClientIdentifier, "x-grok-client-mode": "headless", "User-Agent": cfg.Provider.Build.UserAgent, "Content-Type": "application/json", "Accept": "application/json", "x-grok-model-override": "grok-4.6", "x-grok-user-id": credential.UserID, "x-grok-agent-id": agent, "x-grok-conv-id": key, "x-grok-session-id": key, "x-grok-req-id": uuid.NewString()} {
				req.Header.Set(k, v)
			}
			if turn > 0 && os.Getenv("GROK2API_LIVE_CACHE_OMIT_TURN_INDEX") != "1" {
				req.Header.Set("x-grok-turn-idx", strconv.Itoa(turn))
			}
			started := time.Now()
			resp, err := lease.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var result map[string]any
			err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result)
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("cache probe status=%d decode=%v", resp.StatusCode, err)
			}
			usage, _ := json.Marshal(result["usage"])
			t.Logf("ping_arm=%v turn=%d elapsed_ms=%d usage=%s", ping, turn, time.Since(started).Milliseconds(), usage)
			return result
		}
		for turn := 1; turn <= 5; turn++ {
			if ping {
				post([]any{map[string]any{"type": "message", "role": "user", "content": "ping"}}, 0, 16)
			}
			result := post(history, turn, 1200)
			if result["status"] != "completed" {
				t.Fatalf("turn %d did not complete", turn)
			}
			output, ok := result["output"].([]any)
			if !ok || len(output) == 0 {
				t.Fatal("missing output")
			}
			if os.Getenv("GROK2API_LIVE_CACHE_NATIVE_INPUT") == "1" {
				for i, raw := range output {
					item := raw.(map[string]any)
					if item["type"] == "reasoning" {
						delete(item, "status")
					}
					if item["type"] == "message" {
						parts, _ := item["content"].([]any)
						var texts []string
						for _, part := range parts {
							p := part.(map[string]any)
							if p["type"] == "output_text" {
								texts = append(texts, p["text"].(string))
							}
						}
						output[i] = map[string]any{"type": "message", "role": "assistant", "content": strings.Join(texts, "\n")}
					}
				}
			}
			history = append(history, output...)
			history = append(history, map[string]any{"type": "message", "role": "user", "content": "Continue with one concrete example in about 80 words. Example number " + strconv.Itoa(turn) + "."})
		}
	}
}
