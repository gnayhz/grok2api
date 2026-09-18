package inference

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/gin-gonic/gin"
)

type completionCountingFlusher struct {
	gin.ResponseWriter
	flushes int
	err     error
}

func (w *completionCountingFlusher) FlushError() error {
	w.flushes++
	if w.err != nil {
		return w.err
	}
	w.ResponseWriter.Flush()
	return nil
}

func TestCompletionBarrierFlushesOnlyNewFrames(t *testing.T) {
	const delta = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	const done = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_synthetic\",\"status\":\"completed\"}}\n\n"
	for _, fragment := range []string{"line", "byte"} {
		t.Run(fragment, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			writer := &completionCountingFlusher{ResponseWriter: c.Writer}
			input := strings.Repeat(delta, 4) + done
			var source io.Reader = completionLineReader(input)
			if fragment == "byte" {
				source = iotest.OneByteReader(strings.NewReader(input))
			}
			committed, firstToken := false, 0
			meta, err := copyStreamWithCompletion(writer, source, streamProtocolResponses, func() { firstToken++ }, "grok-4.6", func(responseMetadata) error {
				committed = true
				if writer.flushes != 4 || completionSuccessFrame(rec.Body.Bytes()) {
					t.Errorf("before commit: flushes=%d success=%t", writer.flushes, completionSuccessFrame(rec.Body.Bytes()))
				}
				return nil
			})
			if err != nil || !committed || writer.flushes != 5 || firstToken != 1 || meta.DeliveredEvents != 5 {
				t.Fatalf("err=%v committed=%t flushes=%d firstToken=%d events=%d", err, committed, writer.flushes, firstToken, meta.DeliveredEvents)
			}
			if strings.Count(rec.Body.String(), "hello") != 4 || !strings.Contains(rec.Body.String(), "response.completed") {
				t.Fatalf("incomplete delivery: %s", rec.Body.String())
			}
		})
	}
}

func TestCompletionBarrierFailedFlushRemainsPending(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	wantErr := errors.New("synthetic flush failure")
	writer := &completionCountingFlusher{ResponseWriter: c.Writer, err: wantErr}
	state := responsebuffer.NewState(responsebuffer.NewRequest(), 1<<20)
	defer state.Close()
	flushed := 0
	barrier := &completionBarrierWriter{ResponseWriter: writer, state: state, onFlushed: func() { flushed++ }}
	if _, err := barrier.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := barrier.FlushError(); !errors.Is(err, wantErr) || flushed != 0 {
		t.Fatalf("failed flush acknowledged: err=%v callbacks=%d", err, flushed)
	}
	writer.err = nil
	if err := barrier.FlushError(); err != nil || flushed != 1 || writer.flushes != 2 {
		t.Fatalf("pending flush lost: err=%v callbacks=%d calls=%d", err, flushed, writer.flushes)
	}
	if err := barrier.FlushError(); err != nil || flushed != 1 || writer.flushes != 2 {
		t.Fatalf("unchanged output flushed again: err=%v callbacks=%d calls=%d", err, flushed, writer.flushes)
	}
}

func completionLineReader(input string) io.Reader {
	lines := strings.SplitAfter(input, "\n")
	readers := make([]io.Reader, len(lines))
	for i, line := range lines {
		readers[i] = strings.NewReader(line)
	}
	return io.MultiReader(readers...)
}

func BenchmarkCompletionBarrierFragmented64Frames(b *testing.B) {
	const delta = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	const done = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_synthetic\",\"status\":\"completed\"}}\n\n"
	frames := strings.Repeat(delta, 64) + done
	gin.SetMode(gin.TestMode)
	var flushes int64
	b.ReportAllocs()
	for b.Loop() {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		writer := &completionCountingFlusher{ResponseWriter: c.Writer}
		if _, err := copyStreamWithCompletion(writer, completionLineReader(frames), streamProtocolResponses, nil, "grok-4.6", func(responseMetadata) error { return nil }); err != nil {
			b.Fatal(err)
		}
		flushes += int64(writer.flushes)
	}
	b.ReportMetric(float64(flushes)/float64(b.N), "flush/op")
}
