package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/xaitools"
)

func (c *responsesToolCompatibility) normalizeToolChoice(payload map[string]json.RawMessage, normalizedTools []any) error {
	raw := payload["tool_choice"]
	if isEmptyJSON(raw) {
		return nil
	}
	var choice any
	if err := json.Unmarshal(raw, &choice); err != nil {
		return &responsesRequestError{Message: "tool_choice 格式无效", Param: "tool_choice", Code: "invalid_parameter"}
	}
	object, ok := choice.(map[string]any)
	if !ok {
		return c.normalizeNativeChoice(payload, choice, normalizedTools)
	}
	kind := stringField(object, "type")
	if kind == "tool_search" {
		if c.clientSearchTool == nil {
			return &responsesRequestError{Message: "tool_choice 引用了未声明的 tool_search", Param: "tool_choice", Code: "invalid_parameter"}
		}
		object = map[string]any{
			"type": "function",
			"name": c.alias(responsesToolIdentity{Kind: responsesToolSearch, Name: "tool_search"}),
		}
		c.changed = true
		payload["tool_choice"] = mustJSON(object)
		return nil
	}
	if kind == "custom" {
		name := strings.TrimSpace(stringField(object, "name"))
		namespace := strings.TrimSpace(stringField(object, "namespace"))
		if name == "" {
			return &responsesRequestError{Message: "tool_choice.name 不能为空", Param: "tool_choice.name", Code: "invalid_parameter"}
		}
		identity := responsesToolIdentity{Kind: responsesCustomTool, Namespace: namespace, Name: name}
		alias, exists := c.identityAliases[identity.key()]
		if !exists {
			return &responsesRequestError{Message: "tool_choice 引用了未声明的 custom 工具", Param: "tool_choice.name", Code: "invalid_parameter"}
		}
		object["type"] = "function"
		object["name"] = alias
		delete(object, "namespace")
		c.changed = true
		payload["tool_choice"] = mustJSON(object)
		return nil
	}
	if kind == "apply_patch" {
		identity := responsesToolIdentity{Kind: responsesApplyPatchTool, Name: "apply_patch"}
		alias, exists := c.identityAliases[identity.key()]
		if !exists {
			return &responsesRequestError{Message: "tool_choice 引用了未声明的 apply_patch 工具", Param: "tool_choice", Code: "invalid_parameter"}
		}
		payload["tool_choice"] = mustJSON(map[string]any{"type": "function", "name": alias})
		c.changed = true
		return nil
	}
	if xaitools.HostedKind(kind) != "" {
		return c.normalizeNativeChoice(payload, object, normalizedTools)
	}
	if kind != "function" {
		return &responsesRequestError{Message: fmt.Sprintf("Grok Build 不支持 tool_choice.type=%q", kind), Param: "tool_choice.type", Code: "unsupported_parameter"}
	}
	name := strings.TrimSpace(stringField(object, "name"))
	namespace := strings.TrimSpace(stringField(object, "namespace"))
	if function, nested := object["function"].(map[string]any); nested {
		name = strings.TrimSpace(stringField(function, "name"))
		namespace = strings.TrimSpace(stringField(function, "namespace"))
		if name != "" && namespace != "" {
			identity := responsesToolIdentity{Kind: responsesFunctionTool, Namespace: namespace, Name: name}
			alias, exists := c.identityAliases[identity.key()]
			if !exists {
				return &responsesRequestError{Message: "tool_choice 引用了未声明的 namespace 函数", Param: "tool_choice.function.name", Code: "invalid_parameter"}
			}
			function["name"] = alias
			delete(function, "namespace")
			c.changed = true
			payload["tool_choice"] = mustJSON(object)
		}
		return c.normalizeNativeChoice(payload, object, normalizedTools)
	}
	if name == "" || namespace == "" {
		return c.normalizeNativeChoice(payload, object, normalizedTools)
	}
	identity := responsesToolIdentity{Kind: responsesFunctionTool, Namespace: namespace, Name: name}
	alias, exists := c.identityAliases[identity.key()]
	if !exists {
		return &responsesRequestError{Message: "tool_choice 引用了未声明的 namespace 函数", Param: "tool_choice.name", Code: "invalid_parameter"}
	}
	object["name"] = alias
	delete(object, "namespace")
	c.changed = true
	payload["tool_choice"] = mustJSON(object)
	return nil
}

func (c *responsesToolCompatibility) normalizeNativeChoice(payload map[string]json.RawMessage, choice any, tools []any) error {
	normalized, selected, err := xaitools.Choice(choice, tools)
	if err != nil {
		return toolConstraintError(err)
	}
	if len(selected) != len(tools) {
		payload["tools"] = mustJSON(selected)
		c.addWarning("hosted_tool_choice_tools_narrowed")
	}
	payload["tool_choice"] = mustJSON(normalized)
	c.changed = true
	return nil
}
