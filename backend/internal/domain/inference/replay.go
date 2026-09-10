package inference

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ReplayPolicy describes when a request may be repeated before downstream output
// is published. An idempotency header alone does not prove that an upstream tool
// execution is safe to repeat across accounts or execution planes.
type ReplayPolicy struct {
	Safe            bool
	Tools           bool
	Reason          string
	ReasoningEffort string
}

// ReplayPolicyFromRequest is also applied to the actual normalized provider payload. Unknown
// server tools are conservative: transport loss can hide a completed side effect.
func ReplayPolicyFromRequest(body []byte) (policy ReplayPolicy) {
	var request struct {
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Tools []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if json.Unmarshal(body, &request) != nil || len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return ReplayPolicy{Reason: "unrecognized_request"}
	}
	defer func() {
		policy.ReasoningEffort = request.ReasoningEffort
		if request.Reasoning != nil && request.Reasoning.Effort != "" {
			policy.ReasoningEffort = request.Reasoning.Effort
		}
		policy.ReasoningEffort = strings.ToLower(strings.TrimSpace(policy.ReasoningEffort))
	}()
	if bytes.Equal(bytes.TrimSpace(request.ToolChoice), []byte(`"none"`)) || len(request.Tools) == 0 {
		return ReplayPolicy{Safe: true, Reason: "no_server_tools"}
	}
	for _, tool := range request.Tools {
		switch strings.TrimSpace(tool.Type) {
		case "function", "custom", "web_search", "web_search_preview", "web_search_20250305", "x_search":
			// Function/custom calls are proposals executed by the client after
			// delivery; the listed built-in searches only retrieve information.
		case "":
			if tool.Name == "" || len(tool.InputSchema) == 0 {
				return ReplayPolicy{Tools: true, Reason: "unknown_tool"}
			}
		default:
			return ReplayPolicy{Tools: true, Reason: "server_tool_effects_unknown"}
		}
	}
	return ReplayPolicy{Safe: true, Tools: true, Reason: "client_tools_or_search"}
}
