package inference

import "errors"

// ToolCompatibilityPolicy is chosen by the request orchestrator. Neither account
// tier nor a cache/session identity authorizes an additional callable tool.
type ToolCompatibilityPolicy uint8

const (
	PreserveToolDeclarations ToolCompatibilityPolicy = iota
	AllowDisabledCacheTools
)

// ToolCompatibilityPlan describes cache declarations added after protocol
// normalization. It contains no client tool names, schema, or request content.
type ToolCompatibilityPlan struct {
	AddedCacheTools   []string
	ExecutionDisabled bool
}

func (p ToolCompatibilityPlan) Validate(policy ToolCompatibilityPolicy) error {
	if len(p.AddedCacheTools) == 0 {
		return nil
	}
	if policy != AllowDisabledCacheTools || !p.ExecutionDisabled {
		return errors.New("cache compatibility cannot add executable tools")
	}
	return nil
}
