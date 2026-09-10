package relational

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"gorm.io/gorm"
)

// The service-level regression covers deletion before the member transaction.
// This covers the inverse order on independent connections: once a membership
// writer has read its current members, every parent cleanup must wait, then
// remove the membership it just committed.
func TestPoolMemberPublicationSerializesParentCleanup(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, deletion := range []string{"pool", "node", "nodes_batch", "unhealthy_nodes"} {
			t.Run(dialect+"/"+deletion, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				repo, other := NewEgressRepository(db), NewEgressRepository(peer)
				pool, err := repo.CreateEgressPool(ctx, egress.Pool{Name: "member-cleanup-pool", Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "member-cleanup-node", Enabled: true, EncryptedProxyURL: "opaque-proxy", Health: 1})
				if err != nil {
					t.Fatal(err)
				}
				if deletion == "unhealthy_nodes" {
					sequence, err := repo.BeginEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL)
					if err != nil {
						t.Fatal(err)
					}
					now := time.Now().UTC()
					family := egress.ProbeFamilyResult{Status: egress.ProbeStatusUnhealthy, TestedAt: now, Error: "fixture transport failure"}
					if err := repo.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, egress.ProbeResult{Revision: sequence, Status: egress.ProbeStatusUnhealthy, TestedAt: now, IPv4: family, IPv6: family}); err != nil {
						t.Fatal(err)
					}
				}
				entered, released := make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(released) }) }
				defer release()
				var held atomic.Bool
				if err := db.db.Callback().Query().After("gorm:query").Register("g82_publication", func(tx *gorm.DB) {
					if tx.Statement.Table == "egress_pool_members" && held.CompareAndSwap(false, true) {
						close(entered)
						select {
						case <-released:
						case <-ctx.Done():
							tx.AddError(ctx.Err())
						}
					}
				}); err != nil {
					t.Fatal(err)
				}
				defer db.db.Callback().Query().Remove("g82_publication")
				published, deleted := make(chan error, 1), make(chan error, 1)
				go func() { published <- repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{node.ID}) }()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				go func() {
					var err error
					switch deletion {
					case "pool":
						err = other.DeleteEgressPool(ctx, pool.ID)
					case "node":
						err = other.DeleteEgressNode(ctx, node.ID)
					case "nodes_batch":
						_, err = other.DeleteEgressNodes(ctx, []uint64{node.ID})
					default:
						_, err = other.DeleteUnhealthyEgressNodes(ctx)
					}
					deleted <- err
				}()
				if dialect == "postgres" {
					waitMediaDeletionLock(t, ctx, db, "pg_advisory_xact_lock", deleted)
				} else {
					select {
					case err := <-deleted:
						t.Fatalf("cleanup completed while member snapshot held: %v", err)
					case <-time.After(100 * time.Millisecond):
					}
				}
				release()
				if err := <-published; err != nil {
					t.Fatal(err)
				}
				if err := <-deleted; err != nil {
					t.Fatal(err)
				}
				members, err := other.EgressPoolMembers(ctx)
				if err != nil || len(members[pool.ID]) != 0 {
					t.Fatalf("cleanup left published member: %v %v", members, err)
				}
			})
		}
	}
}
