package cli

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestSingleAttemptPolicyDisablesInternalRecovery(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		adapter, encrypted := newReasoningRecoveryTestAdapter(t)
		var calls atomic.Int32
		adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			return jsonHTTPResponse(request, status, `{"error":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}`), nil
		})
		response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
			Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
			Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", DisableAutomaticReplay: true,
			Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if calls.Load() != 1 || response.StatusCode != status {
			t.Fatalf("calls=%d status=%d", calls.Load(), response.StatusCode)
		}
	}
}
