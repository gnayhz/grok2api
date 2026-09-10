package cli

import (
	_ "embed"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonvalue"
)

const (
	minGatewayCompactionRunes    = 500
	gatewayCompactionMaxAttempts = 3
)

// Generated from xai-org/grok-build's full_replace_summary_prompt.txt using
// build_summary_prompt(None), so the optional {user_context_section} slot is
// empty. The Grok Build source confirms that this text is appended as
// the final user item for every compaction attempt.
//
//go:embed responses_compaction_prompt.txt
var gatewayCompactionPrompt string

// prepareGatewayCompactionSample mirrors Grok Build full-replace
// sampling: normal /responses SSE, instructions=null, tools and explicit choice
// retained, concise reasoning summary, and the canonical final user prompt.
// Only an absent choice uses the upstream default auto selection.
func prepareGatewayCompactionSample(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := jsonvalue.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	items, ok := payload["input"].([]any)
	if !ok {
		return nil, &responsesRequestError{Message: "compaction 请求的 input 必须是数组", Param: "input", Code: "invalid_parameter"}
	}
	items = append(items, map[string]any{
		"type": "message", "role": "user", "content": gatewayCompactionPrompt,
	})
	payload["input"] = items
	payload["instructions"] = nil
	payload["stream"] = true
	payload["store"] = false
	payload["temperature"] = 1.0
	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		// The protocol default applies only when the client did not select a
		// mode. Normalization has already validated explicit choices and narrowed
		// hosted tools; summary sampling must keep that execution permission.
		if payload["tool_choice"] == nil {
			payload["tool_choice"] = "auto"
		}
	} else {
		delete(payload, "tool_choice")
	}
	payload["reasoning"] = map[string]any{"summary": "concise"}
	for _, field := range []string{
		"previous_response_id", "text", "response_format", "max_output_tokens", "max_completion_tokens",
	} {
		delete(payload, field)
	}
	return json.Marshal(payload)
}

func cleanGatewayCompactionSummary(raw string) string {
	result := strings.TrimSpace(raw)
	for {
		start := strings.Index(result, "<analysis>")
		if start < 0 {
			break
		}
		summaryStart := strings.Index(result, "<summary>")
		leading := summaryStart < 0 && strings.TrimSpace(result[:start]) == ""
		if summaryStart >= 0 {
			leading = start < summaryStart || strings.TrimSpace(result[summaryStart+len("<summary>"):start]) == ""
		}
		if !leading {
			break
		}
		endRel := strings.Index(result[start:], "</analysis>")
		if endRel < 0 {
			if nextSummary := strings.Index(result[start:], "<summary>"); nextSummary >= 0 {
				result = result[:start] + result[start+nextSummary:]
			} else {
				result = result[:start]
			}
			break
		}
		end := start + endRel + len("</analysis>")
		result = result[:start] + result[end:]
	}
	if start := strings.Index(result, "<summary>"); start >= 0 {
		if end := strings.LastIndex(result, "</summary>"); end > start {
			before := result[:start]
			inner := stripLeadingGatewayCompactionScratchpad(result[start+len("<summary>") : end])
			after := result[end+len("</summary>"):]
			result = before + "Summary:\n" + inner + after
		}
	}
	result = neutralizeGatewayCompactionTags(result)
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(result)
}

// stripLeadingGatewayCompactionScratchpad mirrors grok-build's summary
// cleaner for the common malformed shape where the model emits an untagged
// markdown analysis followed by an orphan </analysis> inside <summary>.
// Numbered summaries are left intact even when they quote that token later.
func stripLeadingGatewayCompactionScratchpad(inner string) string {
	result := strings.TrimSpace(inner)
	lead := strings.TrimLeft(result, "#*-> \t")
	startsWithNumber := len(lead) > 0 && lead[0] >= '0' && lead[0] <= '9'
	if !startsWithNumber {
		if end := strings.LastIndex(result, "</analysis>"); end >= 0 {
			result = strings.TrimSpace(result[end+len("</analysis>"):])
		}
	}
	if strings.HasPrefix(result, "<summary>") {
		result = strings.TrimSpace(strings.TrimPrefix(result, "<summary>"))
	}
	return result
}

func neutralizeGatewayCompactionTags(text string) string {
	for _, tag := range []string{"</summary>", "<summary>", "</analysis>", "<analysis>", "</summary_request>", "<summary_request>"} {
		text = strings.ReplaceAll(text, tag, "<\u200b"+strings.TrimPrefix(tag, "<"))
	}
	return text
}

func isDegenerateGatewayCompactionSummary(summary string) bool {
	cleaned := cleanGatewayCompactionSummary(summary)
	return cleaned == "" || utf8.RuneCountInString(cleaned) < minGatewayCompactionRunes
}

func gatewayCompactionContinuation(raw string) string {
	return "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n" + cleanGatewayCompactionSummary(raw)
}

func compactionPreparationError(err error) error {
	return &responsesRequestError{Message: err.Error(), Param: "input", Code: "compaction_history_unavailable"}
}
