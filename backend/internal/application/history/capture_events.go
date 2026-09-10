package history

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

type eventCapture struct {
	io.ReadCloser
	budget                                         *responsebuffer.Budget
	sawEOF                                         bool
	readErr                                        error
	closeOnce                                      sync.Once
	closeErr                                       error
	stream                                         *responseflow.Stream
	replay                                         *ReasoningReplay
	model, session                                 string
	compact                                        bool
	mu                                             sync.Mutex
	buffer                                         *responsebuffer.Buffer
	done, complete, accepted, committed, truncated bool
}

func (r *ReasoningReplay) captureEvents(body io.ReadCloser, stream *responseflow.Stream, model, session string, compact bool) (io.ReadCloser, func()) {
	capture := &eventCapture{ReadCloser: body, stream: stream, budget: stream.ResponseBudget(), replay: r, model: model, session: session, compact: compact, buffer: responsebuffer.New(stream.ResponseBudget(), maxReplayCaptureBytes)}
	stream.Observe(capture.observe)
	stream.OnClose(func() {
		complete := stream.ReadOutcome() == io.EOF
		capture.mu.Lock()
		capture.done, capture.complete = true, complete
		capture.mu.Unlock()
		capture.commit()
	})
	return capture, capture.accept
}
func (c *eventCapture) CanonicalStream() *responseflow.Stream  { return c.stream }
func (c *eventCapture) ResponseBudget() *responsebuffer.Budget { return c.budget }

func (r *ReasoningReplay) captureJSON(body io.ReadCloser, model, session string, compact bool) (io.ReadCloser, func()) {
	budget := responsebuffer.BudgetOf(body)
	capture := &eventCapture{ReadCloser: body, budget: budget, replay: r, model: model, session: session, compact: compact, buffer: responsebuffer.New(budget, maxReplayCaptureBytes)}
	return capture, capture.accept
}
func (c *eventCapture) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if c.stream != nil {
		return n, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done && !c.committed {
		if n > 0 && !c.truncated && !c.compact {
			if _, writeErr := c.buffer.Write(p[:n]); writeErr != nil {
				c.truncated = true
				_ = c.buffer.Close()
			}
		}
		if err == io.EOF {
			c.sawEOF = true
		} else if err != nil {
			c.readErr = err
		}
	}
	return n, err
}
func (c *eventCapture) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.ReadCloser.Close()
		if c.stream == nil {
			c.mu.Lock()
			c.done, c.complete = true, c.sawEOF && c.readErr == nil
			c.mu.Unlock()
			c.commit()
		}
	})
	return c.closeErr
}

func (c *eventCapture) observe(event *responseflow.Event) {
	if !event.HasData || c.compact {
		return
	}
	kind := jsonpeek.RootStringFieldScan(event.Data, "type")
	if kind == "" {
		kind = string(event.Kind)
	}
	if kind != "response.output_item.done" && kind != "response.completed" && kind != "response.done" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done || c.committed || c.truncated {
		return
	}
	// JSON permits literal newlines only as whitespace outside strings. Store
	// one data line per assembled payload for the completed-payload extractor.
	write := func(data []byte) bool {
		if _, err := c.buffer.Write(data); err != nil {
			c.truncated = true
			_ = c.buffer.Close()
			return false
		}
		return true
	}
	if !write([]byte("data: ")) {
		return
	}
	data := event.Data
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

func (c *eventCapture) accept() { c.mu.Lock(); c.accepted = true; c.mu.Unlock(); c.commit() }
func (c *eventCapture) DiscardOutput() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.committed = true
	_ = c.buffer.Close()
}
func (c *eventCapture) commit() {
	c.mu.Lock()
	if !c.done || !c.accepted || c.committed {
		c.mu.Unlock()
		return
	}
	c.committed = true
	valid := c.complete && !c.truncated
	body := c.buffer.Body()
	c.mu.Unlock()
	defer body.Close()
	if !valid {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if c.compact {
		c.replay.Clear(ctx, c.model, c.session)
		return
	}
	data, release, _ := body.BorrowBytes()
	defer release()
	workspace, err := responsebuffer.JSONWorkspace(c.budget, data)
	if err != nil {
		return
	}
	defer workspace.Release()
	if c.stream == nil {
		c.replay.StoreFromCompleted(ctx, c.model, c.session, data)
		return
	}
	if completed, ok := extractCompletedPayloadFromSSE(data); ok {
		c.replay.StoreFromCompleted(ctx, c.model, c.session, completed)
	}
}
