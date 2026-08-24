package gateway

import (
	"encoding/json"
	"strings"
)

// Distinctive line from grok-build full_replace_summary_prompt.txt and the
// Grok TUI compaction request. Codex remote-v2 uses compaction_trigger
// instead; the TUI appends this prompt as a normal last user item.
const clientCompactionPromptMarker = "it is a system-generated compaction prompt, not a real user message"

// Compaction directives delivered as a plain final user message by agents
// that implement client-side compaction (no compaction_trigger item, no TUI
// marker). Such requests replay the conversation and append the directive
// as the last user item; the summary response carries no visible thinking
// by design, so an undetected directive falls into the quality hold and is
// wrongly withheld. Each entry is a stable opening phrase of one client's
// directive; matching any one classifies the request as compaction.
var clientCompactionPromptMarkers = []string{
	clientCompactionPromptMarker,
	// deepseek-harness (@deepseek-ai/dsh-compaction-basic COMPACTION_INSTRUCTION)
	"You are now acting as a compaction engine for this AI coding assistant",
	// Codex CLI local /compact (codex-rs/prompts templates/compact/prompt.md)
	"You are performing a CONTEXT CHECKPOINT COMPACTION",
}

type responsesCompactionKind uint8

const (
	responsesCompactionNone responsesCompactionKind = iota
	responsesCompactionTrigger
	responsesCompactionTUI
)

// isResponsesCompactionRequest detects a context-compaction turn without
// retaining the request body. Codex remote compaction v2 sends
// compaction_trigger; Grok TUI sends the canonical summary prompt as the
// last input/message item. The Provider adapter still requires the trigger
// before it rewrites the body into an encrypted compaction blob.
func isResponsesCompactionRequest(body []byte) bool {
	return classifyResponsesCompactionRequest(body) != responsesCompactionNone
}

func classifyResponsesCompactionRequest(body []byte) responsesCompactionKind {
	var payload struct {
		Input    []json.RawMessage `json:"input"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return responsesCompactionNone
	}
	if hasCompactionTrigger(payload.Input) {
		return responsesCompactionTrigger
	}
	if lastItemLooksLikeCompactionPrompt(payload.Input) || lastItemLooksLikeCompactionPrompt(payload.Messages) {
		return responsesCompactionTUI
	}
	return responsesCompactionNone
}

func hasCompactionTrigger(items []json.RawMessage) bool {
	for _, raw := range items {
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(item.Type), "compaction_trigger") {
			return true
		}
	}
	return false
}

func lastItemLooksLikeCompactionPrompt(items []json.RawMessage) bool {
	if len(items) == 0 {
		return false
	}
	var item struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(items[len(items)-1], &item) != nil || !strings.EqualFold(strings.TrimSpace(item.Role), "user") {
		return false
	}
	return looksLikeCompactionPrompt(extractContentText(item.Content))
}

func looksLikeCompactionPrompt(text string) bool {
	for _, marker := range clientCompactionPromptMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func extractContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var builder strings.Builder
		for _, part := range parts {
			builder.WriteString(part.Text)
		}
		return builder.String()
	}
	return ""
}
