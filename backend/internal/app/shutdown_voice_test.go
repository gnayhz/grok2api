package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/gorilla/websocket"
)

// Exercise the real application router, Console DPoP/WS adapter, relay, gateway
// completion, account quota, quality journal and durable billing writer at stop.
func TestRunShutdownKeepsKnownVoiceUsageAndBilling(t *testing.T) {
	upstreamDone := make(chan struct{})
	const transcript = `{"type":"transcript.done","duration":3.45}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/dpop/token" {
			var input struct {
				JWK map[string]string `json:"jwk"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			canonical, _ := json.Marshal(map[string]string{"crv": input.JWK["crv"], "kty": input.JWK["kty"], "x": input.JWK["x"], "y": input.JWK["y"]})
			digest := sha256.Sum256(canonical)
			claims, _ := json.Marshal(map[string]any{"sub": "synthetic", "exp": time.Now().Add(5 * time.Minute).Unix(), "cnf": map[string]any{"jkt": base64.RawURLEncoding.EncodeToString(digest[:])}})
			token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".dGVzdA"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "DPoP", "expires_in": 300})
			return
		}
		if !websocket.IsWebSocketUpgrade(r) {
			// Background catalog/quota refreshes are unrelated to this transcript.
			http.Error(w, "fixture has no catalog", http.StatusServiceUnavailable)
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer close(upstreamDone)
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(transcript)); err != nil {
			t.Error(err)
			return
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	a := newLifecycleApplication(t, func(cfg *config.Config) {
		cfg.Provider.Console.BaseURL = upstream.URL
		cfg.Provider.Build.BaseURL = upstream.URL + "/v1"
		cfg.Provider.Build.FallbackBaseURL = "disabled"
		cfg.Provider.Web.BaseURL = upstream.URL
	})
	cancel, runDone := startLifecycleApplication(t, a)
	ready := lifecycleGET(t, a)
	_ = ready.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !a.startup.acceptsTraffic() {
		if time.Now().After(deadline) {
			t.Fatal("application was not ready")
		}
		time.Sleep(time.Millisecond)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("synthetic-sso")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := a.accountRepo.UpsertByIdentity(context.Background(), account.Credential{Provider: account.ProviderConsole, Name: "shutdown-voice", SourceKey: "shutdown-voice", AuthType: account.AuthTypeSSO, EncryptedAccessToken: token, Enabled: true, AuthStatus: account.AuthStatusActive, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", WebTier: account.WebTierBasic, MaxConcurrent: 1, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Capabilities(context.Background(), a.modelRepo, a.accountRepo, credential.ID, []string{"grok-stt"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	created, err := a.clientKeys.Create(context.Background(), clientkeyapp.CreateInput{Name: "shutdown-voice", Enabled: true, BillingLimitUSDTicks: 100_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	conn, response, err := websocket.DefaultDialer.Dial("ws://"+a.server.Addr+"/v1/stt?model=Console%2Fgrok-stt", http.Header{"Authorization": []string{"Bearer " + created.Secret}})
	if err != nil {
		t.Fatalf("voice dial: %v response=%v", err, response)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("synthetic audio")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, body, err := conn.ReadMessage(); err != nil || string(body) != transcript {
		t.Fatalf("transcript=%q err=%v", body, err)
	}
	cancel()
	awaitRun(t, runDone)
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("upstream WS leaked after Run")
	}
	rows, total, err := relational.NewAuditRepository(a.database).List(context.Background(), 0, 10)
	if err != nil || total != 1 {
		t.Fatalf("final voice audit: total=%d err=%v", total, err)
	}
	record := rows[0]
	if record.GenerationOutcome != "completed" || record.DeliveryOutcome != "canceled" || record.AudioDurationMS != 3450 || record.PhysicalReceipt != "committed" || record.LedgerOutcome != "committed" || record.EstimatedCostInUSDTicks <= 0 {
		t.Fatalf("lost voice completion facts: %+v", record)
	}
	key, err := a.clientKeys.Get(context.Background(), created.Key.ID)
	if err != nil || key.BilledUsageUSDTicks != record.EstimatedCostInUSDTicks || key.ReservedUsageUSDTicks != 0 {
		t.Fatalf("voice settlement: %+v %v", key, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}
