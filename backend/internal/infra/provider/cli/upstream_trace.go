package cli

import (
	"io"

	"github.com/chenyme/grok2api/backend/internal/pkg/upstreamtrace"
)

// 采样工具已提升为共享包（Build 与 Console 双通道共用）；这里保留薄封装。
func upstreamTraceEnabled() (string, bool) { return upstreamtrace.Enabled() }

func traceUpstreamRequest(dir, op, model string, streaming bool, body []byte) {
	upstreamtrace.DumpRequest(dir, op, model, streaming, body)
}

func teeUpstreamStream(dir, op, model string, body io.ReadCloser) io.ReadCloser {
	return upstreamtrace.TeeStream(dir, op, model, body)
}

func traceUpstreamBody(dir, op, model string, data []byte) {
	upstreamtrace.DumpBody(dir, op, model, data)
}
