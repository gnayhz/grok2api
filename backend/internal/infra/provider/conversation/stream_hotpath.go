package conversation

import (
	"encoding/json"
	"io"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

// objectField borrows the last immediate field, matching map decoding. Callers
// validate the complete event before using these slices.
func objectField(data []byte, name string) (value []byte) {
	jsonpeek.ObjectFields(data, func(key, raw []byte) bool {
		if string(key) == name {
			value = raw
		}
		return true
	})
	return value
}

func (c *streamConverter) handleHugeOutputItem(data []byte, kind string) error {
	if !json.Valid(data) {
		return io.ErrUnexpectedEOF
	}
	raw := objectField(data, "item")
	if len(raw) == 0 {
		return nil
	}
	itemType := jsonpeek.RootStringFieldScan(raw, "type")
	if itemType == "message" {
		if kind == "response.output_item.done" {
			return c.emitAggregateTextJSON(raw)
		}
		return nil
	}
	if itemType != "reasoning" && itemType != "function_call" &&
		!(itemType == "web_search_call" && c.operation == OperationMessages && c.options.AnthropicWebSearch) {
		return nil
	}
	var item responseItem
	var signature []byte
	if itemType == "reasoning" {
		item.Type = itemType
		item.ID = jsonpeek.RootStringFieldScan(raw, "id")
		signature = objectField(raw, "encrypted_content")
	} else {
		// Tools need their complete arguments/action. Unlike ciphertext these
		// strings may remain in converter state, so reserve beyond the head cap.
		if c.retention != nil {
			if err := c.retention.Grow(4*max(0, len(raw)-maxParsedSSEJSONBytes), 0); err != nil {
				return err
			}
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
	}
	if kind == "response.output_item.added" {
		index, _ := jsonpeek.RootIntFieldScan(data, "output_index")
		return c.handleOutputItemAdded(item, int(index))
	}
	if item.Type != "reasoning" {
		return c.handleOutputItemDone(item)
	}
	if c.reasoningOutputEnabled() {
		if err := c.reasoningDone(item); err != nil {
			return err
		}
	}
	if c.operation == OperationMessages && c.options.AnthropicThinking && len(signature) > 0 {
		return c.thinkingDoneJSONSignature(signature)
	}
	return c.thinkingDone(item)
}

func (c *streamConverter) thinkingDoneJSONSignature(signatureJSON []byte) error {
	if c.operation != OperationMessages || !c.options.AnthropicThinking || !c.thinkingStarted || c.thinkingClosed {
		return nil
	}
	if len(signatureJSON) > 2 && signatureJSON[0] == '"' && signatureJSON[len(signatureJSON)-1] == '"' {
		if err := c.writeSignatureDelta(signatureJSON); err != nil {
			return err
		}
	}
	c.thinkingClosed = true
	return c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.thinkingIndex})
}

func (c *streamConverter) handleHugeCompleted(data []byte, typeName string) error {
	// Decode only envelope metadata. The decoder skips output/ciphertext
	// without retaining it, and field scope/order is identical to small frames.
	var frame struct {
		Response struct {
			ID        string        `json:"id"`
			Model     string        `json:"model"`
			Status    string        `json:"status"`
			CreatedAt int64         `json:"created_at"`
			Usage     responseUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return err
	}
	response := responseEnvelope{
		ID: frame.Response.ID, Model: frame.Response.Model,
		Status: frame.Response.Status, CreatedAt: frame.Response.CreatedAt,
		Usage: frame.Response.Usage,
	}
	c.setResponse(response)
	var aggregateErr error
	jsonpeek.ArrayValues(objectField(objectField(data, "response"), "output"), func(raw []byte) bool {
		if jsonpeek.RootStringFieldScan(raw, "type") != "message" {
			return true
		}
		aggregateErr = c.emitAggregateTextJSON(raw)
		return aggregateErr == nil
	})
	if aggregateErr != nil {
		return aggregateErr
	}
	if c.operation == OperationMessages && c.options.AnthropicWebSearch {
		if err := c.noteHugeCompletedSearch(data); err != nil {
			return err
		}
	}
	status := response.Status
	if status == "" && typeName == "response.incomplete" {
		status = "incomplete"
	}
	return c.done(status)
}

// noteHugeCompletedSearch projects hosted calls and citation annotations. Text
// and ciphertext have already been streamed and are not needed for this merge.
func (c *streamConverter) noteHugeCompletedSearch(data []byte) error {
	output := objectField(objectField(data, "response"), "output")
	var projected responseEnvelope
	var parseErr error
	calls := 0
	reserve := func(size int) bool {
		if c.retention != nil {
			parseErr = c.retention.Grow(256+4*size, 0)
		}
		return parseErr == nil
	}
	jsonpeek.ArrayValues(output, func(raw []byte) bool {
		switch jsonpeek.RootStringFieldScan(raw, "type") {
		case "web_search_call":
			if calls >= maxWebSearchCalls {
				return true
			}
			if !reserve(len(raw)) {
				return false
			}
			var item responseItem
			_ = json.Unmarshal(raw, &item)
			projected.Output = append(projected.Output, item)
			calls++
		case "message":
			item := responseItem{Type: "message"}
			jsonpeek.ArrayValues(objectField(raw, "content"), func(part []byte) bool {
				annotations := objectField(part, "annotations")
				if len(annotations) == 0 {
					return true
				}
				if !reserve(len(annotations)) {
					return false
				}
				var content responseContent
				_ = json.Unmarshal(annotations, &content.Annotations)
				item.Content = append(item.Content, content)
				return true
			})
			if len(item.Content) > 0 {
				projected.Output = append(projected.Output, item)
			}
		}
		return parseErr == nil
	})
	if parseErr != nil {
		return parseErr
	}
	for _, call := range parseResponse(projected).WebSearch {
		if err := c.noteWebSearch(call, true); err != nil {
			return err
		}
	}
	return nil
}
