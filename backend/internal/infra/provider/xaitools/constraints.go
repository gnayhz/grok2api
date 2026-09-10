// Package xaitools owns the xAI hosted-tool wire constraints shared by Build and
// Console. It performs no I/O and never substitutes a weaker execution constraint.
package xaitools

import (
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
	"time"
)

const MaxWebSearchDomains = 5

type Error struct{ Param, Code, Message string }

func (e *Error) Error() string            { return e.Param + ": " + e.Message }
func invalid(param, message string) error { return &Error{param, "invalid_parameter", message} }
func unsupported(param string) error {
	return &Error{param, "unsupported_parameter", "上游无法等价执行此工具约束"}
}

func keys(object map[string]any) []string {
	result := make([]string, 0, len(object))
	for key := range object {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func HostedKind(kind string) string {
	switch kind {
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
		return "web_search"
	case "x_search", "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter", "mcp", "shell":
		return kind
	case "local_shell":
		return "shell"
	default:
		return ""
	}
}

// ValidateDeclarations prevents duplicate hosted definitions from silently
// replacing a restriction. Client function reloads are handled by their adapter.
func ValidateDeclarations(tools []any) error {
	seen := map[string]map[string]any{}
	var webImagesEnabled, xImagesDisabled bool
	for index, raw := range tools {
		tool, _ := raw.(map[string]any)
		kind, _ := tool["type"].(string)
		if HostedKind(kind) == "" {
			continue
		}
		if kind == "web_search" && tool["enable_image_understanding"] == true {
			webImagesEnabled = true
		}
		if kind == "x_search" && tool["enable_image_understanding"] == false {
			xImagesDisabled = true
		}
		label, _ := tool["server_label"].(string)
		key := kind + "\x00" + label
		if previous, exists := seen[key]; exists && !reflect.DeepEqual(previous, tool) {
			return invalid(fmt.Sprintf("tools[%d]", index), "同一 hosted 工具的约束声明冲突")
		}
		seen[key] = tool
	}
	// xAI's web image-understanding switch also enables the same X tool.
	if webImagesEnabled && xImagesDisabled {
		return invalid("tools", "web_search 图像理解会启用 X 图像理解，与 x_search 的显式禁用冲突")
	}
	return nil
}

// WebSearch keeps equivalent native controls. Search context size and approximate
// location are retrieval hints; their omission does not grant additional tools or
// remove a domain/access restriction. Other unrepresentable fields are rejected.
func WebSearch(tool map[string]any, param string) (map[string]any, error) {
	result := map[string]any{"type": "web_search"}
	filters := map[string]any{}
	if raw := tool["filters"]; raw != nil {
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, invalid(param+".filters", "必须是对象")
		}
		for _, key := range keys(object) {
			if key != "allowed_domains" && key != "excluded_domains" {
				return nil, unsupported(param + ".filters." + key)
			}
			values, err := stringList(object[key], MaxWebSearchDomains, param+".filters."+key)
			if err != nil {
				return nil, err
			}
			if len(values) > 0 {
				filters[key] = values
			}
		}
	}
	for _, key := range keys(tool) {
		value := tool[key]
		switch key {
		case "type", "filters":
		case "allowed_domains", "excluded_domains":
			values, err := stringList(value, MaxWebSearchDomains, param+"."+key)
			if err != nil {
				return nil, err
			}
			if len(values) > 0 {
				if previous, exists := filters[key]; exists && !reflect.DeepEqual(previous, values) {
					return nil, invalid(param+"."+key, "重复的域名限制声明冲突")
				}
				filters[key] = values
			}
		case "enable_image_understanding", "enable_image_search":
			if _, ok := value.(bool); !ok {
				return nil, invalid(param+"."+key, "必须是布尔值")
			}
			result[key] = value
		case "external_web_access", "indexed_web_access":
			enabled, ok := value.(bool)
			if !ok {
				return nil, invalid(param+"."+key, "必须是布尔值")
			}
			if !enabled {
				return nil, unsupported(param + "." + key)
			}
		case "search_content_types":
			values, ok := value.([]any)
			if !ok || len(values) == 0 {
				return nil, invalid(param+"."+key, "必须是非空数组")
			}
			for _, kind := range values {
				if kind != "text" {
					return nil, unsupported(param + "." + key)
				}
			}
		case "search_context_size", "user_location":
		default:
			return nil, unsupported(param + "." + key)
		}
	}
	if _, allowed := filters["allowed_domains"]; allowed && filters["excluded_domains"] != nil {
		return nil, invalid(param+".filters", "不能同时指定允许和排除域名")
	}
	if len(filters) > 0 {
		result["filters"] = filters
	}
	if _, textOnly := tool["search_content_types"]; textOnly {
		for _, key := range []string{"enable_image_search", "enable_image_understanding"} {
			if result[key] == true {
				return nil, invalid(param+"."+key, "与仅文本搜索冲突")
			}
			result[key] = false
		}
	}
	return result, nil
}

func XSearch(tool map[string]any, param string) (map[string]any, error) {
	result := map[string]any{"type": "x_search"}
	for _, key := range keys(tool) {
		value := tool[key]
		switch key {
		case "type":
		case "from_date", "to_date":
			if value == nil {
				continue
			}
			text, ok := value.(string)
			date, err := time.Parse("2006-01-02", text)
			if !ok || err != nil || date.Year() < 1 || date.Format("2006-01-02") != text {
				return nil, invalid(param+"."+key, "必须使用 YYYY-MM-DD 格式")
			}
			result[key] = text
		case "allowed_x_handles", "excluded_x_handles":
			values, err := stringList(value, 20, param+"."+key)
			if err != nil {
				return nil, err
			}
			if len(values) > 0 {
				result[key] = values
			}
		case "enable_image_understanding", "enable_video_understanding":
			if _, ok := value.(bool); !ok {
				return nil, invalid(param+"."+key, "必须是布尔值")
			}
			result[key] = value
		default:
			return nil, unsupported(param + "." + key)
		}
	}
	from, _ := result["from_date"].(string)
	to, _ := result["to_date"].(string)
	if from != "" && to != "" && from > to {
		return nil, invalid(param+".from_date", "不得晚于 to_date")
	}
	if result["allowed_x_handles"] != nil && result["excluded_x_handles"] != nil {
		return nil, invalid(param, "不能同时指定允许和排除 X 账号")
	}
	return result, nil
}

func stringList(value any, limit int, param string) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, invalid(param, "必须是字符串数组")
	}
	if limit > 0 && len(values) > limit {
		return nil, invalid(param, fmt.Sprintf("不能超过 %d 项", limit))
	}
	for index, raw := range values {
		text, ok := raw.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, invalid(fmt.Sprintf("%s[%d]", param, index), "必须是非空字符串")
		}
	}
	return values, nil
}

