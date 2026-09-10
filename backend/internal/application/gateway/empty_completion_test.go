package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
)

func TestEmptyJSONCompletionRetriesOnlyBeforeSafeDelivery(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
		service, accounts := newGuardLoopService(t, adapter, "empty-first", "answer-second")
		service.UpdateQualityRetry(QualityRetryRuntime{Enabled: false})
		accepted := false
		adapter.responses[accounts[0].ID] = []scriptedBuildResponse{{status: 200, body: `{"status":"completed","output":[{"type":"reasoning","summary":[{"text":"plan"}]}]}`, acceptOutput: func() { accepted = true }}}
		adapter.responses[accounts[1].ID] = []scriptedBuildResponse{{status: 200, body: `{"id":"good","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`}}
		input := guardLoopInput("empty-json", false)
		if unsafe {
			input.Body = []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"execute"}],"tools":[{"type":"mcp"}]}`)
		}
		result, err := service.CreateChatCompletion(context.Background(), input)
		if accepted {
			t.Fatal("empty completion committed output")
		}
		if unsafe {
			if result != nil {
				result.Body.Close()
			}
			if !errors.Is(err, responsecheck.ErrEmptyOutput) || len(adapter.Attempts()) != 1 {
				t.Fatalf("unsafe err=%v attempts=%v", err, adapter.Attempts())
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(result.Body)
		finishTestResult(t, result, Usage{}, "good", "")
		_ = result.Body.Close()
		if readErr != nil || !strings.Contains(string(data), "answer") || len(adapter.Attempts()) != 2 {
			t.Fatalf("err=%v body=%s attempts=%v", readErr, data, adapter.Attempts())
		}
	}
}

func TestNativeJSONCompletionValidatedWithoutConverter(t *testing.T) {
	response := &provider.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[]}`))}
	defer func() { response.Body.Close() }()
	if err := prepareResponseDelivery(response, false, nil); !errors.Is(err, responsecheck.ErrEmptyOutput) {
		t.Fatalf("error=%v", err)
	}
}
