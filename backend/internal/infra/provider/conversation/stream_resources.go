package conversation

import (
	"bytes"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

const maxConverterStateBytes = 16 << 20
const maxConverterItems = 4096

// reserveEventState runs before retaining semantic state. Per-event decoder/output
// workspace is owned by responseflow; these reservations cover only state that
// can survive that event. ID count is bounded independently of text length.
func (c *streamConverter) reserveEventState(kind string, data []byte, event *streamEvent) error {
	if c.retention == nil {
		return nil
	}
	retained := 0
	if kind == "response.reasoning_summary_text.delta" && c.reasoningOutputEnabled() || kind == "response.output_text.delta" && c.deferSearchText {
		retained = 4 * len(data)
	}
	if kind == "response.output_item.added" || kind == "response.output_item.done" {
		// Item metadata, tool arguments and deferred search results may all be
		// retained. Large ciphertext is never retained by the client converter.
		retained = 256 + 4*min(len(data), maxParsedSSEJSONBytes)
	}
	if strings.Contains(kind, "reasoning") && event != nil {
		id := event.ItemID
		if id != "" {
			if _, exists := c.retainedIDs[id]; !exists {
				if len(c.retainedIDs) >= maxConverterItems {
					return responsebuffer.ErrLimit
				}
				if err := c.retention.Grow(512+4*len(id), 0); err != nil {
					return err
				}
				c.retainedIDs[id] = struct{}{}
			}
		}
	}
	// Repeat detection, stop filtering and output scratch can retain one
	// event's strings/encoding after its frame has been released.
	return c.retention.Grow(retained, 4096+4*len(data))
}

func (c *streamConverter) releaseResources() {
	if c.retention != nil {
		c.retention.Close()
	}
	c.reasoningItems = nil
	c.reasoningOrder = nil
	c.tools = nil
	c.webSearch = nil
	c.webSearchEmitted = nil
	c.retainedIDs = nil
	c.seenText = nil
	c.pendingSearchText.Reset()
	c.outBuf = bytes.Buffer{}
	c.repeatTracker = streamRepeatTracker{}
}
