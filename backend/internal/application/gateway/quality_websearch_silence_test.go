package gateway

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// TestWebSearchSilenceSurvivesEvidenceDeadline：生产回归
// （创作控制台开联网搜索 → 504 quality_evidence_timeout 连环）。
// 服务端搜索的真实形态：created → web_search_call item.added →
// 静默数秒（搜索执行中）→ item.done → 正文。静默超过证据截止
// 三倍时判决不得中断——工具 item 头已是语义输出进行中。
func TestWebSearchSilenceSurvivesEvidenceDeadline(t *testing.T) {
	t.Parallel()
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		nl := "\n\n"
		_, _ = w.Write([]byte("data: " + `{"type":"response.created","response":{"id":"r_ws"}}` + nl))
		_, _ = w.Write([]byte("data: " + `{"type":"response.output_item.added","item":{"id":"ws_1","type":"web_search_call"}}` + nl))
		time.Sleep(300 * time.Millisecond) // 搜索静默：截止的 3 倍
		_, _ = w.Write([]byte("data: " + `{"type":"response.output_item.done","item":{"id":"ws_1","type":"web_search_call"}}` + nl))
		_, _ = w.Write([]byte("data: " + `{"type":"response.reasoning_text.delta","delta":"plan with sources"}` + nl))
		_, _ = w.Write([]byte("data: " + `{"type":"response.output_text.delta","delta":"answer with sources"}` + nl))
		_, _ = w.Write([]byte("data: " + `{"type":"response.completed","response":{"id":"r_ws","status":"completed"}}` + nl))
	}()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 100 * time.Millisecond, CreatedTimeout: 100 * time.Millisecond}
	replay, verdict, _, err := peekQualityStream(context.Background(), r, qualityProtocolResponses, cfg)
	if replay != nil {
		_ = replay.Close()
	}
	if err != nil {
		t.Fatalf("search silence must survive the evidence deadline, err=%v", err)
	}
	if verdict != QualityDeliver {
		t.Fatalf("verdict = %s, want deliver (thinking evidence after search)", verdict)
	}
}

// TestPlainDegradedSilenceStillTripsDeadline：修复不得放过真正的降智
// 静默——无工具 item 头的静默流仍被证据截止中止（守卫语义保持）。
func TestPlainDegradedSilenceStillTripsDeadline(t *testing.T) {
	t.Parallel()
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		nl := "\n\n"
		_, _ = w.Write([]byte("data: " + `{"type":"response.created","response":{"id":"r_q"}}` + nl))
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("data: " + `{"type":"response.completed"}` + nl))
	}()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 100 * time.Millisecond, CreatedTimeout: 500 * time.Millisecond}
	replay, _, _, err := peekQualityStream(context.Background(), r, qualityProtocolResponses, cfg)
	if replay != nil {
		_ = replay.Close()
	}
	if !errors.Is(err, errQualityEvidenceTimeout) {
		t.Fatalf("plain silence must still trip the evidence deadline, err=%v", err)
	}
}
