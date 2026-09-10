package web

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
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

func BenchmarkWebFullQuotaRefresh(b *testing.B) {
	for _, paid := range []bool{false, true} {
		name := "basic"
		if paid {
			name = "paid"
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
			weekly, err := hex.DecodeString(capturedWeeklyCreditsHex)
			if err != nil {
				b.Fatal(err)
			}
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
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
					if paid {
						total = map[string]int{"auto": 50, "fast": 140}[input.ModelName]
					}
					_ = json.NewEncoder(w).Encode(map[string]int{"totalQueries": total, "remainingQueries": 3, "windowSizeSeconds": 7200})
				case "/rest/media/imagine/quota_info":
					_, _ = io.Copy(io.Discard, r.Body)
					writeEmptyImagineQuota(w)
				case "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig":
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = w.Write(weekly)
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			manager := infraegress.NewManager(relational.NewEgressRepository(db), cipher)
			defer manager.Close(ctx)
			adapter := NewAdapter(Config{BaseURL: upstream.URL, StatsigMode: "manual", StatsigManualValue: base64.RawStdEncoding.EncodeToString(make([]byte, 70))}, manager, cipher, nil, nil)
			service := accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, provider.NewRegistry(adapter), cipher, nil)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				windows, err := service.RefreshQuota(ctx, value.ID)
				if err != nil || len(windows) == 0 {
					b.Fatalf("full quota: windows=%v err=%v", windows, err)
				}
			}
			b.StopTimer()
			perRefresh := int64(3)
			if paid {
				perRefresh = 4
			}
			if calls.Load() != perRefresh*int64(b.N) {
				b.Fatalf("physical calls=%d expected=%d", calls.Load(), perRefresh*int64(b.N))
			}
		})
	}
}
