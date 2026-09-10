package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// Explicitly opted in with a snapshot and account ID. Sends two independent,
// harmless text requests; never retries a compressed POST automatically.
func runLiveCompressionProbe(t *testing.T, application *Application, cfg config.Config, accountID uint64) {
	t.Helper()
	credential, err := application.accountRepo.Get(t.Context(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewVersionedCipher(cfg.Secrets.CredentialEncryptionKey, cfg.Secrets.LegacyEncryptionKeys)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := application.egress.AcquireCredential(t.Context(), domainegress.ScopeBuild, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	raw, _ := json.Marshal(map[string]any{"model": "grok-4.6", "input": "Reply exactly COMPRESSION_OK.", "stream": false, "max_output_tokens": 2048})
	for _, compressed := range []bool{false, true} {
		payload := raw
		if compressed {
			var b bytes.Buffer
			z := gzip.NewWriter(&b)
			_, _ = z.Write(raw)
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			payload = b.Bytes()
		}
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.Provider.Build.BaseURL, "/")+"/responses", bytes.NewReader(payload))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		for k, v := range map[string]string{"Authorization": "Bearer " + token, "X-XAI-Token-Auth": cfg.Provider.Build.TokenAuth, "x-grok-client-version": cfg.Provider.Build.ClientVersion, "x-grok-client-identifier": cfg.Provider.Build.ClientIdentifier, "x-grok-client-mode": "headless", "User-Agent": cfg.Provider.Build.UserAgent, "Content-Type": "application/json", "Accept": "application/json", "x-grok-model-override": "grok-4.6", "x-userid": credential.UserID} {
			req.Header.Set(k, v)
		}
		if compressed {
			req.Header.Set("Content-Encoding", "gzip")
		}
		started := time.Now()
		response, err := lease.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("gzip=%v request_bytes=%d status=%d elapsed_ms=%d expected_answer=%v", compressed, len(payload), response.StatusCode, time.Since(started).Milliseconds(), bytes.Contains(data, []byte("COMPRESSION_OK")))
		if !compressed && (response.StatusCode != 200 || !bytes.Contains(data, []byte("COMPRESSION_OK"))) {
			t.Fatal("identity baseline failed; compression comparison invalid")
		}
	}
}
