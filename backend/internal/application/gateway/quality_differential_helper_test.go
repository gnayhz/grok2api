package gateway

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// assertRawPathAgreement 是裸差分骨架：同一证据的流式表达与 body 表达
// （不经转换器）经对应 peek 后判决必须相等（rounds 73-78 差分族共用）。
func assertRawPathAgreement(t *testing.T, body, stream, protocol string) {
	t.Helper()
	streamReplay, streamVerdict, _, streamErr := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(stream)), protocol, QualityRetryRuntime{})
	if streamReplay != nil {
		_ = streamReplay.Close()
	}
	bodyReplay, bodyVerdict, _, bodyErr := peekQualityBody(io.NopCloser(strings.NewReader(body)), QualityRetryRuntime{})
	if bodyReplay != nil {
		_ = bodyReplay.Close()
	}
	if streamVerdict != bodyVerdict {
		t.Fatalf("verdict drift: stream=%s body=%s (streamErr=%v bodyErr=%v)", streamVerdict, bodyVerdict, streamErr, bodyErr)
	}
}

// assertDualModeAgreement 是转换器双模式差分的共用骨架：同一逻辑上游以
// JSON 与 SSE 两种表达经真转换器到真 peek，判决必须相等且等于 want。
// 四个双模式测试（chat/messages × 健康/降智，rounds 101-104）共用。
func assertDualModeAgreement(t *testing.T, jsonUpstream, streamUpstream string, operation string, bodyProtocol, streamProtocol string, opts conversation.ResponseOptions, want QualityVerdict) {
	t.Helper()
	streamConverted := conversation.ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(streamUpstream)), operation, opts)
	streamReplay, streamVerdict, _, streamErr := peekQualityStream(context.Background(), streamConverted, streamProtocol, QualityRetryRuntime{})
	if streamReplay != nil {
		_ = streamReplay.Close()
	}
	jsonConverted, convErr := conversation.ConvertResponseJSONWithOptions([]byte(jsonUpstream), operation, opts)
	if convErr != nil {
		t.Fatalf("json conversion err = %v", convErr)
	}
	bodyReplay, bodyVerdict, _, bodyErr := peekQualityBody(io.NopCloser(strings.NewReader(string(jsonConverted))), QualityRetryRuntime{})
	if bodyReplay != nil {
		_ = bodyReplay.Close()
	}
	if streamErr != nil || bodyErr != nil {
		t.Fatalf("peek errors: stream=%v body=%v", streamErr, bodyErr)
	}
	if streamVerdict != bodyVerdict || streamVerdict != want {
		t.Fatalf("dual-mode drift: stream=%s body=%s, want both %s", streamVerdict, bodyVerdict, want)
	}
}
