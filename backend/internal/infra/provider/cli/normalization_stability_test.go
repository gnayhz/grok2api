package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestReasoningPatchPreservesExactInputNumbers(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","content":[{"text":"synthetic reasoning","sequence":9007199254740993}]},{"type":"function_call_output","call_id":"synthetic-call","output":{"id":9007199254740993,"amount":1.234567890123456789,"exponent":1e100}}]}`)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	patchReasoningTextTypes(payload)
	for _, value := range []string{`9007199254740993`, `1.234567890123456789`, `1e100`, `"type":"reasoning_text"`} {
		if !bytes.Contains(payload["input"], []byte(value)) {
			t.Fatalf("lost exact value %s", value)
		}
	}
	before := append([]byte(nil), payload["input"]...)
	patchReasoningTextTypes(payload)
	if !bytes.Equal(before, payload["input"]) {
		t.Fatal("normalization is not stable")
	}
}

func TestBuildNoneRejectedBeforeUpstream(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	adapter := NewAdapter(Config{BaseURL: server.URL + "/v1"}, cipher)
	defer adapter.http.CloseIdleConnections()
	for _, tc := range []struct{ operation, body string }{
		{"responses", `{"input":"synthetic","reasoning":{"effort":"none"}}`},
		{"chat", `{"messages":[{"role":"user","content":"synthetic"}],"reasoning_effort":"none"}`},
	} {
		for _, streaming := range []bool{false, true} {
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted}, Method: http.MethodPost, Path: "/responses", Operation: tc.operation, Model: "grok-4.6", Body: []byte(tc.body), NormalizeBody: true, Streaming: streaming})
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 400 || response.RequestValidation == nil || response.RequestValidation.Code != "unsupported_reasoning_effort" {
				t.Fatalf("unexpected validation response: %+v", response.RequestValidation)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid parameter consumed %d upstream calls", calls.Load())
	}
	for _, model := range []string{"grok-4.3", "grok-composer-2.5-fast", "synthetic-unknown"} {
		_, _, err := normalizeResponsesRequestWithMetadata([]byte(`{"input":"synthetic","reasoning":{"effort":"none"}}`), model, nil)
		var validation *responsesRequestError
		if errors.As(err, &validation) || err != nil {
			t.Fatalf("%s compatibility broken: %v", model, err)
		}
	}
}
