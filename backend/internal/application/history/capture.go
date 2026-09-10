package history

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

func (r *ReasoningReplay) CaptureBody(body io.ReadCloser, model, sessionKey string, streaming, compact bool) io.ReadCloser {
	captured, accept := r.CapturePendingBody(body, model, sessionKey, streaming, compact)
	if accept != nil {
		accept()
	}
	return captured
}

// CapturePendingBody requires explicit acceptance before Close may update the
// replay cache. Acceptance may precede or follow Close (adapters can buffer JSON
// before the gateway classifies it). Rejected attempts must call DiscardOutput
// on the returned body to release pending retention without updating the cache.
func (r *ReasoningReplay) CapturePendingBody(body io.ReadCloser, model, sessionKey string, streaming, compact bool) (io.ReadCloser, func()) {
	if !r.Enabled() || body == nil || strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(model) == "" {
		return body, nil
	}
	if stream := responseflow.FromReader(body); stream != nil && streaming {
		return r.captureEvents(body, stream, model, sessionKey, compact)
	}
	if !streaming {
		return r.captureJSON(body, model, sessionKey, compact)
	}
	captured := &replayCaptureBody{inner: body, replay: r, model: model, session: sessionKey, streaming: streaming, compact: compact}
	return captured, captured.accept
}

type replayCaptureBody struct {
	inner     io.ReadCloser
	replay    *ReasoningReplay
	model     string
	session   string
	streaming bool
	compact   bool
	buf       bytes.Buffer
	pending   []byte
	truncated bool
	sawEOF    bool
	readErr   error
	done      bool
	mu        sync.Mutex // protects capture state against concurrent Read/Close/accept
	closeOnce sync.Once
	closeErr  error
	accepted  bool
	committed bool
}

func (b *replayCaptureBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return n, err
	}
	if n > 0 && !b.truncated && !b.compact {
		if b.streaming {
			b.observeSSE(p[:n])
		} else if b.buf.Len()+n > maxReplayCaptureBytes {
			b.discardCapture()
		} else {
			_, _ = b.buf.Write(p[:n])
		}
	}
	if err == io.EOF {
		b.sawEOF = true
		if b.streaming && !b.truncated && !b.compact && len(b.pending) > 0 {
			b.observeSSE([]byte{10})
		}
	} else if err != nil {
		b.readErr = err
	}
	return n, err
}

func (b *replayCaptureBody) Close() error {
	b.closeOnce.Do(func() {
		// Close the transport before acquiring the capture lock: a pending Read
		// must be interrupted, and may finish its observation concurrently.
		b.closeErr = b.inner.Close()
		b.mu.Lock()
		b.done = true
		b.pending = nil // An incomplete final SSE line can never be committed.
		b.mu.Unlock()
	})
	b.commit()
	return b.closeErr
}

func (b *replayCaptureBody) accept() {
	b.mu.Lock()
	b.accepted = true
	b.mu.Unlock()
	b.commit()
}

func (b *replayCaptureBody) DiscardOutput() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.committed = true
	b.discardCapture()
}

func (b *replayCaptureBody) commit() {
	b.mu.Lock()
	if !b.done || !b.accepted || b.committed {
		b.mu.Unlock()
		return
	}
	b.committed = true
	payload := b.buf.Bytes()
	valid := !b.truncated && len(payload) > 0
	complete := b.sawEOF && b.readErr == nil
	b.buf = bytes.Buffer{}
	b.pending = nil
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if b.compact {
		if complete {
			b.replay.Clear(ctx, b.model, b.session)
		}
		return
	}
	if !valid {
		return
	}
	if b.streaming {
		if completed, ok := extractCompletedPayloadFromSSE(payload); ok {
			b.replay.StoreFromCompleted(ctx, b.model, b.session, completed)
		}
	} else if complete {
		b.replay.StoreFromCompleted(ctx, b.model, b.session, payload)
	}
}

func (b *replayCaptureBody) discardCapture() {
	b.truncated = true
	b.buf = bytes.Buffer{}
	b.pending = nil
}

