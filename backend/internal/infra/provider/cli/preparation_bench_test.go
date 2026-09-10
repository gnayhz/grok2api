package cli

import (
	"encoding/json"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
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
