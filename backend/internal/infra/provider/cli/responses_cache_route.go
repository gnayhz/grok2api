package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
)

// buildPromptCacheRoute records internal tools added to route this request through the cache-capable path.
// injectedToolTypes restores the client's original visible tool list during response processing.
type buildPromptCacheRoute struct {
	plan                inferencedomain.ToolCompatibilityPlan
	filterXSearch       bool
	injectedToolTypes   map[string]struct{}
	clientDeclaredTools map[string]struct{}
}

func prepareBuildPromptCacheRoute(body []byte, operation, model, promptCacheKey string, policy inferencedomain.ToolCompatibilityPolicy) ([]byte, buildPromptCacheRoute, error) {
	route := buildPromptCacheRoute{
		injectedToolTypes:   make(map[string]struct{}),
		clientDeclaredTools: make(map[string]struct{}),
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, route, fmt.Errorf("解析 Build prompt cache 请求: %w", err)
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}
	key := strings.TrimSpace(promptCacheKey)
	if key != "" {
		payload["prompt_cache_key"] = mustJSON(key)
	}
	tools, err := buildCacheRouteTools(payload)
	if err != nil {
		return nil, route, err
	}
	for _, rawTool := range tools {
		kind, name := buildCacheToolIdentity(rawTool)
		if kind == "function" || kind == "custom" {
			if name != "" {
				route.clientDeclaredTools[name] = struct{}{}
			}
		}
		if kind == "x_search" {
			route.filterXSearch = true
		}
	}

	// Hide upstream internal subcalls even when the client explicitly declares x_search.
	// Cache routing itself applies only to plain-text conversations with a stable cache session identity.
	if key == "" || !isBuildCacheConversationOperation(operation) || isBuildCacheMediaModel(model) || hasBuildCacheToolType(tools, "image_generation") {
		if key == "" {
			return body, route, nil
		}
		encoded, err := json.Marshal(payload)
		return encoded, route, err
	}

	var choice string
	_ = json.Unmarshal(payload["tool_choice"], &choice)
	route.plan.ExecutionDisabled = choice == "none"
	if policy == inferencedomain.AllowDisabledCacheTools && len(tools) == 0 && (isEmptyJSON(payload["tool_choice"]) || choice == "auto" || choice == "none") {
		// A tool-free request uses none to select the cache-capable route without granting search capability.
		tools = append(tools, json.RawMessage(`{"type":"web_search"}`), json.RawMessage(`{"type":"x_search"}`))
		payload["tool_choice"] = mustJSON("none")
		route.injectedToolTypes["web_search"] = struct{}{}
		route.injectedToolTypes["x_search"] = struct{}{}
		route.filterXSearch = true
		route.plan.AddedCacheTools = []string{"web_search", "x_search"}
		route.plan.ExecutionDisabled = true
		payload["tools"] = mustJSON(tools)
	}
	if err := route.plan.Validate(policy); err != nil {
		return nil, route, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, route, fmt.Errorf("编码 Build prompt cache 请求: %w", err)
	}
	return encoded, route, nil
}

func buildCacheRouteTools(payload map[string]json.RawMessage) ([]json.RawMessage, error) {
	raw, exists := payload["tools"]
	if !exists || isEmptyJSON(raw) {
		return nil, nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, &responsesRequestError{Message: "tools 必须是数组", Param: "tools", Code: "invalid_parameter"}
	}
	return tools, nil
}

func buildCacheToolIdentity(raw json.RawMessage) (kind, name string) {
	var tool struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tool) != nil {
		return "", ""
	}
	return strings.TrimSpace(tool.Type), strings.TrimSpace(tool.Name)
}

func hasBuildCacheToolType(tools []json.RawMessage, kind string) bool {
	for _, rawTool := range tools {
		toolType, _ := buildCacheToolIdentity(rawTool)
		if toolType == kind {
			return true
		}
	}
	return false
}

func isBuildCacheConversationOperation(operation string) bool {
	switch strings.TrimSpace(operation) {
	case "", "responses", "chat", "messages":
		return true
	default:
		return false
	}
}

func isBuildCacheMediaModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "image") || strings.Contains(model, "imagine") || strings.Contains(model, "video")
}
