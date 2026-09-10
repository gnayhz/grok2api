package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func BenchmarkStatsigQuotaRefresh(b *testing.B) {
	for _, cold := range []bool{false, true} {
		name := "cached"
		if cold {
			name = "refresh"
		}
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "quota.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				b.Fatal(err)
			}
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				b.Fatal(err)
			}
			token, err := cipher.Encrypt("synthetic-quota-credential")
			if err != nil {
				b.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			value, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, AuthStatus: account.AuthStatusActive, Enabled: true, SourceKey: "benchmark", Name: "benchmark", WebTier: account.WebTierBasic, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", Email: "synthetic@example.test", EncryptedAccessToken: token})
			if err != nil {
				b.Fatal(err)
			}
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/index" {
					_, _ = io.WriteString(w, `<meta name="grok-site-verification" content="synthetic-meta">`)
					return
				}
				switch r.URL.Path {
				case "/rest/rate-limits":
					var input struct {
						ModelName string `json:"modelName"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						http.Error(w, "invalid body", 400)
						return
					}
					total := map[string]int{"auto": 7, "fast": 30}[input.ModelName]
					_ = json.NewEncoder(w).Encode(map[string]int{"totalQueries": total, "remainingQueries": 3, "windowSizeSeconds": 7200})
				case "/rest/media/imagine/quota_info":
					_, _ = io.Copy(io.Discard, r.Body)
					writeEmptyImagineQuota(w)
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			manager := infraegress.NewManager(relational.NewEgressRepository(db), cipher)
			defer manager.Close(ctx)
			var signs atomic.Int64
			signerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				signs.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]string{"x-statsig-id": base64.RawStdEncoding.EncodeToString(make([]byte, 70))})
			}))
			defer signerServer.Close()
			adapter := NewAdapter(Config{BaseURL: upstream.URL, StatsigMode: "url", StatsigSignerURL: signerServer.URL}, manager, cipher, nil, nil)
			adapter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
			adapter.statsig.client = signerServer.Client()
			adapter.statsig.validateEndpoint = func(context.Context, string) error { return nil }
			service := accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, provider.NewRegistry(adapter), cipher, nil)
			if _, err := service.RefreshQuota(ctx, value.ID); err != nil {
				b.Fatal(err)
			}
			calls.Store(0)
			signs.Store(0)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if cold {
					adapter.statsig.Clear()
				}
				windows, err := service.RefreshQuota(ctx, value.ID)
				if err != nil || len(windows) == 0 {
					b.Fatalf("full quota: windows=%v err=%v", windows, err)
				}
			}
			b.StopTimer()
			perRefresh := int64(3)
			if cold {
				perRefresh = 5
			}
			wantSigns := int64(0)
			if cold {
				wantSigns = 2 * int64(b.N)
			}
			if signs.Load() != wantSigns {
				b.Fatalf("signer requests=%d expected=%d", signs.Load(), wantSigns)
			}
			if calls.Load() != perRefresh*int64(b.N) {
				b.Fatalf("physical calls=%d expected=%d", calls.Load(), perRefresh*int64(b.N))
			}
		})
	}
}
