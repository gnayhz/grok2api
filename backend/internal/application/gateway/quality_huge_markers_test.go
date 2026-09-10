package gateway

import (
	"fmt"
	"strings"
	"testing"
)

// Marker-like text must keep its meaning across the former 64 KiB / 1 MiB
// shortcuts, JSON field order, and arbitrary transport fragmentation.
func TestQualityLargeFramesUseProtocolFields(t *testing.T) {
	t.Parallel()
	for _, size := range []int{32, 65 << 10, (1 << 20) + 64} {
		for _, chunk := range []int{4096, 2 << 20} {
			t.Run(fmt.Sprintf("size=%d/chunk=%d", size, chunk), func(t *testing.T) {
				cases := []struct {
					protocol, payload string
					thinking          bool
					visible           bool
				}{
					{qualityProtocolResponses, `{"type":"response.output_text.delta","delta":"thinking_delta reasoning_text.delta ` + strings.Repeat("x", size) + `"}`, false, true},
					{qualityProtocolResponses, `{"delta":"` + strings.Repeat(" ", size) + `","type":"response.reasoning_text.delta"}`, false, false},
					{qualityProtocolResponses, `{"delta":"` + strings.Repeat(" ", size) + `plan","type":"response.reasoning_text.delta"}`, true, false},
					{qualityProtocolChat, `{"choices":[{"delta":{"content":"reasoning_content ` + strings.Repeat("x", size) + `"}}]}`, false, true},
					{qualityProtocolAnthropic, `{"delta":{"thinking":"` + strings.Repeat(" ", size) + `" ,"type":"thinking_delta"},"type":"content_block_delta"}`, false, false},
				}
				for _, tc := range cases {
					state := qualityScanState{protocol: tc.protocol}
					data := []byte("data: " + tc.payload + "\n\n")
					for len(data) > 0 {
						n := min(chunk, len(data))
						observeQualityChunk(&state, data[:n])
						data = data[n:]
					}
					if state.hasThinking != tc.thinking || (state.visibleRunes > 0) != tc.visible {
						t.Fatalf("%s: thinking=%v visible=%d", tc.protocol, state.hasThinking, state.visibleRunes)
					}
				}
			})
		}
	}
}