// MCP returns nil for an explicitly empty allowlist: xAI interprets [] as all
// tools, so sending that array would invert the client's restriction.
func MCP(tool map[string]any, param string) (map[string]any, error) {
	result := maps.Clone(tool)
	for _, key := range keys(tool) {
		switch key {
		case "type":
		case "server_url", "server_label", "server_description", "authorization":
			if _, ok := tool[key].(string); !ok {
				return nil, invalid(param+"."+key, "必须是字符串")
			}
		case "headers":
			headers, ok := tool[key].(map[string]any)
			if !ok {
				return nil, invalid(param+".headers", "必须是对象")
			}
			for _, value := range headers {
				if _, ok := value.(string); !ok {
					return nil, invalid(param+".headers", "值必须是字符串")
				}
			}
		case "description":
			if _, ok := tool[key].(string); !ok {
				return nil, invalid(param+".description", "必须是字符串")
			}
			if existing, ok := tool["server_description"]; ok && existing != tool[key] {
				return nil, invalid(param+".description", "与 server_description 冲突")
			}
			result["server_description"] = tool[key]
			delete(result, key)
		case "require_approval":
			if tool[key] != "never" {
				return nil, unsupported(param + "." + key)
			}
			delete(result, key)
		case "allowed_tools":
			values, err := stringList(tool[key], 0, param+"."+key)
			if err != nil {
				return nil, err
			}
			if tool[key] != nil && len(values) == 0 {
				return nil, nil
			}
		default:
			return nil, unsupported(param + "." + key)
		}
	}
	return result, nil
}

// Choice resolves a native choice against the surviving declarations. Hosted
// choices become required only after narrowing tools to the requested kind/server.
func Choice(choice any, tools []any) (any, []any, error) {
	if choice == nil {
		return nil, tools, nil
	}
	if mode, ok := choice.(string); ok {
		switch mode {
		case "auto", "none":
			return mode, tools, nil
		case "required":
			if len(tools) > 0 {
				return mode, tools, nil
			}
			return nil, nil, invalid("tool_choice", "没有可用工具满足 required")
		default:
			return nil, nil, invalid("tool_choice", "必须是 auto、none、required 或指定工具")
		}
	}
	object, ok := choice.(map[string]any)
	if !ok {
		return nil, nil, invalid("tool_choice", "格式无效")
	}
	kind, _ := object["type"].(string)
	if kind == "function" {
		name, _ := object["name"].(string)
		if nested, ok := object["function"].(map[string]any); ok {
			if len(nested) != 1 {
				return nil, nil, unsupported("tool_choice.function")
			}
			name, _ = nested["name"].(string)
		}
		for key := range object {
			if key != "type" && key != "name" && key != "function" {
				return nil, nil, unsupported("tool_choice." + key)
			}
		}
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			if tool["type"] == "function" && tool["name"] == name && name != "" {
				return map[string]any{"type": "function", "name": name}, tools, nil
			}
		}
		return nil, nil, invalid("tool_choice.name", "引用了未声明的函数")
	}
	native := HostedKind(kind)
	if native == "" {
		return nil, nil, unsupported("tool_choice.type")
	}
	if raw, exists := object["server_label"]; exists {
		label, ok := raw.(string)
		if !ok || strings.TrimSpace(label) == "" {
			return nil, nil, invalid("tool_choice.server_label", "必须是非空字符串")
		}
	}
	for key := range object {
		if key != "type" && !(native == "mcp" && key == "server_label") {
			return nil, nil, unsupported("tool_choice." + key)
		}
	}
	matching := []any{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["type"] != native {
			continue
		}
		if label, exists := object["server_label"]; exists && label != tool["server_label"] {
			continue
		}
		matching = append(matching, raw)
	}
	if len(matching) == 0 {
		return nil, nil, invalid("tool_choice", "引用了未声明或禁用的工具")
	}
	return "required", matching, nil
}
