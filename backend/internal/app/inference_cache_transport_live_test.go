package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/google/uuid"
)

// Repeats an identical, unique long prompt to distinguish upstream cache
// retention/routing from history conversion. Reports timing and safe metadata.
func runLiveCacheTransportProbe(t *testing.T, app *Application, cfg config.Config, id uint64) {
	credential, err := app.accountRepo.Get(t.Context(), id)
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
	key, agent := uuid.NewString(), uuid.NewString()
	var cookies []*http.Cookie
	ctx := infraegress.WithBuildSession(t.Context(), key)
	if node, err := strconv.ParseUint(os.Getenv("GROK2API_LIVE_CACHE_NODE"), 10, 64); err == nil && node > 0 {
		ctx = infraegress.WithPinnedNode(ctx, node)
	}
	lease, err := app.egress.AcquireCredential(ctx, domainegress.ScopeBuild, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	var prefix strings.Builder
	fmt.Fprintf(&prefix, "Experiment %s. Reference records follow.\n", key)
	records := 1500
	if n, err := strconv.Atoi(os.Getenv("GROK2API_LIVE_CACHE_RECORDS")); err == nil && n > 0 && n <= 20000 {
		records = n
	}
	for i := 0; i < records; i++ {
		fmt.Fprintf(&prefix, "Record %04d: code %d; category %d.\n", i, i*17+3, i%23)
	}
	payload := map[string]any{"model": "grok-4.6", "input": []any{map[string]any{"type": "message", "role": "system", "content": prefix.String()}, map[string]any{"type": "message", "role": "user", "content": "Reply exactly CACHE_DIAG_OK."}}, "prompt_cache_key": key, "stream": true, "store": false, "include": []string{"reasoning.encrypted_content"}, "reasoning": map[string]any{"effort": "low", "summary": "concise"}, "max_output_tokens": 256}
	body, _ := json.Marshal(payload)
	mode := os.Getenv("GROK2API_LIVE_CACHE_DIAGNOSTIC")
	turns := 8
	if n, err := strconv.Atoi(os.Getenv("GROK2API_LIVE_CACHE_TURNS")); err == nil && n > 0 && n <= 40 {
		turns = n
	}
	// Cached input can still count toward the upstream subscription allowance.
	// Keep this opt-in diagnostic bounded even when both sizing knobs are set.
	plan := make([]liveCacheTransportStep, turns)
	for i := range plan {
		plan[i] = liveCacheTransportStep{body: body, key: key, label: "repeat", records: records}
	}
	if mode == "branches" {
		plan = liveCacheBranchPlan(payload, records)
	}
	if mode == "length-paired" {
		plan = liveCacheLengthPlan(payload, records)
	}
	if mode == "key-routing" {
		plan = liveCacheKeyPlan(payload, records)
	}
	estimated := 0
	for _, step := range plan {
		estimated += step.records*16 + 256
	}
	if estimated > 200000 {
		t.Fatal("diagnostic exceeds the conservative 200000 input-token budget; reduce records or turns")
	}
	for index, step := range plan {
		turn := index + 1
		if turn > 1 {
			select {
			case <-time.After(8 * time.Second):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		started := time.Now()
		var reused atomic.Bool
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) }}
		callCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, trace), 120*time.Second)
		req, err := http.NewRequestWithContext(callCtx, "POST", strings.TrimRight(cfg.Provider.Build.BaseURL, "/")+"/responses", bytes.NewReader(step.body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range map[string]string{"Authorization": "Bearer " + token, "X-XAI-Token-Auth": cfg.Provider.Build.TokenAuth, "x-grok-client-version": cfg.Provider.Build.ClientVersion, "x-grok-client-identifier": cfg.Provider.Build.ClientIdentifier, "x-grok-client-mode": "headless", "User-Agent": cfg.Provider.Build.UserAgent, "Content-Type": "application/json", "Accept": "text/event-stream", "Accept-Encoding": "identity", "x-grok-model-override": "grok-4.6", "x-grok-user-id": credential.UserID, "x-grok-agent-id": agent, "x-grok-req-id": uuid.NewString()} {
			req.Header.Set(k, v)
		}
		sendCookies := mode == "cookies" || mode == "paired-cookies" && turn%2 == 0
		if sendCookies {
			for _, cookie := range cookies {
				req.AddCookie(cookie)
			}
		}
		if mode != "body-only" && step.key != "" {
			req.Header.Set("x-grok-conv-id", step.key)
			req.Header.Set("x-grok-session-id", step.key)
			req.Header.Set("x-grok-turn-idx", strconv.Itoa(turn))
		}
		response, err := lease.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		row := map[string]any{"mode": mode, "turn": turn, "account": id, "node": lease.NodeID, "status": response.StatusCode, "headers_ms": time.Since(started).Milliseconds(), "connection_reused": reused.Load(), "protocol": response.Proto}
		safeHeaders := map[string]string{}
		for _, name := range []string{"Server", "CF-Ray", "Server-Timing", "X-Models-Etag", "X-Grok-Model", "X-Grok-Model-Id", "X-Request-Id", "X-Cache", "X-Grok-Context-Window"} {
			if v := response.Header.Get(name); v != "" {
				safeHeaders[name] = v
			}
		}
		row["headers"] = safeHeaders
		for _, name := range []string{"Retry-After", "X-Should-Retry", "X-Ratelimit-Limit-Tokens", "X-Ratelimit-Remaining-Tokens", "X-Ratelimit-Limit-Requests", "X-Ratelimit-Remaining-Requests"} {
			if v := response.Header.Get(name); v != "" {
				safeHeaders[name] = v
			}
		}
		row["body_sha256"] = fmt.Sprintf("%x", sha256.Sum256(step.body))
		if step.key != "" {
			row["cache_key_sha256"] = fmt.Sprintf("%x", sha256.Sum256([]byte(step.key)))
		}
		row["step"] = step.label
		row["started_at"] = started.UTC().Format(time.RFC3339Nano)
		safeHeaders["Date"] = response.Header.Get("Date")
		var names []string
		for _, cookie := range response.Cookies() {
			names = append(names, cookie.Name)
		}
		row["cookie_names"] = names
		row["sent_response_cookies"] = sendCookies && len(cookies) > 0
		if (mode == "cookies" || mode == "paired-cookies") && len(response.Cookies()) > 0 {
			cookies = response.Cookies()
		}
		var headerNames []string
		for name := range response.Header {
			headerNames = append(headerNames, name)
		}
		if turn == 1 {
			row["header_names"] = headerNames
		}
		scanner := bufio.NewScanner(io.LimitReader(response.Body, 16<<20))
		scanner.Buffer(make([]byte, 4096), 8<<20)
		completed := false
		if response.StatusCode != 200 {
			data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			var failure struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(data, &failure)
			row["error_code"] = failure.Code
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			raw := bytes.TrimSpace(line[5:])
			if bytes.Equal(raw, []byte("[DONE]")) {
				continue
			}
			var event map[string]any
			if json.Unmarshal(raw, &event) != nil {
				continue
			}
			if _, exists := row["first_event_ms"]; !exists {
				row["first_event_ms"] = time.Since(started).Milliseconds()
			}
			kind, _ := event["type"].(string)
			if kind == "response.reasoning_summary_text.delta" {
				if _, ok := row["first_reasoning_ms"]; !ok {
					row["first_reasoning_ms"] = time.Since(started).Milliseconds()
				}
			}
			if kind == "response.output_text.delta" {
				if _, ok := row["first_text_ms"]; !ok {
					row["first_text_ms"] = time.Since(started).Milliseconds()
				}
			}
			if kind == "response.completed" {
				completed = true
				r, _ := event["response"].(map[string]any)
				row["usage"] = r["usage"]
				row["model"] = r["model"]
				if m, ok := r["metadata"].(map[string]any); ok {
					row["fingerprint"] = m["system_fingerprint"]
				}
			}
		}
		response.Body.Close()
		cancel()
		row["elapsed_ms"] = time.Since(started).Milliseconds()
		row["completed"] = completed
		encoded, _ := json.Marshal(row)
		t.Log(string(encoded))
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		if !completed {
			t.Fatal("missing completion")
		}
	}
}

