package inference

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/gin-gonic/gin"
)

// faultedSource 先吐一段正常 SSE 数据，随后返回给定错误——精确模拟
// transport 在指定故障形态下 body Read 的解阻塞行为。
type faultedSource struct {
	data   []byte
	err    error
	done   bool
	closed bool
}

func (s *faultedSource) Read(p []byte) (int, error) {
	if !s.done {
		s.done = true
		n := copy(p, s.data)
		return n, nil
	}
	return 0, s.err
}

func (s *faultedSource) Close() error { s.closed = true; return nil }

func runClassification(t *testing.T, name string, reqCtx context.Context, sourceErr error) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(reqCtx)
	ctx.Request = req
	captured := "<finalize-not-called>"
	result := &gateway.Result{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &faultedSource{data: []byte("data: hi\n\n"), err: sourceErr},
		Finalize:   func(_ gateway.Usage, _, errorCode string) { captured = errorCode },
	}
	h := &Handler{}
	h.writeProtocolResult(ctx, result, true, false, streamProtocolChat, "grok-4.6")
	t.Logf("%s: errorCode=%q clientCtxErr=%v", name, captured, reqCtx.Err())
	return captured
}

// A canceled request lifetime is not proof of a client FIN: server shutdown
// and request deadlines can cause the same context result. Upstream RST, idle
// and an upstream-only canceled context keep their independent classification.
func TestClientDisconnectClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctxA, cancelA := context.WithCancel(context.Background())
	cancelA() // Only cancellation is known; its initiator is not.
	codeA := runClassification(t, "A-request-cancellation", ctxA, context.Canceled)

	codeB := runClassification(t, "B-upstream-reset", context.Background(), io.ErrUnexpectedEOF)
	codeC := runClassification(t, "C-idle-sentinel", context.Background(), neterror.ErrUpstreamStreamIdleTimeout)
	codeD := runClassification(t, "D-upstream-midlayer-cancel", context.Background(), context.Canceled)

	if codeA != "request_canceled" {
		t.Fatalf("用例A：取消来源未知应记 request_canceled，得到 %q", codeA)
	}
	if codeB != "upstream_stream_interrupted" {
		t.Fatalf("用例B：上游中断应记 upstream_stream_interrupted，得到 %q", codeB)
	}
	if codeC != "upstream_stream_idle_timeout" {
		t.Fatalf("用例C：idle 哨兵应记 upstream_stream_idle_timeout，得到 %q", codeC)
	}
	if codeD != "upstream_stream_interrupted" {
		t.Fatalf("用例D：上游侧取消不得误判为客户端断开，得到 %q", codeD)
	}
}

// Both nonstreaming callers must preserve the same cancellation fact while
// still closing their source and finalizing the bytes they actually wrote.
func TestRequestCancellationAcrossJSONAndMedia(t *testing.T) {
	for _, media := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "media"}[media], func(t *testing.T) {
			requestCtx, cancel := context.WithCancel(context.Background())
			cancel()
			output := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(output)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestCtx)
			source := &faultedSource{data: []byte("partial"), err: context.Canceled}
			calls, code := 0, ""
			delivery := gateway.DeliveryStats{}
			result := &gateway.Result{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: source,
				Finalize:       func(_ gateway.Usage, _, value string) { calls++; code = value },
				RecordDelivery: func(value gateway.DeliveryStats) { delivery = value },
			}
			if media {
				result.Header.Set("Content-Type", "audio/wav")
				(&Handler{}).writeMediaResult(c, result)
			} else {
				(&Handler{}).writeProtocolResult(c, result, false, false, streamProtocolResponses, "")
			}
			if calls != 1 || code != "request_canceled" || !source.closed || delivery.Bytes != int64(output.Body.Len()) {
				t.Fatalf("calls=%d code=%q closed=%t delivered=%d actual=%d", calls, code, source.closed, delivery.Bytes, output.Body.Len())
			}
		})
	}
}