func (b *replayCaptureBody) observeSSE(chunk []byte) {
	for len(chunk) > 0 && !b.truncated {
		// Search new bytes only; a long fragmented ciphertext line must not
		// repeatedly scan the prefix accumulated by earlier reads.
		index := bytes.IndexByte(chunk, 10)
		if index < 0 {
			if len(b.pending)+len(chunk) > maxReplayCaptureBytes {
				b.discardCapture()
				return
			}
			b.pending = append(b.pending, chunk...)
			return
		}
		line := chunk[:index+1]
		chunk = chunk[index+1:]
		if len(b.pending) > 0 {
			if len(b.pending)+len(line) > maxReplayCaptureBytes {
				b.discardCapture()
				return
			}
			b.pending = append(b.pending, line...)
			line = b.pending
		}
		if keepReplaySSELine(line) {
			if b.buf.Len()+len(line) > maxReplayCaptureBytes {
				b.discardCapture()
				return
			}
			_, _ = b.buf.Write(line)
		}
		if cap(b.pending) > 32<<10 {
			b.pending = nil // Do not retain a large completed frame as scratch.
		} else {
			b.pending = b.pending[:0]
		}
	}
}

func keepReplaySSELine(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return false
	}
	payload := bytes.TrimSpace(trimmed[5:])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	head := payload
	if len(head) > 4096 {
		head = payload[:4096]
	}
	switch jsonpeek.StringField(head, "type") {
	case "response.output_item.done", "response.completed", "response.done":
		return true
	case "":
		return bytes.Contains(head, []byte(`"output"`))
	default:
		return false
	}
}

func extractCompletedPayloadFromSSE(data []byte) ([]byte, bool) {
	lines := bytes.Split(data, []byte("\n"))
	var last []byte
	itemsByIndex := map[int][]byte{}
	var fallbackItems [][]byte
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(value, []byte("[DONE]")) || len(value) == 0 {
			continue
		}
		var typed struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Output   json.RawMessage `json:"output"`
			Item     json.RawMessage `json:"item"`
			Index    *int            `json:"output_index"`
		}
		if json.Unmarshal(value, &typed) != nil {
			continue
		}
		switch strings.TrimSpace(typed.Type) {
		case "response.output_item.done":
			if len(typed.Item) == 0 || !json.Valid(typed.Item) {
				continue
			}
			item := append([]byte(nil), typed.Item...)
			if typed.Index != nil {
				itemsByIndex[*typed.Index] = item
			} else {
				fallbackItems = append(fallbackItems, item)
			}
		case "response.completed", "response.done":
			if len(typed.Response) > 0 {
				last = patchCompletedOutput(value, itemsByIndex, fallbackItems)
			} else if len(typed.Output) > 0 {
				last = append([]byte(nil), value...)
			}
		default:
			if len(typed.Output) > 0 && typed.Type == "" {
				last = append([]byte(nil), value...)
			}
		}
	}
	return last, len(last) > 0
}

func patchCompletedOutput(event []byte, itemsByIndex map[int][]byte, fallbackItems [][]byte) []byte {
	if len(itemsByIndex) == 0 && len(fallbackItems) == 0 {
		return append([]byte(nil), event...)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(event, &root) != nil {
		return append([]byte(nil), event...)
	}
	responseRaw, ok := root["response"]
	if !ok {
		return append([]byte(nil), event...)
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(responseRaw, &response) != nil {
		return append([]byte(nil), event...)
	}
	var existing []json.RawMessage
	if raw, exists := response["output"]; exists && json.Unmarshal(raw, &existing) == nil && len(existing) > 0 {
		return append([]byte(nil), event...)
	}
	indexes := make([]int, 0, len(itemsByIndex))
	for index := range itemsByIndex {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	output := make([]json.RawMessage, 0, len(indexes)+len(fallbackItems))
	for _, index := range indexes {
		output = append(output, json.RawMessage(itemsByIndex[index]))
	}
	for _, item := range fallbackItems {
		output = append(output, json.RawMessage(item))
	}
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		return append([]byte(nil), event...)
	}
	response["output"] = encodedOutput
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		return append([]byte(nil), event...)
	}
	root["response"] = encodedResponse
	patched, err := json.Marshal(root)
	if err != nil {
		return append([]byte(nil), event...)
	}
	return patched
}

// StoreFromCompletedPayload 供已缓冲完整 JSON 的路径直接调用。
func (r *ReasoningReplay) StoreFromCompletedPayload(ctx context.Context, model, sessionKey string, payload []byte, compact bool) {
	if compact {
		r.Clear(ctx, model, sessionKey)
		return
	}
	r.StoreFromCompleted(ctx, model, sessionKey, payload)
}
