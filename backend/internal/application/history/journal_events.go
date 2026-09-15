package history

import (
	"bytes"
	"encoding/json"
)

// Indexed output_item.done is the authoritative ordering when a terminal
// response omits items. Never silently discard an opaque item observed there.
// A partial indexed set may only validate an already complete terminal list.
func extractJournalPayloadFromSSE(data []byte) ([]byte, error) {
	indexed := map[int]json.RawMessage{}
	var unindexed []json.RawMessage
	var response json.RawMessage
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Output   json.RawMessage `json:"output"`
			Item     json.RawMessage `json:"item"`
			Index    *int            `json:"output_index"`
		}
		if json.Unmarshal(payload, &event) != nil {
			return nil, journalCommitFailure("extract", "invalid_event", nil)
		}
		switch event.Type {
		case "response.output_item.done":
			if event.Index == nil {
				unindexed = append(unindexed, event.Item)
				continue
			}
			index := *event.Index
			if index < 0 || index >= 4096 {
				return nil, journalCommitFailure("extract", "invalid_output_index", nil)
			}
			if prior, ok := indexed[index]; ok && journalOutputSignature(prior) != journalOutputSignature(event.Item) {
				return nil, journalCommitFailure("extract", "conflicting_output_index", nil)
			}
			indexed[index] = event.Item
		case "response.completed", "response.done":
			next := event.Response
			if len(next) == 0 && len(event.Output) > 0 {
				next = payload
			}
			if len(response) > 0 && !bytes.Equal(response, next) {
				return nil, journalCommitFailure("extract", "conflicting_terminal_responses", nil)
			}
			response = next
		}
	}
	if len(response) == 0 {
		return nil, journalCommitFailure("extract", "missing_terminal_response", nil)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(response, &root) != nil {
		return nil, journalCommitFailure("extract", "invalid_terminal_response", nil)
	}
	var complete []json.RawMessage
	if raw := root["output"]; len(raw) > 0 && json.Unmarshal(raw, &complete) != nil {
		return nil, journalCommitFailure("extract", "invalid_terminal_output", nil)
	}
	// Without indices, only an already complete terminal list can establish order.
	for _, item := range unindexed {
		found := false
		for _, candidate := range complete {
			if journalOutputSignature(item) == journalOutputSignature(candidate) {
				found = true
				break
			}
		}
		if !found {
			return nil, journalCommitFailure("extract", "unindexed_output_missing", nil)
		}
	}
	if len(indexed) > 0 {
		ordered := make([]json.RawMessage, 0, len(indexed))
		contiguous := true
		for i := 0; i < len(indexed); i++ {
			item, ok := indexed[i]
			if !ok {
				contiguous = false
				break
			}
			ordered = append(ordered, item)
		}
		if contiguous {
			// The completed list must be an ordered subset of observed native items.
			position := 0
			for _, candidate := range ordered {
				if position < len(complete) && journalOutputSignature(candidate) == journalOutputSignature(complete[position]) {
					position++
				}
			}
			if position == len(complete) {
				complete = ordered
			} else {
				for i, item := range indexed {
					if i >= len(complete) || journalOutputSignature(item) != journalOutputSignature(complete[i]) {
						return nil, journalCommitFailure("extract", "conflicting_terminal_output", nil)
					}
				}
			}
		} else {
			for i, item := range indexed {
				if i >= len(complete) || journalOutputSignature(item) != journalOutputSignature(complete[i]) {
					return nil, journalCommitFailure("extract", "incomplete_indexed_output", nil)
				}
			}
		}
	}
	if len(complete) == 0 {
		return nil, journalCommitFailure("extract", "missing_terminal_output", nil)
	}
	root["output"], _ = json.Marshal(complete)
	return json.Marshal(root)
}
func journalOutputSignature(raw []byte) string {
	canonical, reasoning, err := canonicalJournalItem(raw)
	if err != nil {
		return "invalid:" + string(raw)
	}
	if reasoning {
		var ok bool
		canonical, ok = normalizeJournalReasoning(raw)
		if !ok {
			return "invalid:" + string(raw)
		}
	}
	return string(canonical)
}
