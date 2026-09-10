package cli

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/xaitools"
)

// normalizeNativeTool preserves native Build tools and removes Tool Search-only fields.
func (c *responsesToolCompatibility) normalizeNativeTool(tool map[string]any, _ string) ([]any, error) {
	converted := cloneJSONObject(tool)
	if _, exists := converted["defer_loading"]; exists {
		delete(converted, "defer_loading")
		c.changed = true
		c.addWarning("orphan_deferred_tool_loaded")
	}
	return []any{converted}, nil
}

func toolConstraintError(err error) error {
	if typed, ok := err.(*xaitools.Error); ok {
		return &responsesRequestError{Message: typed.Error(), Param: typed.Param, Code: typed.Code}
	}
	return err
}

func (c *responsesToolCompatibility) normalizeXSearchTool(tool map[string]any, param string) ([]any, error) {
	converted, err := xaitools.XSearch(tool, param)
	if err != nil {
		return nil, toolConstraintError(err)
	}
	if !reflect.DeepEqual(tool, converted) {
		c.changed = true
	}
	return []any{converted}, nil
}

func (c *responsesToolCompatibility) normalizeWebSearchTool(tool map[string]any, kind, param string) ([]any, error) {
	converted, err := xaitools.WebSearch(tool, param)
	if err != nil {
		return nil, toolConstraintError(err)
	}
	if !reflect.DeepEqual(tool, converted) {
		c.changed = true
		c.addWarning("web_search_controls_normalized")
		for _, field := range []string{"allowed_domains", "excluded_domains"} {
			if _, exists := tool[field]; exists {
				c.addWarning("web_search_" + field + "_normalized")
			}
		}
	}
	return []any{converted}, nil
}

// normalizeMCPTool 支持客户端 Tool Search 延迟加载整个 MCP server 定义。
func (c *responsesToolCompatibility) normalizeMCPTool(tool map[string]any, clientSearch, force bool, param string) ([]any, error) {
	deferred, _ := tool["defer_loading"].(bool)
	if deferred && !clientSearch && !force {
		c.changed = true
		c.addWarning("orphan_deferred_tool_loaded")
	}
	if deferred && clientSearch && !force {
		label := strings.TrimSpace(stringField(tool, "server_label"))
		if label == "" {
			label = strings.TrimSpace(stringField(tool, "name"))
		}
		if label == "" {
			return nil, &responsesRequestError{Message: "延迟 MCP 工具缺少 server_label", Param: param + ".server_label", Code: "invalid_parameter"}
		}
		c.deferredSurfaces = append(c.deferredSurfaces, describeDeferredTool(label, stringField(tool, "description")))
		c.changed = true
		return nil, nil
	}
	converted := cloneJSONObject(tool)
	if _, exists := converted["defer_loading"]; exists {
		delete(converted, "defer_loading")
		c.changed = true
	}
	normalized, err := xaitools.MCP(converted, param)
	if err != nil {
		return nil, toolConstraintError(err)
	}
	if !reflect.DeepEqual(tool, normalized) {
		c.changed = true
	}
	if normalized == nil {
		return nil, nil
	}
	return []any{normalized}, nil
}

func unsupportedBuildToolError(kind, param string) error {
	return &responsesRequestError{
		Message: fmt.Sprintf("Grok Build 不支持 tools.type=%q", kind),
		Param:   param + ".type", Code: "unsupported_parameter",
	}
}

func hasToolType(tools []any, kind string) bool {
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if ok && stringField(tool, "type") == kind {
			return true
		}
	}
	return false
}
