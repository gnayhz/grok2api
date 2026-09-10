package relational

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"reflect"
	"testing"
	"time"
)

func TestFixedRoutingWriteFailurePreservesCurrentStateAndReleasesOwnership(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"node", "save", "cas"} {
			for _, fault := range []string{"rollback", "cancel_wait"} {
				t.Run(dialect+"/"+operation+"/"+fault, func(t *testing.T) {
					db, peer := settingsDatabasePair(t, dialect)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					repo, reader := NewEgressRepository(db), NewEgressRepository(peer)
					second, err := repo.CreateEgressPool(ctx, egress.Pool{Name: "second", Enabled: true})
					if err != nil {
						t.Fatal(err)
					}
					_, err = repo.CreateEgressPool(ctx, egress.Pool{Name: "first", Enabled: true, FallbackMode: egress.PoolFallbackPool, FallbackPoolID: second.ID})
					if err != nil {
						t.Fatal(err)
					}
					node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "member", Enabled: true, EncryptedProxyURL: "opaque-test-proxy", Health: 1})
					if err != nil {
						t.Fatal(err)
					}
					if err := repo.SetEgressPoolMembers(ctx, second.ID, []uint64{node.ID}); err != nil {
						t.Fatal(err)
					}
					config := egress.DefaultOperationsConfig()
					config.DefaultTarget = egress.RoutingTarget{Mode: egress.RoutingTargetPool, PoolID: second.ID}
					if _, err := repo.SaveEgressOperationsConfig(ctx, config, func(egress.Node) error {
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if err := repo.SetEgressPoolMemberPriority(ctx, second.ID, node.ID, 7); err != nil {
						t.Fatal(err)
					}
					before := readPoolGraphSnapshot(t, ctx, reader)
					var membersBefore []egressPoolMemberModel
					if err := peer.db.Order("pool_id, node_id").Find(&membersBefore).Error; err != nil {
						t.Fatal(err)
					}
					var nodesBefore []egressNodeModel
					if err := peer.db.Order("id").Find(&nodesBefore).Error; err != nil {
						t.Fatal(err)
					}
					validate := func(n egress.Node) error {
						if !egress.CanNodeServeFixedTarget(n) {
							return repository.ErrEgressRoutingNodeInUse
						}
						return nil
					}
					write := func(writeCtx context.Context) error {
						if operation == "node" {
							changed := node
							changed.Name = "renamed"
							_, err := repo.UpdateEgressNodeConfiguration(writeCtx, changed, validate)
							return err
						}
						changed := before.config
						changed.DefaultTarget = egress.RoutingTarget{Mode: egress.RoutingTargetNode, NodeID: node.ID}
						changed.UpdatedAt = time.Now().UTC()
						if operation == "save" {
							_, err := repo.SaveEgressOperationsConfig(writeCtx, changed, validate)
							return err
						}
						_, err := repo.SaveEgressOperationsConfigIfCurrent(writeCtx, changed, before.config.UpdatedAt, validate)
						return err
					}
					if fault == "rollback" {
						injected := errors.New("injected fixed routing write failure")
						hook := func(tx *gorm.DB) {
							if (operation == "node" && tx.Statement.Table == "egress_nodes") || (operation != "node" && tx.Statement.Table == "egress_operations_config") {
								tx.AddError(injected)
							}
						}
						var remove func()
						if operation != "node" {
							err = db.db.Callback().Create().After("gorm:create").Register("g83_routing_failure", hook)
							remove = func() { _ = db.db.Callback().Create().Remove("g83_routing_failure") }
						} else {
							err = db.db.Callback().Update().After("gorm:update").Register("g83_routing_failure", hook)
							remove = func() { _ = db.db.Callback().Update().Remove("g83_routing_failure") }
						}
						if err != nil {
							t.Fatal(err)
						}
						defer remove()
						if err := write(ctx); !errors.Is(err, injected) {
							t.Fatalf("write failure = %v", err)
						}
						remove()
					} else {
						pool, err := db.db.DB()
						if err != nil {
							t.Fatal(err)
						}
						waits := pool.Stats().WaitCount
						var release func()
						if dialect == "sqlite" {
							pool.SetMaxOpenConns(1)
							conn, err := pool.Conn(ctx)
							if err != nil {
								t.Fatal(err)
							}
							release = func() { _ = conn.Close() }
						} else {
							tx := peer.db.WithContext(ctx).Begin()
							if tx.Error != nil {
								t.Fatal(tx.Error)
							}
							if _, err := lockEgressOperationsConfig(tx); err != nil {
								_ = tx.Rollback()
								t.Fatal(err)
							}
							release = func() { _ = tx.Rollback() }
						}
						defer release()
						writeCtx, cancelWrite := context.WithCancel(ctx)
						defer cancelWrite()
						done := make(chan error, 1)
						go func() { done <- write(writeCtx) }()
						if dialect == "postgres" {
							waitMediaDeletionLock(t, ctx, peer, "egress_operations_config", done)
						} else {
							for pool.Stats().WaitCount == waits {
								select {
								case err := <-done:
									t.Fatalf("writer did not wait: %v", err)
								case <-ctx.Done():
									t.Fatal(ctx.Err())
								case <-time.After(time.Millisecond):
								}
							}
						}
						cancelWrite()
						select {
						case err := <-done:
							if !errors.Is(err, context.Canceled) {
								t.Fatalf("canceled writer = %v", err)
							}
						case <-ctx.Done():
							t.Fatal("canceled writer did not finish")
						}
						release()
					}
					if after := readPoolGraphSnapshot(t, ctx, reader); !reflect.DeepEqual(before, after) {
						t.Fatalf("failed write changed references: before=%+v after=%+v", before, after)
					}
					var membersAfter []egressPoolMemberModel
					if err := peer.db.Order("pool_id, node_id").Find(&membersAfter).Error; err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(membersBefore, membersAfter) {
						t.Fatalf("failed write changed priorities: %+v -> %+v", membersBefore, membersAfter)
					}
					var nodesAfter []egressNodeModel
					if err := peer.db.Order("id").Find(&nodesAfter).Error; err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(nodesBefore, nodesAfter) {
						t.Fatalf("failed write changed node state: before=%+v after=%+v", nodesBefore, nodesAfter)
					}
					if err := write(ctx); err != nil {
						t.Fatalf("retry did not regain writer ownership: %v", err)
					}
				})
			}
		}
	}
}
