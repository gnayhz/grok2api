package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	clientkey "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func TestGuardAcceptsOnlyDeliveredAttemptBeforeMessagesConversion(t *testing.T) {
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	service, credentials := newGuardLoopService(t, adapter, "context-rejected", "context-accepted")
	adapter.checkRequest = func(request provider.ResponseResourceRequest) {
		if !request.DeferOutputCommit {
			t.Error("guarded request did not defer context effects")
		}
	}
	var rejected, accepted, conversions atomic.Int32
	convert := func(raw []byte) ([]byte, error) {
		if accepted.Load() != 0 {
			t.Error("cache accepted before conversion succeeded")
		}
		conversions.Add(1)
		return conversation.ConvertResponseJSONWithOptions(raw, conversation.OperationMessages, conversation.ResponseOptions{})
	}
	adapter.responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: `{"output":[{"type":"reasoning","encrypted_content":"cipher"},{"type":"message","content":[{"type":"output_text","text":"REJECTED"}]}]}`, acceptOutput: func() { rejected.Add(1) }, convertJSON: convert}}
	adapter.responses[credentials[1].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: `{"id":"healthy","output":[{"type":"reasoning","summary":[{"text":"plan"}]},{"type":"message","content":[{"type":"output_text","text":"ACCEPTED"}]}]}`, acceptOutput: func() { accepted.Add(1) }, convertJSON: convert}}
	result, err := service.CreateMessage(context.Background(), Input{RequestID: "context-commit", ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: 1, Name: "k"}, PublicModel: "grok-4.6", Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hello"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	body, err := io.ReadAll(result.Body)
	finishTestResult(t, result, Usage{}, "healthy", "")
	if err != nil || !strings.Contains(string(body), "ACCEPTED") || strings.Contains(string(body), "REJECTED") {
		t.Fatalf("body=%s err=%v", body, err)
	}
	if rejected.Load() != 0 || accepted.Load() != 1 || conversions.Load() != 1 {
		t.Fatalf("rejected=%d accepted=%d conversions=%d", rejected.Load(), accepted.Load(), conversions.Load())
	}
}
