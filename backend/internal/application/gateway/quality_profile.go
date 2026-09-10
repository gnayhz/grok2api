package gateway

import (
	"bytes"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

// Chat choices are independent generations. A single shared evidence state
// cannot authorize a choice from another choice's thinking. Build/Console emit
// one generation, so the supported profile is exactly index zero (or omitted).
func singleQualityChoice(choices []byte) bool {
	count := 0
	valid := true
	jsonpeek.ArrayValues(choices, func(choice []byte) bool {
		count++
		if count > 1 {
			valid = false
			return false
		}
		var index []byte
		jsonpeek.ObjectFields(choice, func(key, value []byte) bool {
			if string(key) == "index" {
				index = value
			}
			return true
		})
		if index != nil && !bytes.Equal(bytes.TrimSpace(index), []byte("0")) {
			valid = false
			return false
		}
		return true
	})
	return valid
}
