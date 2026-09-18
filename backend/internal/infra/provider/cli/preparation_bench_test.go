package cli

import (
	"encoding/json"
	"fmt"
	"net/http"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"strings"
	"testing"
)

func BenchmarkBuildCachePreparation(b *testing.B) {
	body, _ := json.Marshal(map[string]any{"model": "grok-4.6", "input": strings.Repeat("Stable context record. ", 4096)})
	b.ReportAllocs()
	for b.Loop() {
		prepared, _, err := prepareBuildPromptCacheRoute(body, "responses", "grok-4.6", "benchmark-cache-key", inferencedomain.AllowDisabledCacheTools)
		if err != nil || len(prepared) == 0 {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildResponsesPreparation(b *testing.B) {
	for _, size := range []int{4 << 10, 90 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			body, _ := json.Marshal(map[string]any{"model": "grok-4.6", "input": strings.Repeat("x", size)})
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			request := provider.ResponseResourceRequest{Method: http.MethodPost, Operation: "responses", Model: "grok-4.6", PromptCacheKey: "synthetic-benchmark-session", ToolCompatibilityPolicy: inferencedomain.AllowDisabledCacheTools}
			for b.Loop() {
				prepared, _, _, err := prepareBuildResponsesRequest(body, request)
				if err != nil || len(prepared) == 0 {
					b.Fatal(err)
				}
			}
		})
	}
}