// The main request is byte-identical throughout. Short requests differ only in
// their routing identity between arms, and ABBA ordering reduces time confounding.
// This experiment does not simulate history replay or prove upstream causality.
type liveCacheTransportStep struct {
	body    []byte
	key     string
	label   string
	records int
}

func liveCacheBranchPlan(main map[string]any, records int) []liveCacheTransportStep {
	mainBody, _ := json.Marshal(main)
	mainKey := main["prompt_cache_key"].(string)
	branchKey := uuid.NewString()
	shortRecords := max(1, records/4)
	var prefix strings.Builder
	fmt.Fprintf(&prefix, "Independent auxiliary task %s.\n", branchKey)
	for i := 0; i < shortRecords; i++ {
		fmt.Fprintf(&prefix, "Record %04d: code %d; category %d.\n", i, i*31+7, i%19)
	}
	short := make(map[string]any, len(main))
	for k, v := range main {
		short[k] = v
	}
	short["input"] = []any{map[string]any{"type": "message", "role": "system", "content": prefix.String()}, map[string]any{"type": "message", "role": "user", "content": "Reply exactly AUX_DIAG_OK."}}
	mainStep := func(label string) liveCacheTransportStep {
		return liveCacheTransportStep{mainBody, mainKey, label, records}
	}
	plan := []liveCacheTransportStep{mainStep("warmup"), mainStep("warmup"), mainStep("warmup")}
	for _, arm := range []string{"shared", "isolated", "isolated", "shared", "isolated", "shared", "shared", "isolated"} {
		key := mainKey
		if arm == "isolated" {
			key = branchKey
		}
		short["prompt_cache_key"] = key
		body, _ := json.Marshal(short)
		plan = append(plan, mainStep(arm+"/before"), liveCacheTransportStep{body, key, arm + "/aux", shortRecords}, mainStep(arm+"/after"))
	}
	return plan
}

