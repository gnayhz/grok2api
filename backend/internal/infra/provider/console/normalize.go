package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/xaitools"
)

func normalizeRequest(body []byte, spec ModelSpec) ([]byte, error) {
	return normalizeRequestWithMetadata(body, spec, nil)
}

func normalizeRequestWithMetadata(body []byte, spec ModelSpec, metadata *provider.NormalizedRequestMetadata) ([]byte, error) {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	// Tool schemas and structured-output constraints may contain integer IDs
	// beyond float64 precision. Preserve their original numeric representation.
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析 Console Responses 请求: %w", err)
	}
	if payload == nil || len(bytes.TrimSpace(body[decoder.InputOffset():])) != 0 {
		return nil, fmt.Errorf("解析 Console Responses 请求: 必须是单个 JSON 对象")
	}
	payload["model"] = spec.UpstreamModel
	// Console is stateless. Replay the supplied input and silently discard
	// stateful client hints instead of rejecting an otherwise valid request.
	payload["store"] = false
	for _, field := range []string{
		"metadata", "previous_response_id", "service_tier", "prompt_cache_key",
		"background", "conversation",
	} {
		delete(payload, field)
	}
	normalizeConsoleResponseFormat(payload)
	patchConsoleInput(payload)
	if _, exists := payload["max_output_tokens"]; !exists && spec.MaxOutputTokens > 0 {
		payload["max_output_tokens"] = spec.MaxOutputTokens
	}
	requestedEffort := metadata != nil && auditdomain.NormalizeReasoningEffort(metadata.ReasoningEffort) != ""
	requestedEffort = requestedEffort || hasRecognizedConsoleReasoningEffort(payload)
	normalizeReasoning(payload, spec)
	updateConsoleReasoningMetadata(payload, spec, requestedEffort, metadata)
	ensureReasoningInclude(payload)
	if err := normalizeConsoleTools(payload); err != nil {
		return nil, err
	}
	return json.Marshal(payload)
}

func hasRecognizedConsoleReasoningEffort(payload map[string]any) bool {
	reasoning, _ := payload["reasoning"].(map[string]any)
	effort, _ := reasoning["effort"].(string)
	if strings.EqualFold(strings.TrimSpace(effort), "auto") {
		return true
	}
	return normalizeEffort(effort) != ""
}

func updateConsoleReasoningMetadata(payload map[string]any, spec ModelSpec, requested bool, metadata *provider.NormalizedRequestMetadata) {
	if metadata == nil {
		return
	}
	previous := auditdomain.NormalizeReasoningEffort(metadata.ReasoningEffort)
	metadata.ReasoningEffort = ""
	if !requested || !spec.SupportsReasoning {
		return
	}
	if !spec.SupportsReasoningEffort {
		metadata.ReasoningEffort = "fixed"
		return
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	effort, _ := reasoning["effort"].(string)
	if normalized := auditdomain.NormalizeReasoningEffort(effort); normalized != "" {
		metadata.ReasoningEffort = normalized
		return
	}
	metadata.ReasoningEffort = previous
}

func normalizeConsoleResponseFormat(payload map[string]any) {
	raw, exists := payload["response_format"]
	if !exists {
		return
	}
	delete(payload, "response_format")
	format, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if typeName, _ := format["type"].(string); typeName == "json_schema" {
		if nested, ok := format["json_schema"].(map[string]any); ok {
			flattened := map[string]any{"type": "json_schema"}
			for key, value := range nested {
				if key != "type" {
					flattened[key] = value
				}
			}
			format = flattened
		}
	}
	text, _ := payload["text"].(map[string]any)
	if text == nil {
		text = make(map[string]any)
	}
	if _, exists := text["format"]; !exists {
		text["format"] = format
	}
	payload["text"] = text
}

func patchConsoleInput(payload map[string]any) {
	items, ok := payload["input"].([]any)
	if !ok {
		return
	}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if item["type"] == "reasoning" {
			patchConsoleReasoningContent(item)
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			typeName, _ := part["type"].(string)
			switch typeName {
			case "text", "output_text":
				part["type"] = "input_text"
			case "image_url":
				if image, ok := part["image_url"].(map[string]any); ok {
					if url, _ := image["url"].(string); strings.TrimSpace(url) != "" {
						part["type"] = "input_image"
						part["image_url"] = url
					}
				}
			}
		}
	}
}

