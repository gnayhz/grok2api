package audit

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// EventIdentity preserves an explicit identity or derives the existing legacy
// identity from the immutable request metadata. Callers persist the result
// before waiting for asynchronous settlement so retries retain the same ID.
func (v Record) EventIdentity() string {
	if id := strings.TrimSpace(v.EventID); id != "" {
		return id
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d", v.RequestID, v.ClientKeyID, v.ModelRouteID, v.CreatedAt.UnixNano())))
	return fmt.Sprintf("evt_%x", digest[:18])
}