// Both lengths share a literal system-text prefix and one routing identity.
// Once the long input is warm, the short request should be eligible for that
// prefix too; neither key changes nor independent content explain a miss.
func liveCacheLengthPlan(main map[string]any, records int) []liveCacheTransportStep {
	key := main["prompt_cache_key"].(string)
	longBody, _ := json.Marshal(main)
	shortRecords := max(1, records/5)
	var prefix strings.Builder
	fmt.Fprintf(&prefix, "Experiment %s. Reference records follow.\n", key)
	for i := 0; i < shortRecords; i++ {
		fmt.Fprintf(&prefix, "Record %04d: code %d; category %d.\n", i, i*17+3, i%23)
	}
	short := make(map[string]any, len(main))
	for k, v := range main {
		short[k] = v
	}
	short["input"] = []any{map[string]any{"type": "message", "role": "system", "content": prefix.String()}, map[string]any{"type": "message", "role": "user", "content": "Reply exactly CACHE_DIAG_OK."}}
	shortBody, _ := json.Marshal(short)
	var plan []liveCacheTransportStep
	for _, long := range []bool{false, true, true, false, false, true, true, false} {
		step := liveCacheTransportStep{shortBody, key, "short", shortRecords}
		if long {
			step = liveCacheTransportStep{longBody, key, "long", records}
		}
		plan = append(plan, step)
	}
	return plan
}

