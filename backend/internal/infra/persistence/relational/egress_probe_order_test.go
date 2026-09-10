package relational

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestProbeVersionsFenceConcurrentAndOlderResults(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			path := t.TempDir() + "/probe.db"
			open := func() *Database {
				var db *Database
				var err error
				if driver == "sqlite" {
					db, err = OpenSQLite(ctx, path)
				} else {
					dsn := os.Getenv("TEST_POSTGRES_DSN")
					if dsn == "" {
						t.Skip("requires isolated TEST_POSTGRES_DSN")
					}
					db, err = OpenPostgres(ctx, dsn, 8, 2)
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				return db
			}
			db := open()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			a, b := NewEgressRepository(db), NewEgressRepository(open())
			node, err := a.CreateEgressNode(ctx, egress.Node{Name: "probe-order", Enabled: true, EncryptedProxyURL: "fixed-proxy"})
			if err != nil {
				t.Fatal(err)
			}
			const count = 12
			versions := make(chan uint64, count)
			var wg sync.WaitGroup
			for i := 0; i < count; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					r := a
					if i%2 != 0 {
						r = b
					}
					v, err := r.BeginEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL)
					if err != nil {
						t.Error(err)
						return
					}
					versions <- v
				}(i)
			}
			wg.Wait()
			close(versions)
			seen := map[uint64]bool{}
			for v := range versions {
				if seen[v] || v == 0 {
					t.Fatalf("duplicate version %d", v)
				}
				seen[v] = true
			}
			if len(seen) != count || !seen[count] {
				t.Fatalf("versions=%v", seen)
			}
			newer := egress.ProbeResult{Revision: count, Status: egress.ProbeStatusHealthy, ExitIP: "192.0.2.2", TestedAt: time.Now().UTC()}
			if err := b.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, newer); err != nil {
				t.Fatal(err)
			}
			older := egress.ProbeResult{Revision: count - 1, Status: egress.ProbeStatusHealthy, ExitIP: "192.0.2.1", TestedAt: newer.TestedAt.Add(time.Hour)}
			if err := a.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, older); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("older/future-clock result accepted: %v", err)
			}
			older.Revision = 0
			if err := a.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, older); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("unversioned result accepted: %v", err)
			}
			got, err := a.GetEgressNode(ctx, node.ID)
			if err != nil || got.ExitIP != newer.ExitIP {
				t.Fatalf("new observation overwritten: %+v %v", got, err)
			}
			// A restarted writer continues the persistent sequence; a slow machine's
			// wall clock is allowed to move backwards without blocking fresh probes.
			restarted := NewEgressRepository(open())
			v, err := restarted.BeginEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL)
			if err != nil || v != count+1 {
				t.Fatalf("restart sequence: %d %v", v, err)
			}
			newer.Revision = v
			newer.ExitIP = "192.0.2.3"
			newer.TestedAt = newer.TestedAt.Add(-time.Hour)
			if err := restarted.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, newer); err != nil {
				t.Fatal(err)
			}
			got, err = a.GetEgressNode(ctx, node.ID)
			if err != nil || got.ExitIP != newer.ExitIP {
				t.Fatalf("fresh result lost: %+v %v", got, err)
			}
		})
	}
}
