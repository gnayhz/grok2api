package conversation

import (
	"encoding/json"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

type streamTextKey struct {
	item    string
	index   int
	refusal bool
}

func (c *streamConverter) noteText(item string, index int, refusal bool) error {
	key := streamTextKey{item, index, refusal}
	if c.seenText[key] {
		return nil
	}
	if len(c.seenText) >= maxConverterItems {
		return responsebuffer.ErrLimit
	}
	if err := c.retention.Grow(128+len(item), 0); err != nil {
		return err
	}
	if c.seenText == nil {
		c.seenText = make(map[streamTextKey]bool)
	}
	c.seenText[key] = true
	return nil
}

// Some upstreams provide text only in item.done or the final output array.
// Recover content blocks that had no deltas, once. Anonymous deltas retain the
// existing conservative behavior because their item identity is unavailable.
func (c *streamConverter) emitAggregateText(item responseItem) error {
	if item.Type != "message" || c.stopSequence != "" {
		return nil
	}
	for index, part := range item.Content {
		if err := c.emitAggregatePart(item.ID, index, part); err != nil {
			return err
		}
	}
	return nil
}

// Borrow large message content and decode only blocks that need recovery.
// Already streamed text and adjacent reasoning ciphertext are never copied.
func (c *streamConverter) emitAggregateTextJSON(raw []byte) error {
	if c.stopSequence != "" {
		return nil
	}
	id := jsonpeek.RootStringFieldScan(raw, "id")
	index := 0
	var err error
	jsonpeek.ArrayValues(objectField(raw, "content"), func(content []byte) bool {
		current := index
		index++
		typ := jsonpeek.RootStringFieldScan(content, "type")
		refusal := typ == "refusal"
		if (!refusal && typ != "text" && typ != "output_text") || c.seenText[streamTextKey{id, current, refusal}] || c.seenText[streamTextKey{"", current, refusal}] {
			return true
		}
		var part responseContent
		if err = json.Unmarshal(content, &part); err != nil {
			return false
		}
		err = c.emitAggregatePart(id, current, part)
		return err == nil
	})
	return err
}

func (c *streamConverter) emitAggregatePart(id string, index int, part responseContent) error {
	if c.stopSequence != "" {
		return nil
	}
	refusal := part.Type == "refusal"
	if !refusal && part.Type != "output_text" && part.Type != "text" {
		return nil
	}
	if c.seenText[streamTextKey{id, index, refusal}] || c.seenText[streamTextKey{"", index, refusal}] {
		return nil
	}
	text := part.Text
	if refusal {
		text = part.Refusal
	}
	if text == "" {
		return nil
	}
	if err := c.noteText(id, index, refusal); err != nil {
		return err
	}
	if err := c.start(); err != nil {
		return err
	}
	if refusal {
		c.refused = true
		if c.operation == OperationChat {
			if err := c.chatDelta(map[string]any{"refusal": text}); err != nil {
				return err
			}
			return nil
		}
	}
	if c.operation == OperationMessages && c.deferSearchText {
		if err := c.bufferSearchText(text); err != nil {
			return err
		}
	} else if err := c.textDelta(text); err != nil {
		return err
	}
	return nil
}
