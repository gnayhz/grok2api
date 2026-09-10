// Package responsecheck validates successful completion independently of
// early reasoning admission. It retains no generated text or tool arguments.
package responsecheck

import (
	"bytes"
	"errors"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

var ErrEmptyOutput = errors.New("upstream completed without answer or tool output")
var ErrToolChoice = errors.New("upstream did not honor the required tool choice")

type Responses struct{ output bool }

func Stream(stream *responseflow.Stream) *responseflow.Stream {
	state := new(Responses)
	stream.Validate(func(event *responseflow.Event) error {
		if !event.HasData {
			return nil
		}
		return state.Observe(string(event.Kind), event.Data)
	})
	return stream
}

// Observe runs before an event is delivered. Reasoning remains immediately
// streamable, but it cannot by itself establish successful task completion.
// Failed and incomplete responses retain their upstream semantics.
func (s *Responses) Observe(kind string, data []byte) error {
	if s.output {
		return nil
	}
	if kind == "" {
		kind = jsonpeek.RootStringFieldScan(data, "type")
	}
	switch kind {
	case "response.output_text.delta", "response.refusal.delta":
		if jsonpeek.Valid(data) {
			s.output = nonemptyString(jsonpeek.RawValue(data, "delta"))
		}
	case "response.output_item.added", "response.output_item.done":
		if jsonpeek.Valid(data) {
			s.output = itemOutput(jsonpeek.RawValue(data, "item"))
		}
	case "response.completed":
		if !jsonpeek.Valid(data) {
			return nil // Parsing errors remain the protocol reader's responsibility.
		}
		response := jsonpeek.RawValue(data, "response")
		status := jsonpeek.RootStringFieldScan(response, "status")
		if status != "" && status != "completed" {
			return nil
		}
		s.output = responseOutput(response)
		if !s.output {
			return ErrEmptyOutput
		}
	}
	return nil
}

// JSON checks an explicitly completed Responses object. Other protocol shapes
// and incomplete/error objects continue through their existing handlers.
func JSON(data []byte) error {
	if !jsonpeek.Valid(data) || jsonpeek.RootStringFieldScan(data, "status") != "completed" {
		return nil
	}
	if output := jsonpeek.RawValue(data, "output"); len(output) > 0 && !responseOutput(data) {
		return ErrEmptyOutput
	}
	return nil
}

func responseOutput(data []byte) bool {
	found := false
	jsonpeek.ArrayValues(jsonpeek.RawValue(data, "output"), func(item []byte) bool {
		found = itemOutput(item)
		return !found
	})
	return found
}

func itemOutput(item []byte) bool {
	switch jsonpeek.RootStringFieldScan(item, "type") {
	case "", "reasoning":
		return false
	case "message":
		found := false
		jsonpeek.ArrayValues(jsonpeek.RawValue(item, "content"), func(content []byte) bool {
			switch jsonpeek.RootStringFieldScan(content, "type") {
			case "output_text", "text":
				found = nonemptyString(jsonpeek.RawValue(content, "text"))
			case "refusal":
				found = nonemptyString(jsonpeek.RawValue(content, "refusal"))
			case "":
			default:
				found = true // Preserve non-text output and future content types.
			}
			return !found
		})
		return found
	default:
		return true // Function calls and hosted tools are meaningful output.
	}
}

func nonemptyString(raw []byte) bool {
	return len(bytes.TrimSpace(jsonpeek.UnquoteBytes(raw))) > 0
}
