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
	data []byte
	err  error
	done bool
}

func (s *faultedSource) Read(p []byte) (int, error) {
	if !s.done {
		s.done = true
		n := copy(p, s.data)
		return n, nil
	}
	return 0, s.err
}

func (s *faultedSource) Close() error { return nil }

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

// TestClientDisconnectClassification 差分回归（2026-08-21 差分复现定案）：
// A 客户端断开（请求 ctx 已取消 + Read 返回 context.Canceled，生产中
//
//	client FIN 的真实形态）→ 必须记 client_disconnected，不得再污染
//	upstream_stream_interrupted。
//
// B 上游连接 RST（客户端存活）→ 仍记 upstream_stream_interrupted。
// C 上游空闲哨兵 → 仍记 upstream_stream_idle_timeout。
// D 防误判：客户端存活但 Read 返回裸 context.Canceled（上游侧中间层
//
//	取消形态）→ 不得判为客户端断开，按上游中断记账。
func TestClientDisconnectClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctxA, cancelA := context.WithCancel(context.Background())
	cancelA() // 客户端已断开
	codeA := runClassification(t, "A-client-disconnect", ctxA, context.Canceled)

	codeB := runClassification(t, "B-upstream-reset", context.Background(), io.ErrUnexpectedEOF)
	codeC := runClassification(t, "C-idle-sentinel", context.Background(), neterror.ErrUpstreamStreamIdleTimeout)
	codeD := runClassification(t, "D-upstream-midlayer-cancel", context.Background(), context.Canceled)

	if codeA != "client_disconnected" {
		t.Fatalf("用例A：客户端断开应记 client_disconnected，得到 %q", codeA)
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
