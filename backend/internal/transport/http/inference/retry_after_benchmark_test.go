package inference

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func BenchmarkRetryAfterHTTPAdapter(b *testing.B) {
	for _, source := range []string{"header", "body"} {
		b.Run(source, func(b *testing.B) {
			var calls atomic.Int32
			f := newResponseRetentionFixture(b, "sqlite", account.ProviderConsole, responseRetentionHooks{beforeResponse: func(w http.ResponseWriter, r *http.Request) bool {
				calls.Add(1)
				if source == "header" {
					w.Header().Set("Retry-After", "300")
				}
				w.WriteHeader(429)
				body := "Requests per Minute (actual/limit): 2/1"
				if source == "body" {
					body += ". Resets in: 300s"
				}
				_, _ = io.WriteString(w, body)
				return true
			}})
			credential, err := f.accounts.Get(context.Background(), f.accountID)
			if err != nil {
				b.Fatal(err)
			}
			request := provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: f.model, Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.5","input":"hello"}`)}
			run := func() {
				response, err := f.adapter.ForwardResponse(context.Background(), request)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != 429 || response.RateLimit == nil || response.RateLimit.RetryAfter != 300*time.Second {
					b.Fatalf("status=%d metadata=%+v", response.StatusCode, response.RateLimit)
				}
			}
			run()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				run()
			}
			b.StopTimer()
			if calls.Load() != int32(b.N+1) || f.generated.Load() != 0 {
				b.Fatalf("responses=%d generated=%d", calls.Load(), f.generated.Load())
			}
			b.ReportMetric(1, "responses/op")
			b.ReportMetric(0, "generations/op")
		})
	}
}

func BenchmarkRetryAfterHTTPCooling(b *testing.B) {
	var calls atomic.Int32
	f := newResponseRetentionFixture(b, "sqlite", account.ProviderBuild, responseRetentionHooks{beforeResponse: func(w http.ResponseWriter, r *http.Request) bool {
		calls.Add(1)
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(429)
		return true
	}})
	f.create(b, false, nil, "cost")
	f.restartGateway()
	run := func() {
		got := f.create(b, false, nil, "cost")
		if got.Code != 429 || got.Header().Get("Retry-After") == "" {
			b.Fatalf("status=%d body=%s", got.Code, got.Body.String())
		}
	}
	run()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		run()
	}
	b.StopTimer()
	if calls.Load() != 1 || f.generated.Load() != 0 {
		b.Fatalf("responses=%d generated=%d", calls.Load(), f.generated.Load())
	}
	record := f.lastAudit(b)
	if record.InputTokens != 0 || record.OutputTokens != 0 || len(record.GenerationUsages) != 0 || record.ErrorCode != "upstream_cooling" {
		b.Fatalf("invalid refusal audit: %+v", record)
	}
	b.ReportMetric(0, "responses/op")
	b.ReportMetric(0, "generations/op")
}
