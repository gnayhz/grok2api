package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// The identical benchmark runs before and after the detection contract change.
// It includes the actual batch, account/billing reads, decryption, CLI wire,
// local HTTP transfer and response inspection; setup is outside measurement.
func BenchmarkBuildDetectionCost(b *testing.B) {
	for _, scenario := range []struct {
		name                           string
		outputBytes, status, succeeded int
	}{
		{name: "output_2", outputBytes: 2, status: 200, succeeded: 1},
		{name: "output_32768", outputBytes: 32 << 10, status: 200, succeeded: 1},
		{name: "credential_refused", status: 401},
	} {
		b.Run(fmt.Sprintf("sqlite/%s", scenario.name), func(b *testing.B) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "detect-cost.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				b.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				b.Fatal(err)
			}
			encrypted, err := cipher.Encrypt("synthetic")
			if err != nil {
				b.Fatal(err)
			}
			value, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
				Name: "cost", SourceKey: "cost", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), RefreshPermanent: true, AuthStatus: account.AuthStatusActive})
			if err != nil {
				b.Fatal(err)
			}
			body := `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"` + strings.Repeat("a", scenario.outputBytes) + `"}]}]}`
			if scenario.status == 401 {
				body = `{"error":{"code":"invalid_api_key"}}`
			}
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(scenario.status)
				_, _ = io.WriteString(w, body)
			}))
			b.Cleanup(server.Close)
			adapter := NewAdapter(Config{BaseURL: server.URL}, cipher)
			adapter.http = server.Client()
			service := accountapp.NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
			service.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
			ids := []uint64{value.ID}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				succeeded, failed, err := service.DetectBuildAccountsWithProgress(ctx, ids, false, nil, nil)
				if err != nil || succeeded != scenario.succeeded || failed != 1-scenario.succeeded {
					b.Fatalf("succeeded=%d failed=%d err=%v", succeeded, failed, err)
				}
			}
			if calls.Load() != int64(b.N) {
				b.Fatalf("upstream calls=%d operations=%d", calls.Load(), b.N)
			}
		})
	}
}
