package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/gin-gonic/gin"
)

// completionBarrierWriter forwards complete nonterminal SSE frames immediately.
// The first success frame and all subsequent bytes remain bounded and private
// until clean response validation and durable commit have both succeeded.
type completionBarrierWriter struct {
	gin.ResponseWriter
	pending, held []byte
	holding       bool
	state         *responsebuffer.State
	written       int64
	events        int64
	onFlushed     func()
}

func (w *completionBarrierWriter) Write(p []byte) (int, error) {
	if len(w.pending)+len(w.held)+len(p) > 8<<20 {
		return 0, responsebuffer.ErrLimit
	}
	if err := w.state.Grow(0, 6*(len(w.pending)+len(w.held)+len(p))); err != nil {
		return 0, err
	}
	if w.holding {
		w.held = append(w.held, p...)
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	for {
		end := bytes.Index(w.pending, []byte("\n\n"))
		sep := 2
		crlf := bytes.Index(w.pending, []byte("\r\n\r\n"))
		if crlf >= 0 && (end < 0 || crlf < end) {
			end, sep = crlf, 4
		}
		if end < 0 {
			break
		}
		frame := w.pending[:end+sep]
		if completionSuccessFrame(frame) {
			w.holding = true
			w.held = append(w.held, w.pending...)
			w.pending = nil
			break
		}
		n, err := w.ResponseWriter.Write(frame)
		w.written += int64(n)
		if n == len(frame) {
			w.events += completionDataEvents(frame)
		}
		if err != nil {
			return 0, err
		}
		if n != len(frame) {
			return 0, io.ErrShortWrite
		}
		w.pending = w.pending[end+sep:]
	}
	return len(p), nil
}
func (w *completionBarrierWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *completionBarrierWriter) WriteHeaderNow() {
	if w.written > 0 {
		w.ResponseWriter.WriteHeaderNow()
	}
}
func (w *completionBarrierWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *completionBarrierWriter) Flush()                      { _ = w.FlushError() }
func (w *completionBarrierWriter) FlushError() error {
	if w.written == 0 {
		return nil
	}
	if err := flushStreamResponse(w.ResponseWriter); err != nil {
		return err
	}
	if w.onFlushed != nil {
		w.onFlushed()
	}
	return nil
}

func completionSuccessFrame(frame []byte) bool {
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("event:")) {
			kind := string(bytes.TrimSpace(line[6:]))
			if completionSuccessEventKind(kind) {
				return true
			}
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimSpace(line[5:])...)
			data = append(data, '\n')
		}
	}
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		return true
	}
	var event struct {
		Type  string `json:"type"`
		Delta struct {
			StopReason *string `json:"stop_reason"`
		} `json:"delta"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	if completionSuccessEventType(event.Type) || event.Delta.StopReason != nil {
		return true
	}
	for _, c := range event.Choices {
		if c.FinishReason != nil {
			return true
		}
	}
	return false
}

func copyStreamWithCompletion(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol, firstToken func(), model string, commit func(responseMetadata) error) (responseMetadata, error) {
	if commit == nil {
		return copyStreamWithFallbackModel(writer, source, protocol, firstToken, model)
	}
	state := responsebuffer.NewState(responseReaderBudget(source), 64<<20)
	defer state.Close()
	barrier := &completionBarrierWriter{ResponseWriter: writer, state: state}
	observed := &completionReadOutcome{Reader: source, budget: responseReaderBudget(source)}
	tokenReady := false
	markFirstToken := func() {
		if tokenReady && firstToken != nil {
			firstToken()
			firstToken = nil
		}
	}
	barrier.onFlushed = markFirstToken
	meta, err := copyStreamWithFallbackModel(barrier, observed, protocol, func() {
		tokenReady = true
		if barrier.written > 0 {
			markFirstToken()
		}
	}, model)
	meta.DeliveredBytes = barrier.written
	meta.DeliveredEvents = barrier.events
	if err != nil {
		if barrier.holding && !errors.Is(err, errClientStreamWrite) {
			return writeCompletionAbort(writer, protocol, err, meta, int(barrier.written))
		}
		return meta, err
	}
	if observed.err != io.EOF {
		err = fmt.Errorf("%w: %v", errUpstreamStreamRead, observed.err)
		return writeCompletionAbort(writer, protocol, err, meta, int(barrier.written))
	}
	// A final unterminated SSE frame can still be semantically terminal.
	if len(barrier.pending) > 0 && completionSuccessFrame(barrier.pending) {
		barrier.held = barrier.pending
		barrier.pending = nil
		barrier.holding = true
	}
	if !barrier.holding {
		err := fmt.Errorf("%w: missing success boundary", inferencedomain.ErrCompletionCommit)
		return writeCompletionAbort(writer, protocol, err, meta, int(barrier.written))
	}
	if err = commit(meta); err != nil {
		err = fmt.Errorf("%w: %w", inferencedomain.ErrCompletionCommit, err)
		return writeCompletionAbort(writer, protocol, err, meta, int(barrier.written))
	}
	n, err := writeStreamChunk(writer, barrier.held)
	meta.DeliveredBytes += int64(n)
	if n == len(barrier.held) {
		meta.DeliveredEvents += completionDataEvents(barrier.held)
	}
	if err == nil {
		markFirstToken()
	}
	return meta, err
}

func copyJSONWithCompletion(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol, commit func(responseMetadata) error) (responseMetadata, error) {
	if commit == nil {
		return copyJSON(writer, source, protocol)
	}
	var data []byte
	if body, ok := source.(io.ReadCloser); ok {
		if borrowed, release, ok := responsebuffer.Borrow(body); ok {
			data = borrowed
			defer release()
		}
	}
	if data == nil {
		buffer := responsebuffer.New(responseReaderBudget(source), maxJSONResponseTransferBytes)
		defer buffer.Close()
		if _, err := io.Copy(buffer, source); err != nil {
			return responseMetadata{}, err
		}
		data = buffer.Bytes()
	}
	if len(data) > maxJSONResponseTransferBytes {
		return responseMetadata{}, errResponseTransferLimit
	}
	// Image data is opaque payload, not text usage metadata. Its Provider has
	// already validated the image protocol. A borrowed buffer avoids a second
	// full base64 copy and this raw JSON check allocates no semantic tree.
	var workspace *responsebuffer.Lease
	if protocol != streamProtocolImage {
		var err error
		workspace, err = responsebuffer.JSONWorkspace(responseReaderBudget(source), data)
		if err != nil {
			return responseMetadata{}, err
		}
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		workspace.Release()
		return responseMetadata{}, fmt.Errorf("%w: invalid JSON response", errUpstreamStreamIncomplete)
	}
	metadata := responseMetadata{}
	if protocol != streamProtocolImage {
		metadata = normalizeMetadataUsage(extractMetadata(data), protocol)
	}
	invalidCompletion := jsonpeek.RootStringFieldScan(data, "type") == "error"
	if raw := bytes.TrimSpace(jsonpeek.RootRawValue(data, "error")); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		invalidCompletion = true
	}
	switch jsonpeek.RootStringFieldScan(data, "status") {
	case "failed", "incomplete", "cancelled", "canceled", "queued", "in_progress":
		invalidCompletion = true
	}
	workspace.Release()
	if invalidCompletion {
		return metadata, errUpstreamStreamFailed
	}
	if err := commit(metadata); err != nil {
		return metadata, fmt.Errorf("%w: %w", inferencedomain.ErrCompletionCommit, err)
	}
	// Copy the already validated bytes without re-reading or re-parsing. Keep
	// upstream metadata even when the downstream accepts only part of the body.
	for offset := 0; offset < len(data); {
		chunk := data[offset:min(offset+responseCopyBufferBytes, len(data))]
		if err := setResponseWriteDeadline(writer); err != nil {
			return metadata, fmt.Errorf("%w: %w", errClientStreamWrite, err)
		}
		n, err := writer.Write(chunk)
		metadata.DeliveredBytes += int64(n)
		if err != nil {
			return metadata, fmt.Errorf("%w: %w", errClientStreamWrite, err)
		}
		if n != len(chunk) {
			return metadata, fmt.Errorf("%w: %w", errClientStreamWrite, io.ErrShortWrite)
		}
		offset += n
	}
	metadata.DeliveredEvents = 1
	return metadata, nil
}

type completionReadOutcome struct {
	io.Reader
	budget *responsebuffer.Budget
	err    error
}

func (r *completionReadOutcome) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	if e != nil {
		r.err = e
	}
	return n, e
}
func (r *completionReadOutcome) Close() error                           { return nil }
func (r *completionReadOutcome) ResponseBudget() *responsebuffer.Budget { return r.budget }
func completionDataEvents(p []byte) int64 {
	var n int64
	for _, line := range bytes.Split(p, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			n++
		}
	}
	return n
}

func writeCompletionAbort(writer gin.ResponseWriter, protocol streamProtocol, cause error, meta responseMetadata, transferred int) (responseMetadata, error) {
	trailer := streamAbortTrailer(protocol, cause, meta, nil)
	if protocol == streamProtocolChat {
		trailer = bytes.TrimSuffix(trailer, []byte("data: [DONE]\n\n"))
	}
	if len(trailer) > 0 && transferred+len(trailer) <= maxStreamResponseTransferBytes {
		n, err := writeStreamChunk(writer, trailer)
		meta.DeliveredBytes += int64(n)
		if n == len(trailer) {
			meta.DeliveredEvents += completionDataEvents(trailer)
		}
		return meta, errors.Join(cause, err)
	}
	return meta, cause
}
