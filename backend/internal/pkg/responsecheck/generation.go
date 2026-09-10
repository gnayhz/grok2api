package responsecheck

import (
	"bytes"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

// JSONGeneration reports only explicit protocol completion. It does not
// decide admission, output validity, history commits or delivery success.
// In particular, usage alone and an arbitrary JSON object prove no completion.
func JSONGeneration(data []byte) string {
	if !jsonpeek.Valid(data) {
		return "unconfirmed"
	}
	if raw := jsonpeek.RootRawValue(data, "error"); len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "failed"
	}
	if output := bytes.TrimSpace(jsonpeek.RootRawValue(data, "output")); len(output) > 0 && output[0] == '[' {
		switch jsonpeek.RootStringFieldScan(data, "status") {
		case "completed":
			return "completed"
		case "failed", "incomplete", "cancelled", "canceled":
			return "failed"
		}
	}
	if jsonpeek.RootStringFieldScan(data, "object") == "chat.completion" {
		count, complete := 0, true
		jsonpeek.ArrayValues(jsonpeek.RootRawValue(data, "choices"), func(choice []byte) bool {
			count++
			complete = complete && jsonpeek.RootStringFieldScan(choice, "finish_reason") != ""
			return true
		})
		if count > 0 && complete {
			return "completed"
		}
	}
	if jsonpeek.RootStringFieldScan(data, "type") == "message" && jsonpeek.RootStringFieldScan(data, "stop_reason") != "" {
		return "completed"
	}
	return "unconfirmed"
}

// EventGeneration accepts the native Responses terminal envelope. The shared
// SSE assembler calls observers even when output validation rejects an event.
func EventGeneration(kind string, data []byte) string {
	if !jsonpeek.Valid(data) {
		return "unconfirmed"
	}
	if kind == "" {
		kind = jsonpeek.RootStringFieldScan(data, "type")
	}
	switch kind {
	case "response.failed", "response.incomplete", "error":
		return "failed"
	case "response.completed":
		response := jsonpeek.RootRawValue(data, "response")
		if len(response) == 0 {
			return "unconfirmed"
		}
		switch jsonpeek.RootStringFieldScan(response, "status") {
		case "", "completed":
			return "completed"
		case "failed", "incomplete", "cancelled", "canceled":
			return "failed"
		}
	}
	return "unconfirmed"
}