// Stable routing vs fresh routing keys vs no key, with identical input in all
// three arms. Unique-prefix negative controls check the meaning of cache usage.
func liveCacheKeyPlan(main map[string]any, records int) []liveCacheTransportStep {
	key := main["prompt_cache_key"].(string)
	base, _ := json.Marshal(main)
	plan := []liveCacheTransportStep{{base, key, "warmup", records}, {base, key, "warmup", records}, {base, key, "warmup", records}}
	for cycle := 0; cycle < 3; cycle++ {
		for _, mode := range []string{"stable", "rotated", "absent", "absent", "rotated", "stable"} {
			payload := make(map[string]any, len(main))
			for k, v := range main {
				payload[k] = v
			}
			cache := key
			if mode == "rotated" {
				cache = uuid.NewString()
				payload["prompt_cache_key"] = cache
			}
			if mode == "absent" {
				cache = ""
				delete(payload, "prompt_cache_key")
			}
			body, _ := json.Marshal(payload)
			plan = append(plan, liveCacheTransportStep{body, cache, mode, records})
		}
	}
	for i := 0; i < 2; i++ {
		payload := make(map[string]any, len(main))
		for k, v := range main {
			payload[k] = v
		}
		var prefix strings.Builder
		fmt.Fprintf(&prefix, "Negative control %s.\n", uuid.NewString())
		for j := 0; j < records; j++ {
			fmt.Fprintf(&prefix, "Record %04d: code %d; category %d.\n", j, j*17+3, j%23)
		}
		payload["input"] = []any{map[string]any{"type": "message", "role": "system", "content": prefix.String()}, map[string]any{"type": "message", "role": "user", "content": "Reply exactly CACHE_DIAG_OK."}}
		body, _ := json.Marshal(payload)
		plan = append(plan, liveCacheTransportStep{body, key, "unique-prefix", records})
	}
	return plan
}

func TestLiveCacheLengthPlanControls(t *testing.T) {
	key := "experiment-key"
	var prefix strings.Builder
	fmt.Fprintf(&prefix, "Experiment %s. Reference records follow.\n", key)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&prefix, "Record %04d: code %d; category %d.\n", i, i*17+3, i%23)
	}
	main := map[string]any{"input": []any{map[string]any{"type": "message", "role": "system", "content": prefix.String()}, map[string]any{"type": "message", "role": "user", "content": "Reply exactly CACHE_DIAG_OK."}}, "prompt_cache_key": key, "stream": true}
	plan := liveCacheLengthPlan(main, 100)
	if len(plan) != 8 {
		t.Fatal("unexpected schedule")
	}
	bodies := map[string]string{}
	for _, step := range plan {
		if step.key != key {
			t.Fatal("length experiment changed routing identity")
		}
		if old, ok := bodies[step.label]; ok && old != string(step.body) {
			t.Fatal("repeated length body changed")
		}
		bodies[step.label] = string(step.body)
		var p struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(step.body, &p); err != nil {
			t.Fatal(err)
		}
		if step.label == "short" && !strings.HasPrefix(prefix.String(), p.Input[0].Content) {
			t.Fatal("short system text is not long system prefix")
		}
	}
	keyPlan := liveCacheKeyPlan(main, 100)
	var inputHash string
	rotated := map[string]bool{}
	for _, step := range keyPlan {
		var p map[string]json.RawMessage
		if err := json.Unmarshal(step.body, &p); err != nil {
			t.Fatal(err)
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(p["input"]))
		if inputHash == "" {
			inputHash = hash
		}
		if step.label != "unique-prefix" && hash != inputHash {
			t.Fatal("key experiment changed input")
		}
		if step.label == "unique-prefix" && hash == inputHash {
			t.Fatal("negative control reused input")
		}
		if step.label == "rotated" {
			if rotated[step.key] {
				t.Fatal("rotation reused key")
			}
			rotated[step.key] = true
		}
		if step.label == "absent" && (step.key != "" || p["prompt_cache_key"] != nil) {
			t.Fatal("absent-key control still routes explicitly")
		}
	}
}
