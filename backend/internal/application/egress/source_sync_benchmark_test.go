package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func BenchmarkSourceSyncCommit(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"success", "failure"} {
			b.Run(dialect+"/"+operation, func(b *testing.B) {
				ctx := context.Background()
				var db *relational.Database
				var err error
				if dialect == "postgres" {
					dsn := os.Getenv("TEST_POSTGRES_DSN")
					if dsn == "" {
						b.Skip("requires isolated TEST_POSTGRES_DSN")
					}
					db, err = relational.OpenPostgres(ctx, dsn, 4, 2)
				} else {
					db, err = relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "fixed-routing-cost.db"))
				}
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
				repo := relational.NewEgressRepository(db)
				service := NewService(repo, cipher)
				defer service.Close(ctx)
				var calls atomic.Int64
				feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if operation == "failure" {
						http.Error(w, "source unavailable", 503)
						return
					}
					_, _ = w.Write([]byte("http://source-cost.example:8080\n"))
				}))
				defer feed.Close()
				name := fmt.Sprintf("source-cost-%d", time.Now().UnixNano())
				url, proxy := "http://1.1.1.1/feed", feed.URL
				source, err := service.CreateSource(ctx, SubscriptionSourceInput{Name: name, Enabled: true, URL: &url, ProxyURL: &proxy})
				if err != nil {
					b.Fatal(err)
				}
				defer service.DeleteSource(ctx, source.ID)
				if operation == "success" {
					if _, err := service.SyncSource(ctx, source.ID); err != nil {
						b.Fatal(err)
					}
				}
				initialCalls := calls.Load()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					result, err := service.SyncSource(ctx, source.ID)
					if operation == "success" {
						if err != nil || result.Imported != 0 {
							b.Fatalf("same feed: %+v %v", result, err)
						}
					} else if err == nil {
						b.Fatal("failed feed accepted")
					}
				}
				b.StopTimer()
				if calls.Load()-initialCalls != int64(b.N) {
					b.Fatal("fetch count changed")
				}
				stored, err := repo.GetEgressSource(ctx, source.ID)
				if err != nil || stored.LastSyncedAt == nil {
					b.Fatalf("source status missing: %+v %v", stored, err)
				}
				if (operation == "failure") != (stored.LastSyncError != "") {
					b.Fatalf("wrong source outcome: %+v", stored)
				}
				b.ReportMetric(1, "fetches/op")
			})
		}
	}
}
