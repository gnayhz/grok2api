package gateway

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

// This measures the live request path: healthy evidence arrives in the first
// read, followed by a coalesced burst that the guard does not need to inspect.
func BenchmarkGuardHealthyPeek(b *testing.B) {
	for _, tc := range []struct{ name, protocol, payload string }{
		{"responses", qualityProtocolResponses, benchResponsesStream(250)},
		{"chat", qualityProtocolChat, benchStream(500)},
		{"anthropic", qualityProtocolAnthropic, benchAnthropicStream(250)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(tc.payload)), tc.protocol, QualityRetryRuntime{})
				if err != nil || verdict != QualityDeliver {
					b.Fatalf("verdict=%s err=%v", verdict, err)
				}
				replay.Close()
			}
		})
	}
}

func BenchmarkGuardCanonicalHealthyPeek(b *testing.B) {
	payload := benchResponsesStream(250)
	b.ReportAllocs()
	for b.Loop() {
		body := responseflow.New(io.NopCloser(strings.NewReader(payload)), nil)
		replay, verdict, _, err := peekQualityStream(context.Background(), body, qualityProtocolResponses, QualityRetryRuntime{})
		if err != nil || verdict != QualityDeliver {
			b.Fatalf("verdict=%s err=%v", verdict, err)
		}
		_ = replay.Close()
	}
}
