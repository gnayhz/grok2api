package inference

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	upstreamws "github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/gin-gonic/gin"
	clientws "github.com/gorilla/websocket"
)

const voiceWSMessageLimit = 16 << 20

var voiceWSUpgrader = clientws.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	CheckOrigin: func(*http.Request) bool {
		return true
	},
	EnableCompression: true,
}

func (h *Handler) proxyRealtimeWebSocket(c *gin.Context) {
	h.proxyVoiceWebSocket(c, "/realtime")
}

func (h *Handler) proxySTTWebSocket(c *gin.Context) {
	if !clientws.IsWebSocketUpgrade(c.Request) {
		writeOpenAIError(c, http.StatusMethodNotAllowed, "invalid_request", "STT 流式接口需要 WebSocket Upgrade")
		return
	}
	h.proxyVoiceWebSocket(c, "/stt")
}

func (h *Handler) proxyVoiceWebSocket(c *gin.Context, pathValue string) {
	if !clientws.IsWebSocketUpgrade(c.Request) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "请求不是有效的 WebSocket Upgrade")
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(query["model"]) > 1 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "WebSocket query 无效或 model 重复")
		return
	}
	model := strings.TrimSpace(query.Get("model"))
	query.Del("model")
	session, err := h.gateway.OpenVoiceWebSocket(c.Request.Context(), gateway.VoiceWebSocketInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, Path: pathValue, Query: query,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}

	if session.BeginDelivery != nil {
		if err := session.BeginDelivery(); err != nil {
			session.Finalize(gateway.VoiceWebSocketOutcome{ErrorCode: "request_canceled"})
			return
		}
	}
	clientConn, err := voiceWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		session.Finalize(gateway.VoiceWebSocketOutcome{ErrorCode: "client_upgrade_failed"})
		return
	}
	clientConn.SetReadLimit(voiceWSMessageLimit)
	session.Conn.SetReadLimit(voiceWSMessageLimit)
	outcome := relayVoiceWebSocket(c.Request.Context(), session.Conn, clientConn)
	outcome.ClientUpgraded = true
	session.Finalize(outcome)
}

// The relay owns writes and connection shutdown; the Provider observes upstream
// protocol facts before forwarding. Join both pumps before handing off receipt
// counts so a concurrent final upstream frame cannot disappear from accounting.
func relayVoiceWebSocket(ctx context.Context, upstream, client voiceMessageConn) gateway.VoiceWebSocketOutcome {
	var closeOnce sync.Once
	closeAll := func() { closeOnce.Do(func() { _ = client.Close(); _ = upstream.Close() }) }
	defer closeAll()
	type pumpResult struct {
		upstreamSide bool
		result       voiceWSPumpResult
	}
	results := make(chan pumpResult, 2)
	start := func(upstreamSide bool, read func() (int, []byte, error), write func(int, []byte) error) {
		go func() {
			result := voiceWSPumpResult{}
			if err := batch.Do(ctx, func(context.Context) error { result = proxyVoiceWSPump(read, write); return nil }); err != nil {
				result.err = err
			}
			results <- pumpResult{upstreamSide: upstreamSide, result: result}
		}()
	}
	start(false, client.ReadMessage, upstream.WriteMessage)
	start(true, upstream.ReadMessage, client.WriteMessage)
	outcome := gateway.VoiceWebSocketOutcome{}
	collected := 0
	accept := func(result pumpResult) {
		if result.upstreamSide {
			outcome.DeliveredBytes = result.result.bytes
			outcome.DeliveredEvents = result.result.events
		}
	}
	select {
	case first := <-results:
		collected++
		accept(first)
		if ctx.Err() != nil {
			outcome.ErrorCode = "request_canceled"
		} else if !isNormalVoiceWSClose(first.result.err) {
			if (first.upstreamSide && !first.result.writeFailed) || (!first.upstreamSide && first.result.writeFailed) {
				outcome.ErrorCode, outcome.UpstreamFailed = "upstream_stream_interrupted", true
			} else {
				outcome.ErrorCode = "client_stream_interrupted"
			}
		}
	case <-ctx.Done():
		outcome.ErrorCode = "request_canceled"
	}
	closeAll()
	for ; collected < 2; collected++ {
		accept(<-results)
	}
	return outcome
}

type voiceMessageConn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	Close() error
}

type voiceWSPumpResult struct {
	err           error
	writeFailed   bool
	bytes, events int64
}

func proxyVoiceWSPump(read func() (int, []byte, error), write func(int, []byte) error) voiceWSPumpResult {
	result := voiceWSPumpResult{}
	for {
		messageType, payload, err := read()
		if err != nil {
			result.err = err
			return result
		}
		if err := write(messageType, payload); err != nil {
			result.err, result.writeFailed = err, true
			return result
		}
		result.bytes += int64(len(payload))
		result.events++
	}
}

func isNormalVoiceWSClose(err error) bool {
	if err == nil || err == io.EOF {
		return true
	}
	return clientws.IsCloseError(err, clientws.CloseNormalClosure, clientws.CloseGoingAway) ||
		upstreamws.IsCloseError(err, upstreamws.CloseNormalClosure, upstreamws.CloseGoingAway)
}
