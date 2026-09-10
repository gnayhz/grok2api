package history

import (
	"encoding/json"
	"testing"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
)

func TestRecoveryPlanPreservesPortableHistory(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"reasoning","id":"rs_empty","status":"completed","summary":[],"encrypted_content":"opaque-empty"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":""}],"encrypted_content":"opaque-blank"},
			{"type":"reasoning","id":"rs_summary","status":"completed","summary":[{"type":"summary_text","text":"readable"}],"encrypted_content":"opaque-summary"},
			{"type":"message","role":"assistant","content":"answer","encrypted_content":"message-value"},
			{"type":"message","role":"user","content":"continue"}
		]
	}`)
	steps, reason := PlanRecovery(historydomain.RecoveryInput{Rejection: historydomain.OpaqueDecodeRejected, Method: "POST", Body: body})
	if reason != "" || len(steps) != 1 {
		t.Fatal("expected encrypted reasoning downgrade")
	}
	downgraded := steps[0].Body
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	if json.Unmarshal(downgraded, &payload) != nil || len(payload.Input) != 3 {
		t.Fatalf("downgraded = %s", downgraded)
	}
	reasoning := payload.Input[0]
	if reasoning["type"] != "reasoning" || reasoning["id"] != nil || reasoning["status"] != nil || reasoning["encrypted_content"] != nil {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	if payload.Input[1]["encrypted_content"] != "message-value" {
		t.Fatalf("non-reasoning encrypted content changed: %#v", payload.Input[1])
	}
}

func TestRecoveryPlanNeverTreatsIncrementalHistoryAsFullInput(t *testing.T) {
	for _, previous := range []string{`"seed"`, `42`, `{}`} {
		input := historydomain.RecoveryInput{Rejection: historydomain.OpaqueDecodeRejected, Method: "POST", PromptCacheKey: "cache", Body: []byte(`{"previous_response_id":` + previous + `,"input":[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"next"}]}`)}
		steps, reason := PlanRecovery(input)
		if len(steps) != 0 || reason != "incremental_history" {
			t.Fatalf("steps=%v reason=%s", steps, reason)
		}
	}
}

func TestRecoveryPlanRetainsExactPortableNumbersAndDoesNotResetForHintAlone(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"continue"}],"metadata":{"large":9007199254740993},"prompt_cache_key":"old"}`)
	steps, reason := PlanRecovery(historydomain.RecoveryInput{Rejection: historydomain.OpaqueDecodeRejected, Method: "POST", PromptCacheKey: "old", Body: body})
	if reason != "" || len(steps) != 1 || steps[0].InvalidateHistory || !steps[0].ClearSessionHint {
		t.Fatalf("steps=%+v reason=%s", steps, reason)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(steps[0].Body, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["metadata"]) != `{"large":9007199254740993}` {
		t.Fatalf("portable number changed: %s", payload["metadata"])
	}
	if string(body) != `{"input":[{"role":"user","content":"continue"}],"metadata":{"large":9007199254740993},"prompt_cache_key":"old"}` {
		t.Fatal("planning mutated input")
	}
}
