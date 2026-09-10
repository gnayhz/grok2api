package relational

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"gorm.io/gorm"
)

type poolGraphSnapshot struct {
	pools   []egress.Pool
	members map[uint64][]uint64
	config  egress.OperationsConfig
}

func readPoolGraphSnapshot(t *testing.T, ctx context.Context, repo *EgressRepository) poolGraphSnapshot {
	t.Helper()
	pools, err := repo.ListEgressPools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	members, err := repo.EgressPoolMembers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return poolGraphSnapshot{pools: pools, members: members, config: config}
}

func TestPoolGraphWriteFailurePreservesReferencesAndReleasesOwnership(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"create", "update", "delete"} {
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
					first, err := repo.CreateEgressPool(ctx, egress.Pool{Name: "first", Enabled: true, FallbackMode: egress.PoolFallbackPool, FallbackPoolID: second.ID})
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
					before := readPoolGraphSnapshot(t, ctx, reader)
					write := func(writeCtx context.Context) error {
						switch operation {
						case "create":
							_, err := repo.CreateEgressPool(writeCtx, egress.Pool{Name: "new", Enabled: true, FallbackMode: egress.PoolFallbackPool, FallbackPoolID: first.ID})
							return err
						case "update":
							changed := first
							changed.FallbackMode, changed.FallbackPoolID = egress.PoolFallbackNone, 0
							_, err := repo.UpdateEgressPool(writeCtx, changed)
							return err
						default:
							return repo.DeleteEgressPool(writeCtx, second.ID)
						}
					}
					if fault == "rollback" {
						injected := errors.New("injected graph write failure")
						hook := func(tx *gorm.DB) {
							if tx.Statement.Table == "egress_pools" {
								tx.AddError(injected)
							}
						}
						var remove func()
						switch operation {
						case "create":
							err = db.db.Callback().Create().After("gorm:create").Register("g81_graph_failure", hook)
							remove = func() { _ = db.db.Callback().Create().Remove("g81_graph_failure") }
						case "update":
							err = db.db.Callback().Update().After("gorm:update").Register("g81_graph_failure", hook)
							remove = func() { _ = db.db.Callback().Update().Remove("g81_graph_failure") }
						default:
							err = db.db.Callback().Delete().After("gorm:delete").Register("g81_graph_failure", hook)
							remove = func() { _ = db.db.Callback().Delete().Remove("g81_graph_failure") }
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
							if err := lockEgressPoolGraph(tx); err != nil {
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
							waitMediaDeletionLock(t, ctx, peer, "pg_advisory_xact_lock", done)
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
					if err := write(ctx); err != nil {
						t.Fatalf("retry did not regain writer ownership: %v", err)
					}
				})
			}
		}
	}
}