func patchConsoleReasoningContent(item map[string]any) {
	content, ok := item["content"].([]any)
	if !ok {
		return
	}
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := part["type"]; !exists {
			if _, hasText := part["text"]; hasText {
				part["type"] = "reasoning_text"
			}
		}
	}
}

func normalizeReasoning(payload map[string]any, spec ModelSpec) {
	if !spec.SupportsReasoning {
		delete(payload, "reasoning")
		return
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning == nil {
		if spec.DefaultReasoningEffort == "" {
			delete(payload, "reasoning")
			return
		}
		reasoning = make(map[string]any)
	}
	if !spec.SupportsReasoningEffort {
		delete(reasoning, "effort")
		if len(reasoning) == 0 {
			delete(payload, "reasoning")
		} else {
			payload["reasoning"] = reasoning
		}
		return
	}
	effort, _ := reasoning["effort"].(string)
	effort = normalizeEffort(effort)
	if effort == "" {
		effort = spec.DefaultReasoningEffort
	}
	if effort != "" {
		reasoning["effort"] = effort
	}
	payload["reasoning"] = reasoning
}

func normalizeEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none":
		return "none"
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max":
		return "xhigh"
	default:
		return ""
	}
}

func ensureReasoningInclude(payload map[string]any) {
	value, _ := payload["include"].([]any)
	seen := make(map[string]struct{})
	result := make([]any, 0)
	for _, item := range value {
		name, ok := item.(string)
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	if _, exists := seen["reasoning.encrypted_content"]; !exists {
		result = append(result, "reasoning.encrypted_content")
	}
	payload["include"] = result
}

func normalizeConsoleTools(payload map[string]any) error {
	if err := xaitools.NormalizePayload(payload); err != nil {
		return err
	}
	tools, _ := payload["tools"].([]any)
	hasClientViewImage := hasConsoleFunctionTool(tools, "view_image")
	for index, raw := range tools {
		tool := raw.(map[string]any)
		typeName, _ := tool["type"].(string)
		switch typeName {
		case "web_search":
			// A client view_image definition owns that name. Avoid enabling the
			// hosted tool of the same name; explicit contradictory choices fail.
			if hasClientViewImage && tool["enable_image_understanding"] == true {
				return fmt.Errorf("tools[%d].enable_image_understanding 与客户端 view_image 重名", index)
			}
			if _, explicit := tool["enable_image_understanding"]; !explicit && hasClientViewImage {
				tool["enable_image_understanding"] = false
			}
		case "x_search":
		case "function":
			name, _ := tool["name"].(string)
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("tools[%d].name 不能为空", index)
			}
		case "mcp", "shell", "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter":
		default:
			return fmt.Errorf("Console 不支持 tools[%d].type=%q", index, typeName)
		}
	}
	if len(tools) > 0 && payload["tool_choice"] == nil {
		payload["tool_choice"] = "auto"
	}
	return nil
}

func hasConsoleFunctionTool(tools []any, target string) bool {
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := tool["type"].(string)
		name, _ := tool["name"].(string)
		if strings.EqualFold(strings.TrimSpace(typeName), "function") && strings.EqualFold(strings.TrimSpace(name), target) {
			return true
		}
	}
	return false
}

func toolIdentity(value any) string {
	tool, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	typeName, _ := tool["type"].(string)
	if typeName != "function" {
		return typeName
	}
	name, _ := tool["name"].(string)
	return typeName + ":" + name
}

func parseConsoleRateLimitMetadata(body []byte) *provider.RateLimitMetadata {
	return provider.ParseRateLimitMetadata(body)
}
