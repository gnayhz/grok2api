package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
)

// PlanRecovery does not write history. Even a readable summary cannot make an
// opaque downgrade lossless, so the execution owner must approve these steps.
func PlanRecovery(input historydomain.RecoveryInput) ([]historydomain.RecoveryStep, string) {
	if input.Rejection != historydomain.OpaqueDecodeRejected || input.Method != "POST" {
		return nil, "unsupported_rejection"
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(input.Body, &payload) != nil || payload == nil {
		return nil, "invalid_history"
	}
	var previous string
	if p, ok := payload["previous_response_id"]; ok && string(p) != "null" {
		if json.Unmarshal(p, &previous) != nil || strings.TrimSpace(previous) != "" {
			return nil, "incremental_history"
		}
	}
	portable, removed := stripRejectedOpaque(payload)
	steps := make([]historydomain.RecoveryStep, 0, 2)
	if removed > 0 {
		body, err := json.Marshal(portable)
		if err != nil {
			return nil, "invalid_history"
		}
		steps = append(steps, historydomain.RecoveryStep{Action: historydomain.RemoveRejectedOpaque, Body: body, RemovedOpaque: removed, InvalidateHistory: true})
	}
	if strings.TrimSpace(input.PromptCacheKey) != "" {
		delete(portable, "prompt_cache_key")
		body, err := json.Marshal(portable)
		if err != nil {
			return nil, "invalid_history"
		}
		steps = append(steps, historydomain.RecoveryStep{Action: historydomain.ClearUpstreamSessionHint, Body: body, ClearSessionHint: true})
	}
	if len(steps) == 0 {
		return nil, "no_portable_recovery"
	}
	return steps, ""
}

func stripRejectedOpaque(payload map[string]json.RawMessage) (map[string]json.RawMessage, int) {
	var input []json.RawMessage
	if json.Unmarshal(payload["input"], &input) != nil {
		return payload, 0
	}
	rebuilt := make([]json.RawMessage, 0, len(input))
	removed := 0
	for _, raw := range input {
		var item map[string]json.RawMessage
		var kind, encrypted string
		if json.Unmarshal(raw, &item) != nil {
			rebuilt = append(rebuilt, raw)
			continue
		}
		_ = json.Unmarshal(item["type"], &kind)
		_ = json.Unmarshal(item["encrypted_content"], &encrypted)
		if kind != "reasoning" || strings.TrimSpace(encrypted) == "" {
			rebuilt = append(rebuilt, raw)
			continue
		}
		delete(item, "encrypted_content")
		delete(item, "id")
		delete(item, "status")
		removed++
		if readableReasoning(item) {
			encoded, _ := json.Marshal(item)
			rebuilt = append(rebuilt, encoded)
		}
	}
	if removed > 0 {
		payload["input"], _ = json.Marshal(rebuilt)
	}
	return payload, removed
}

func readableReasoning(item map[string]json.RawMessage) bool {
	for _, field := range []string{"summary", "content"} {
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(item[field], &parts) != nil {
			continue
		}
		for _, part := range parts {
			if strings.TrimSpace(part.Text) != "" {
				return true
			}
		}
	}
	return false
}

// ApplyRecovery is called only after the execution owner reserves an attempt.
// Durable histories always require the rejected turn's generation token.
func (r *ReasoningReplay) ApplyRecovery(ctx context.Context, prepared historydomain.Prepared, model, key string, step historydomain.RecoveryStep) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !step.InvalidateHistory || r == nil || key == "" {
		return nil
	}
	if prepared != nil {
		turn, ok := prepared.(*PreparedHistory)
		if !ok || turn == nil || turn.replay != r {
			return fmt.Errorf("%w: foreign recovery turn", historydomain.ErrHistoryPrepare)
		}
		return turn.reset(ctx)
	}
	if r.Persistent() {
		return fmt.Errorf("%w: missing recovery generation", historydomain.ErrHistoryPrepare)
	}
	return r.Reset(ctx, model, key)
}
