package clientkey

import (
	"fmt"
	"slices"
)

// ModelScope is an administrator's authorization decision, independent of the
// number of surviving model relations. Restricted with no members denies all.
type ModelScope string

const (
	ModelScopeAll        ModelScope = "all"
	ModelScopeRestricted ModelScope = "restricted"
)

// NormalizeModelAccess accepts the legacy creation/explicit-list command only
// at the write boundary. Runtime Key authorization never infers from emptiness.
func NormalizeModelAccess(scope ModelScope, ids []uint64) (ModelScope, []uint64, error) {
	if scope == "" {
		scope = ModelScopeAll
		if len(ids) != 0 {
			scope = ModelScopeRestricted
		}
	}
	if scope != ModelScopeAll && scope != ModelScopeRestricted {
		return scope, nil, fmt.Errorf("modelScope 无效")
	}
	if scope == ModelScopeAll && len(ids) != 0 {
		return scope, nil, fmt.Errorf("全部模型范围不能同时指定 allowedModelIds")
	}
	result := slices.Clone(ids)
	slices.Sort(result)
	result = slices.Compact(result)
	if len(result) > 0 && result[0] == 0 {
		return scope, nil, fmt.Errorf("allowedModelIds 包含无效 ID")
	}
	return scope, result, nil
}
