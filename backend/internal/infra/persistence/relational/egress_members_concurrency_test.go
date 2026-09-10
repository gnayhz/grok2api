package relational

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestPoolMemberReplacementPreservesConcurrentPriority(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			repo, other := NewEgressRepository(db), NewEgressRepository(peer)
			pool, err := repo.CreateEgressPool(ctx, egress.Pool{Name: "priority-pool", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "priority-node", Enabled: true, EncryptedProxyURL: "opaque-proxy", Health: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{node.ID}); err != nil {
				t.Fatal(err)
			}
			if err := repo.SetEgressPoolMemberPriority(ctx, pool.ID, node.ID, 7); err != nil {
				t.Fatal(err)
			}
			entered, released := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(released) }) }
			defer release()
			var held atomic.Bool
			if err := db.db.Callback().Query().After("gorm:query").Register("g82_member_read", func(tx *gorm.DB) {
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
			defer db.db.Callback().Query().Remove("g82_member_read")
			replaced, prioritized := make(chan error, 1), make(chan error, 1)
			go func() { replaced <- repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{node.ID}) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			go func() { prioritized <- other.SetEgressPoolMemberPriority(ctx, pool.ID, node.ID, 5) }()
			priorityFinished := false
			select {
			case err := <-prioritized:
				priorityFinished = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(100 * time.Millisecond):
			}
			release()
			if err := <-replaced; err != nil {
				t.Fatal(err)
			}
			if !priorityFinished {
				if err := <-prioritized; err != nil {
					t.Fatal(err)
				}
			}
			var member egressPoolMemberModel
			if err := peer.db.First(&member, "pool_id = ? AND node_id = ?", pool.ID, node.ID).Error; err != nil || member.Priority != 5 {
				t.Fatalf("replacement overwrote concurrent priority: %+v %v", member, err)
			}
		})
	}
}

func TestPoolDeletionSerializesFirstRoutingPublication(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, parent := range []string{"pool", "node"} {
			t.Run(dialect+"/"+parent, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				repo, other := NewEgressRepository(db), NewEgressRepository(peer)
				pool, err := repo.CreateEgressPool(ctx, egress.Pool{Name: "first-routing-pool", Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "first-routing-node", Enabled: true, EncryptedProxyURL: "opaque-proxy", Health: 1})
				if err != nil {
					t.Fatal(err)
				}
				var count int64
				if err := db.db.Model(&egressOperationsConfigModel{}).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("fixture already published routing: %d %v", count, err)
				}
				entered, released := make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(released) }) }
				defer release()
				var held atomic.Bool
				if err := db.db.Callback().Delete().Before("gorm:delete").Register("g82_delete_after_routing", func(tx *gorm.DB) {
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
				defer db.db.Callback().Delete().Remove("g82_delete_after_routing")
				deleted, published := make(chan error, 1), make(chan error, 1)
				go func() {
					if parent == "pool" {
						deleted <- repo.DeleteEgressPool(ctx, pool.ID)
					} else {
						deleted <- repo.DeleteEgressNode(ctx, node.ID)
					}
				}()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				config := egress.DefaultOperationsConfig()
				config.DefaultTarget = egress.RoutingTarget{Mode: egress.RoutingTargetPool, PoolID: pool.ID}
				if parent == "node" {
					config.DefaultTarget = egress.RoutingTarget{Mode: egress.RoutingTargetNode, NodeID: node.ID}
				}
				go func() {
					_, err := other.SaveEgressOperationsConfig(ctx, config, func(egress.Node) error {
						return nil
					})
					published <- err
				}()
				finished := false
				var publishErr error
				select {
				case publishErr = <-published:
					finished = true
				case <-time.After(100 * time.Millisecond):
				}
				release()
				if err := <-deleted; err != nil {
					t.Fatal(err)
				}
				if !finished {
					publishErr = <-published
				}
				if publishErr != nil && publishErr != repository.ErrEgressRoutingInvalid && publishErr != repository.ErrEgressRoutingNodeInUse {
					t.Fatalf("unexpected publication failure: %v", publishErr)
				}
				stored, err := other.GetEgressOperationsConfig(ctx)
				if err != nil || stored.DefaultTarget.PoolID != 0 || stored.DefaultTarget.NodeID != 0 {
					t.Fatalf("deleted parent remains first routing target: %+v %v", stored, err)
				}
			})
		}
	}
}
