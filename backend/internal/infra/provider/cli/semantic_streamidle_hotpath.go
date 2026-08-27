package cli

import (
	"bytes"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

func (d *buildSSEActivityDetector) classifyActivity() bool {
	kind := strings.TrimSpace(jsonpeek.StringField(d.data, "type"))
	if kind == "" {
		kind = strings.TrimSpace(d.eventName)
	}
	if _, generatedDelta := buildGeneratedDeltaEvents[kind]; generatedDelta {
		if d.eventBytes > maxIdleInspectBytes {
			return true
		}
		return jsonpeek.StringField(d.data, "delta") != ""
	}
	if kind != "response.output_item.added" && kind != "response.output_item.done" {
		return false
	}
	if d.eventBytes > maxIdleInspectBytes {
		return true
	}
	itemType := nestedItemType(d.data)
	_, generatedItem := buildGeneratedOutputItemTypes[itemType]
	if !generatedItem {
		return false
	}
	return jsonpeek.StringField(d.data, "id") != "" ||
		jsonpeek.StringField(d.data, "call_id") != "" ||
		jsonpeek.StringField(d.data, "name") != ""
}

func nestedItemType(data []byte) string {
	index := bytes.Index(data, []byte(`"item"`))
	if index < 0 {
		return ""
	}
	return jsonpeek.StringField(data[index:], "type")
}
