package inference

import (
	"bytes"
	"testing"
)

// BenchmarkRewriteResponsesDeltaLine 量化增量帧(推理阶段每 token 一帧)
// 的 compat 重写开销与分配。零分配口径(InternType+RootStringBytes)下
// 每次 rewrite 应为 0 alloc;回归(StringField 分配)时为 2 alloc/op。
func BenchmarkRewriteResponsesDeltaLine(b *testing.B) {
	line := []byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"item_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello token\"}\n\n")
	state := &responsesCompatState{model: "grok-4.5"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := rewriteResponsesDataLine(line, state)
		if !bytes.Equal(result, line) {
			b.Fatalf("addressed delta frame must pass through unchanged: %s", result)
		}
	}
}
