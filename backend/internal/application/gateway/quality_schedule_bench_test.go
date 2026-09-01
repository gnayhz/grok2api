package gateway

import (
	"strings"
	"testing"
)

// BenchmarkQualityLivenessSchedule：制度表在扣留点的每请求成本（一次
// Unmarshal 同时取 tools 与 effort）。对照请求本身的秒级时延。
func BenchmarkQualityLivenessSchedule(b *testing.B) {
	small := []byte(`{"model":"grok-4.6","input":[{"role":"user","content":"hi"}],"reasoning":{"effort":"low"}}`)
	var bigBuilder strings.Builder
	bigBuilder.WriteString(`{"model":"grok-4.6","input":[{"role":"user","content":"`)
	chunk := strings.Repeat("这是一段用于测量的较长对话历史，模拟真实的多轮请求负载。", 20)
	for i := 0; i < 200; i++ {
		bigBuilder.WriteString(chunk)
	}
	bigBuilder.WriteString(`"}],"reasoning":{"effort":"high"},"tools":[{"type":"web_search"}]}`)
	big := []byte(bigBuilder.String())
	b.Run("small-body", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = qualityLivenessSchedule(small, "responses", QualityRetryRuntime{})
		}
	})
	b.Run("128k-body", func(b *testing.B) {
		b.SetBytes(int64(len(big)))
		for i := 0; i < b.N; i++ {
			_ = qualityLivenessSchedule(big, "chat", QualityRetryRuntime{})
		}
	})
}
