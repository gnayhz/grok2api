package history

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

// Capture keeps native output through protocol conversion. Commit is invoked
// only by the delivery success barrier after quality validation and clean EOF.
// Closing a body never authorizes a durable write.
func (p *PreparedHistory) Capture(body io.ReadCloser, streaming bool) (io.ReadCloser, func() error, func()) {
	if p == nil {
		return body, nil, nil
	}
	stream := responseflow.FromReader(body)
	if streaming && stream == nil {
		stream = responseflow.New(body, responsebuffer.BudgetOf(body))
		body = stream
	}
	c := &journalCapture{ReadCloser: body, prepared: p, stream: stream, buffer: responsebuffer.New(responsebuffer.BudgetOf(body), maxReplayCaptureBytes)}
	if streaming {
		stream.Observe(c.observe)
	} else {
		c.stream = nil
	}
	return c, c.commit, c.discard
}

type journalCapture struct {
	io.ReadCloser
	prepared   *PreparedHistory
	stream     *responseflow.Stream
	mu         sync.Mutex
	buffer     *responsebuffer.Buffer
	captureErr error
	eof        bool
	discarded  bool
	once       sync.Once
	commitErr  error
}

func (c *journalCapture) CanonicalStream() *responseflow.Stream { return c.stream }
func (c *journalCapture) ResponseBudget() *responsebuffer.Budget {
	return responsebuffer.BudgetOf(c.ReadCloser)
}
func (c *journalCapture) observe(event *responseflow.Event) {
	if !event.HasData {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.discarded || c.captureErr != nil {
		return
	}
	kind := jsonpeek.RootStringFieldScan(event.Data, "type")
	missingType := kind == ""
	if kind == "" {
		kind = string(event.Kind)
	}
	if kind != "response.output_item.done" && kind != "response.completed" && kind != "response.done" {
		return
	}
	write := func(p []byte) bool { _, c.captureErr = c.buffer.Write(p); return c.captureErr == nil }
	data := event.Data
	if missingType {
		data = bytes.TrimSpace(data)
		if len(data) == 0 || data[0] != '{' {
			c.captureErr = fmt.Errorf("invalid SSE event")
			return
		}
		if !write([]byte(`data: {"type":"` + kind + `",`)) {
			return
		}
		data = data[1:]
	} else if !write([]byte("data: ")) {
		return
	}
	for len(data) > 0 {
		part, rest, found := bytes.Cut(data, []byte{'\n'})
		if !write(part) {
			return
		}
		if found && !write([]byte{' '}) {
			return
		}
		data = rest
	}
	write([]byte{'\n'})
}
func (c *journalCapture) Read(p []byte) (int, error) {
	n, e := c.ReadCloser.Read(p)
	if c.stream == nil {
		c.mu.Lock()
		if !c.discarded && c.captureErr == nil && n > 0 {
			_, c.captureErr = c.buffer.Write(p[:n])
		}
		if e == io.EOF {
			c.eof = true
		} else if e != nil {
			c.captureErr = e
		}
		c.mu.Unlock()
	}
	return n, e
}
func (c *journalCapture) commit() error {
	c.once.Do(func() {
		defer func() { c.prepared.observeCommitFailure(c.commitErr) }()
		complete := false
		if c.stream != nil {
			complete = c.stream.ReadOutcome() == io.EOF
		}
		c.mu.Lock()
		if c.discarded {
			c.commitErr = journalCommitFailure("capture", "discarded", nil)
			c.mu.Unlock()
			return
		}
		if c.captureErr != nil {
			reason := journalFailureReason(c.captureErr)
			if reason == "store_error" {
				reason = "read_error"
			}
			c.commitErr = journalCommitFailure("capture", reason, c.captureErr)
			c.mu.Unlock()
			return
		}
		if c.stream == nil {
			complete = c.eof
		}
		if !complete {
			c.commitErr = journalCommitFailure("capture", "incomplete_capture", nil)
			c.mu.Unlock()
			return
		}
		body := c.buffer.Body()
		c.discarded = true
		c.mu.Unlock()
		defer body.Close()
		data, release, ok := body.BorrowBytes()
		if !ok {
			c.commitErr = journalCommitFailure("capture", "buffer_unavailable", nil)
			return
		}
		defer release()
		workspace, err := responsebuffer.JSONWorkspace(c.ResponseBudget(), data)
		if err != nil {
			c.commitErr = journalCommitFailure("decode", journalFailureReason(err), err)
			return
		}
		defer workspace.Release()
		if c.stream != nil {
			var extractionErr error
			data, extractionErr = extractJournalPayloadFromSSE(data)
			if extractionErr != nil {
				c.commitErr = extractionErr
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.commitErr = c.prepared.store(ctx, data)
	})
	return c.commitErr
}
func (c *journalCapture) discard() {
	c.mu.Lock()
	c.discarded = true
	_ = c.buffer.Close()
	c.mu.Unlock()
	c.prepared.Discard()
}
