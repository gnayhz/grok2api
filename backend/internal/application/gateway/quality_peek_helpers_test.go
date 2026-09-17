package gateway

import (
	"context"
	"io"

	"github.com/chenyme/grok2api/backend/internal/port/provider"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

// 本文件集中收纳"仅测试消费"的判决/读取便利形态:生产路径全部使用带
// budget/report 的入口(peekQualityBodyReportWithBudget、
// newQualityReadPumpWithBudget),这些薄投影只为测试断言的可读性存在。

// peekQualityBody 非流式响应的完整 body 判决(4 值形态,测试断言用)。
func peekQualityBody(body io.ReadCloser, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, error) {
	replay, verdict, usage, _, err := peekQualityBodyReport(body, cfg)
	return replay, verdict, usage, err
}

func peekQualityBodyReport(body io.ReadCloser, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, qualityHoldFingerprint, error) {
	return peekQualityBodyReportWithBudget(body, cfg, responsebuffer.BudgetOf(body))
}

// shouldHoldQualityStream 是 qualityHoldExemptReason 的布尔投影(测试断言用)。
func shouldHoldQualityStream(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg QualityRetryRuntime, jurisdiction QualityJurisdiction) bool {
	return qualityHoldExemptReason(input, ownership, route, operation, cfg, jurisdiction) == ""
}

func newQualityReadPump(source io.ReadCloser) *qualityReadPump {
	return newQualityReadPumpWithBudget(source, responsebuffer.BudgetOf(source))
}

// observeQualityChunk collects complete traces for diagnostic replay. The live
// peek scans its retained prefix directly, avoiding a second frame buffer.
func observeQualityChunk(state *qualityScanState, chunk []byte) {
	if state == nil || len(chunk) == 0 {
		return
	}
	previous := len(state.pending)
	data := chunk
	if previous > 0 {
		if previous+len(chunk) > qualityHoldMaxBufferBytes {
			state.pending = state.pending[:0]
			return
		}
		state.pending = append(state.pending, chunk...)
		data = state.pending
	}
	consumed, _, _ := scanQualityLines(state, data, previous, nil)
	tail := data[consumed:]
	if len(tail) > qualityHoldMaxBufferBytes {
		state.pending = state.pending[:0]
		return
	}
	state.pending = append(state.pending[:0], tail...)
}

// probeOutcome 是 qualityProbeMeasurement 的 2 值投影(测试断言用)。
func probeOutcome(s *Service, ctx context.Context, request provider.ResponseResourceRequest, hold QualityRetryRuntime) (qualitymodel.MeasurementOutcome, string) {
	result := s.qualityProbeMeasurement(ctx, request, hold)
	return result.Outcome, result.Reason
}
