package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

type auxiliaryReadBody struct {
	io.ReadCloser
	once    sync.Once
	prepare func() error
	err     error
}

func (b *auxiliaryReadBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.err = b.prepare() })
	if b.err != nil {
		return 0, b.err
	}
	return b.ReadCloser.Read(p)
}

func TestPhysicalBudgetLivesUntilDeliveryFinalization(t *testing.T) {
	s, records := completionService(t, nil)
	s.UpdateQualityRetry(QualityRetryRuntime{Enabled: false})
	var auxiliaryCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auxiliaryCalls.Add(1)
		_, _ = io.WriteString(w, "asset")
	}))
	defer upstream.Close()
	var executionCtx context.Context
	s.providers = provider.NewRegistry(resourceTestAdapter{&scriptedBuildAdapter{}, func(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
		executionCtx = ctx
		primary := attemptmeta.Begin(ctx, attemptmeta.Path{})
		if err := infraegress.BeginDirectPhysicalCall(primary); err != nil {
			return nil, err
		}
		raw := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}
		infraegress.RecordDirectPhysicalCall(primary, raw, nil)
		_ = raw.Body.Close()
		body := &auxiliaryReadBody{ReadCloser: io.NopCloser(strings.NewReader(completionJSON)), prepare: func() error {
			auxiliary := attemptmeta.Begin(ctx, attemptmeta.Path{})
			if err := infraegress.BeginDirectPhysicalCall(auxiliary); err != nil {
				return err
			}
			r, _ := http.NewRequestWithContext(auxiliary, http.MethodGet, upstream.URL, nil)
			response, err := upstream.Client().Do(r)
			infraegress.RecordDirectPhysicalCall(auxiliary, response, err)
			if err != nil {
				return err
			}
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			return err
		}}
		return &provider.Response{Attempt: attemptmeta.FromContext(primary), StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
	}})
	result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("late-auxiliary", true))
	if err != nil {
		t.Fatal(err)
	}
	if auxiliaryCalls.Load() != 0 {
		t.Fatal("fixture read before stream handoff")
	}
	readCompletion(t, result)
	if auxiliaryCalls.Load() != 1 {
		t.Fatal("delivery could not use remaining execution budget")
	}
	finishTestResult(t, result, Usage{Reported: true, InputTokens: 20, OutputTokens: 5}, "resp_auxiliary", "")
	_ = result.Body.Close()
	if record := oneCompletionAudit(t, records); record.GenerationOutcome != "completed" {
		t.Fatalf("completion=%+v", record)
	}
	late := attemptmeta.Begin(context.WithoutCancel(executionCtx), attemptmeta.Path{})
	if err := infraegress.BeginDirectPhysicalCall(late); !errors.Is(err, infraegress.ErrPhysicalCallLimit) {
		t.Fatalf("finalization did not close execution budget: %v", err)
	}
}
