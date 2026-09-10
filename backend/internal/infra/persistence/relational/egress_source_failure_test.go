package relational

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"gorm.io/gorm"
	"reflect"
	"testing"
	"time"
)

func TestSourceSyncFailurePreservesCurrentStateAndReleasesOwnership(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"begin", "commit", "failure"} {
			for _, fault := range []string{"rollback", "cancel_wait"} {
				t.Run(dialect+"/"+operation+"/"+fault, func(t *testing.T) {
					db, peer := settingsDatabasePair(t, dialect)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					repo := NewEgressRepository(db)
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
					source, err := repo.CreateEgressSource(ctx, egress.SubscriptionSource{Name: "failure-source", Enabled: true, EncryptedURL: "cipher", RefreshIntervalSeconds: 900})
					if err != nil {
						t.Fatal(err)
					}
					claim, err := repo.BeginEgressSourceSync(ctx, source)
					if err != nil {
						t.Fatal(err)
					}
					before := sourceSyncSnapshot(t, peer)
					write := func(writeCtx context.Context) error {
						now := time.Now().UTC()
						switch operation {
						case "begin":
							_, err := repo.BeginEgressSourceSync(writeCtx, source)
							return err
						case "failure":
							return repo.FailEgressSourceSync(writeCtx, claim, now, now.Add(time.Minute), "fetch failed")
						default:
							_, err := repo.CommitEgressSourceSync(writeCtx, claim, []egress.Node{{Name: "imported", SourceID: source.ID, SourceKey: "imported", Enabled: true, EncryptedProxyURL: "new-cipher", Health: 1}}, now, now.Add(time.Minute))
							return err
						}
					}
					if fault == "rollback" {
						injected := errors.New("injected fixed routing write failure")
						hook := func(tx *gorm.DB) {
							if tx.Statement.Table == "egress_subscription_sources" {
								tx.AddError(injected)
							}
						}
						err = db.db.Callback().Update().After("gorm:update").Register("g84_source_failure", hook)
						remove := func() { _ = db.db.Callback().Update().Remove("g84_source_failure") }
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
							if _, err := lockEgressSource(tx, source.ID); err != nil {
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
							waitMediaDeletionLock(t, ctx, peer, "egress_subscription_sources", done)
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
					if after := sourceSyncSnapshot(t, peer); !reflect.DeepEqual(before, after) {
						t.Fatalf("failed source write changed state: before=%+v after=%+v", before, after)
					}
					if err := write(ctx); err != nil {
						t.Fatalf("retry did not regain writer ownership: %v", err)
					}
				})
			}
		}
	}
}
