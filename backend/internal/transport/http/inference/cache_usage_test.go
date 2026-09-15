package inference

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestCachePresenceSurvivesGenerationDeliveryAndAudit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, cache := range []int64{-1, 0, 128} {
			t.Run(fmt.Sprintf("stream=%v/cache=%d", stream, cache), func(t *testing.T) {
				var calls atomic.Int32
				upstream := completionHTTPUpstreamWithResponse(t, "grok-4.5", &calls, func(_ int32, body map[string]any) {
					usage := map[string]any{"input_tokens": 256, "output_tokens": 5, "total_tokens": 261}
					if cache >= 0 {
						usage["input_tokens_details"] = map[string]any{"cached_tokens": cache}
					}
					body["usage"] = usage
				})
				defer upstream.Close()
				fx := newProviderCompletionFixture(t, upstream.URL, "grok-4.5", account.ProviderBuild, nil, nil)
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"grok-4.5","input":"fictional cache test","stream":%v}`, stream)))
				request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
				request.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				fx.router.ServeHTTP(out, request)
				record := waitVoiceAudit(t, fx.audits)
				record, err := fx.audits.Get(request.Context(), record.ID)
				if err != nil || out.Code != 200 || calls.Load() != 1 || len(record.GenerationUsages) != 1 {
					t.Fatalf("delivery status=%d calls=%d details=%d err=%v", out.Code, calls.Load(), len(record.GenerationUsages), err)
				}
				for _, reported := range []*bool{record.CachedInputTokensReported, record.GenerationUsages[0].CachedInputTokensReported} {
					if reported == nil || *reported != (cache >= 0) {
						t.Fatalf("cache presence lost: %v want=%v", reported, cache >= 0)
					}
				}
				if record.InputTokens != 256 || record.CachedInputTokens != max(cache, 0) || record.GenerationUsages[0].CachedInputTokens != max(cache, 0) {
					t.Fatalf("cache counters changed: %+v", record.GenerationUsages)
				}
			})
		}
	}
}

func TestStreamCachePresenceRetainsZeroAcrossFrames(t *testing.T) {
	for _, fields := range []string{`"cache_read_input_tokens":0`, `"input_tokens_details":{"cached_tokens":0}`} {
		inspector := &responseInspector{}
		inspector.Inspect([]byte("data: {\"usage\":{\"input_tokens\":256," + fields + "}}\n\n"))
		inspector.Inspect([]byte("data: {\"usage\":{\"output_tokens\":5}}\n\n"))
		inspector.Finish()
		usage := inspector.Metadata().Usage
		if !usage.CachedInputTokensReported || usage.CachedInputTokens != 0 || usage.InputTokens != 256 || usage.OutputTokens != 5 {
			t.Fatalf("merged usage lost zero: %+v", usage)
		}
	}
}
