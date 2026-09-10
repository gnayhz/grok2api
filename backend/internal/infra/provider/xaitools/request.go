package xaitools

import "fmt"

// NormalizePayload handles converted native Responses tool declarations and
// choice. It does not inspect or rewrite conversation content.
func NormalizePayload(payload map[string]any) error {
	var tools []any
	if raw := payload["tools"]; raw != nil {
		var ok bool
		tools, ok = raw.([]any)
		if !ok {
			return invalid("tools", "必须是数组")
		}
	}
	normalized := make([]any, 0, len(tools))
	for index, raw := range tools {
		tool, ok := raw.(map[string]any)
		param := fmt.Sprintf("tools[%d]", index)
		if !ok {
			return invalid(param, "必须是对象")
		}
		kind, _ := tool["type"].(string)
		var err error
		switch HostedKind(kind) {
		case "web_search":
			tool, err = WebSearch(tool, param)
		case "x_search":
			tool, err = XSearch(tool, param)
		case "mcp":
			tool, err = MCP(tool, param)
		}
		if err != nil {
			return err
		}
		if tool != nil {
			normalized = append(normalized, tool)
		}
	}
	if err := ValidateDeclarations(normalized); err != nil {
		return err
	}
	choice, selected, err := Choice(payload["tool_choice"], normalized)
	if err != nil {
		return err
	}
	if len(selected) > 0 {
		payload["tools"] = selected
	} else {
		delete(payload, "tools")
	}
	if choice != nil {
		payload["tool_choice"] = choice
	}
	return nil
}
