package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func sseData(payload string) string {
	return "data: " + payload + "\n\n"
}

func TestQualityHoldFingerprintMatchesRealDaSignature(t *testing.T) {
	t.Parallel()
	cipher := strings.Repeat("A", 256)
	done := fmt.Sprintf("{\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"%s\"}}", cipher)
	degraded := sseData("{\"type\":\"response.created\",\"response\":{\"id\":\"resp_deg\"}}") +
		sseData("{\"type\":\"response.in_progress\"}") +
		sseData("{\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}") +
		sseData(done)
	replay, verdict, _, fp, err := peekQualityStreamReport(context.Background(), io.NopCloser(strings.NewReader(degraded)), qualityProtocolResponses, QualityRetryRuntime{
		EvidenceTimeout: 3500 * time.Millisecond, CreatedTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("verdict = %s, want withhold", verdict)
	}
	if fp.Rule != "item_done" || !fp.ReasoningEnded || fp.HasThinking || !fp.Encrypted {
		t.Fatalf("fingerprint rule/flags = %+v", fp)
	}
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done"}
	if len(fp.Events) < len(want) {
		t.Fatalf("events = %v, want prefix %v", fp.Events, want)
	}
	for i, typ := range want {
		if fp.Events[i] != typ {
			t.Fatalf("events = %v, want prefix %v", fp.Events, want)
		}
	}
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	response := &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Cf-Ray": {"abc-SJC"}, "Content-Type": {"text/event-stream"}}}
	recorder.captureQualityDegraded(account.Credential{ID: 7, Name: "probe"}, time.Now().Add(-2*time.Millisecond), response, fp)
	stored := recorder.snapshot()
	if len(stored) != 1 || stored[0].Stage != "quality_hold" || len(stored[0].ResponseBody) == 0 {
		t.Fatalf("attempt = %#v", stored)
	}
	if stored[0].ResponseHeaders["Cf-Ray"][0] != "abc-SJC" {
		t.Fatalf("headers = %#v", stored[0].ResponseHeaders)
	}
	var got qualityHoldFingerprint
	if err := json.Unmarshal(stored[0].ResponseBody, &got); err != nil {
		t.Fatalf("fingerprint json: %v body=%s", err, stored[0].ResponseBody)
	}
	if got.Rule != "item_done" || !got.Encrypted || got.Events[0] != "response.created" {
		t.Fatalf("stored fingerprint = %+v", got)
	}
}

func TestQualityHoldFingerprintCleanThinking(t *testing.T) {
	t.Parallel()
	clean := sseData("{\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\"}}") +
		sseData("{\"type\":\"response.in_progress\"}") +
		sseData("{\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}") +
		sseData("{\"type\":\"response.reasoning_summary_part.added\"}") +
		sseData("{\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"step\"}")
	replay, verdict, _, fp, err := peekQualityStreamReport(context.Background(), io.NopCloser(strings.NewReader(clean)), qualityProtocolResponses, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("verdict = %s, want deliver", verdict)
	}
	if fp.Rule != "thinking" || !fp.HasThinking || fp.ReasoningEnded {
		t.Fatalf("fingerprint = %+v", fp)
	}
	found := false
	for _, typ := range fp.Events {
		if typ == "response.reasoning_summary_text.delta" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events = %v, missing summary delta", fp.Events)
	}
}
