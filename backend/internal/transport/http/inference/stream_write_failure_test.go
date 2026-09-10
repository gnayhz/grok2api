package inference

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/gin-gonic/gin"
)

type flushFailureWriter struct {
	*httptest.ResponseRecorder
	failure error
}

func (w *flushFailureWriter) FlushError() error { return w.failure }

type writeFailureSource struct {
	reads  int
	closed bool
	data   string
}

func (r *writeFailureSource) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		return copy(p, r.data), nil
	}
	return 0, io.EOF
}
func (r *writeFailureSource) Close() error { r.closed = true; return nil }

func TestCopyStreamReportsFlushFailureBeforeAnotherRead(t *testing.T) {
	failure := errors.New("socket flush failed")
	writer := &flushFailureWriter{ResponseRecorder: httptest.NewRecorder(), failure: failure}
	c, _ := gin.CreateTestContext(writer)
	source := &writeFailureSource{data: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"}
	marked := false
	_, err := copyStream(c.Writer, source, streamProtocolChat, func() { marked = true })
	if !errors.Is(err, failure) || source.reads != 1 || marked {
		t.Fatalf("err=%v reads=%d firstToken=%v", err, source.reads, marked)
	}
}

func TestHandlerReleasesUpstreamAfterFlushFailure(t *testing.T) {
	writer := &flushFailureWriter{ResponseRecorder: httptest.NewRecorder(), failure: errors.New("socket flush failed")}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	source := &writeFailureSource{data: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"}
	code := ""
	(&Handler{}).writeProtocolResult(c, &gateway.Result{
		StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: source,
		Finalize: func(_ gateway.Usage, _, errCode string) { code = errCode },
	}, true, false, streamProtocolChat, "")
	if !source.closed || code != "client_disconnected" {
		t.Fatalf("closed=%v code=%s", source.closed, code)
	}
}

type shortStreamWriter struct{ *httptest.ResponseRecorder }

func (w *shortStreamWriter) Write(p []byte) (int, error) { return w.ResponseRecorder.Write(p[:3]) }

func TestCopyStreamCountsShortWriteAndStops(t *testing.T) {
	c, _ := gin.CreateTestContext(&shortStreamWriter{httptest.NewRecorder()})
	meta, err := copyStream(c.Writer, strings.NewReader("data: [DONE]\n\n"), streamProtocolChat, nil)
	if !errors.Is(err, io.ErrShortWrite) || meta.DeliveredBytes != 3 {
		t.Fatalf("err=%v bytes=%d", err, meta.DeliveredBytes)
	}
}
