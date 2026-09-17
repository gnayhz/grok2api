package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

func BenchmarkProviderDurationQuota(b *testing.B) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "quota.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		b.Fatal(err)
	}
	token, err := cipher.Encrypt("synthetic-duration-credential")
	if err != nil {
		b.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"remainingQueries":7,"totalQueries":10,"windowSizeSeconds":3600}`)
	}))
	defer server.Close()
	var cfg config.Config
	cfg.Provider.Web.BaseURL = server.URL
	cfg.Provider.Web.QuotaTimeout = config.Duration(30 * time.Second)
	cfg.Provider.Web.StatsigMode, cfg.Provider.Web.StatsigManualValue = "manual", "synthetic-signature"
	manager := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
	defer manager.Close(ctx)
	adapter := webprovider.NewAdapter(webProviderConfig(cfg), manager, cipher, nil, nil)
	credential := account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: token}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got, err := adapter.SyncQuotaMode(ctx, credential, "auto")
		if err != nil || got.Remaining != 7 {
			b.Fatalf("quota response: remaining=%d err=%v", got.Remaining, err)
		}
	}
	b.StopTimer()
	if calls.Load() != int64(b.N) {
		b.Fatalf("physical calls=%d iterations=%d", calls.Load(), b.N)
	}
}
