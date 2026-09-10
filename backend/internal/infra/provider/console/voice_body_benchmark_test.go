package console

import (
	"bytes"
	"fmt"
	"testing"
)

// This isolates the changed read/parse path, excluding DNS/HTTP/SQL scheduling.
func BenchmarkVoiceBodyReadAndDecode(b *testing.B) {
	for _, size := range []int{256, 16 << 10, 1 << 20} {
		prefix := []byte(`{"text":"synthetic","duration":3.45}`)
		payload := append(prefix, bytes.Repeat([]byte{' '}, size-len(prefix))...)
		b.Run(fmt.Sprintf("bytes_%d", len(payload)), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				data, err := benchmarkVoiceBodyRead(bytes.NewReader(payload), consoleVoiceBodyLimit)
				if err != nil {
					b.Fatal(err)
				}
				result, err := parseSTTResult(data)
				if err != nil || !result.DurationReported {
					b.Fatalf("invalid benchmark result: %v", err)
				}
			}
		})
	}
}
