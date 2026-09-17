package relational

import (
	"context"
	"errors"
	"fmt"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"strings"
	"sync"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func seedKeyConsistency(t *testing.T, db *Database) clientkey.Key {
	t.Helper()
	route, err := NewModelRepository(db).Create(context.Background(), model.Route{PublicID: "allowed", Provider: account.ProviderBuild, UpstreamModel: "grok-4.3", Capability: model.CapabilityResponses, Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewClientKeyRepository(db).Create(context.Background(), clientkey.Key{Name: "original", Prefix: "consistency", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, AllowedModels: []uint64{route.ID}})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

type keyReadGate struct {
	repository.ClientKeyRepository
	read   chan struct{}
	resume chan struct{}
}

func (g *keyReadGate) Get(ctx context.Context, id uint64) (clientkey.Key, error) {
	key, err := g.ClientKeyRepository.Get(ctx, id)
	close(g.read)
	select {
	case <-g.resume:
	case <-ctx.Done():
		return clientkey.Key{}, ctx.Err()
	}
	return key, err
}

func TestClientKeyPartialManagementPreservesConcurrentDisable(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			key := seedKeyConsistency(t, a)
			gate := &keyReadGate{ClientKeyRepository: NewClientKeyRepository(a), read: make(chan struct{}), resume: make(chan struct{})}
			service := clientkeyapp.NewService("fixture", gate, nil, nil, 0, 0, nil, security.RandomTokenSource{})
			defer service.Close(context.Background())
			done := make(chan error, 1)
			go func() {
				name := "renamed"
				_, err := service.Update(ctx, key.ID, clientkeyapp.UpdateInput{Name: &name})
				done <- err
			}()
			select {
			case <-gate.read:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			_, err := NewClientKeyRepository(b).UpdateManyEnabled(ctx, []uint64{key.ID}, false)
			close(gate.resume)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			actual, err := NewClientKeyRepository(b).Get(ctx, key.ID)
			if err != nil {
				t.Fatal(err)
			}
			if actual.Enabled || actual.Name != "renamed" {
				t.Fatalf("name-only patch overwrote committed disable: enabled=%v name=%q", actual.Enabled, actual.Name)
			}
		})
	}
}

func TestClientKeyReadersKeepOnePermissionSnapshot(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, readKind := range []string{"get", "prefix", "list"} {
			t.Run(dialect+"/"+readKind, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				key := seedKeyConsistency(t, a)
				read, resume := make(chan struct{}), make(chan struct{})
				var once sync.Once
				callback := "key_snapshot_gate"
				if err := a.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "client_keys" && tx.Error == nil && !strings.Contains(tx.Statement.SQL.String(), "count(") {
						once.Do(func() {
							close(read)
							select {
							case <-resume:
							case <-ctx.Done():
							}
						})
					}
				}); err != nil {
					t.Fatal(err)
				}
				defer a.db.Callback().Query().Remove(callback)
				type result struct {
					key clientkey.Key
					err error
				}
				done := make(chan result, 1)
				go func() {
					repo := NewClientKeyRepository(a)
					var value clientkey.Key
					var err error
					switch readKind {
					case "get":
						value, err = repo.Get(ctx, key.ID)
					case "prefix":
						value, err = repo.GetByPrefix(ctx, key.Prefix)
					case "list":
						var values []clientkey.Key
						values, _, err = repo.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 10}})
						if err == nil && len(values) != 1 {
							err = fmt.Errorf("list count %d", len(values))
						}
						if len(values) == 1 {
							value = values[0]
						}
					}
					done <- result{value, err}
				}()
				select {
				case <-read:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				// One committed change disables the key and clears the model relation.
				key.Enabled, key.AllowedModels = false, nil
				_, err := NewClientKeyRepository(b).Patch(ctx, key.ID, clientkey.ManagementPatch{Enabled: &key.Enabled, AllowedModels: &key.AllowedModels})
				close(resume)
				if err != nil {
					t.Fatal(err)
				}
				actual := <-done
				if actual.err != nil {
					t.Fatal(actual.err)
				}
				if actual.key.Enabled && len(actual.key.AllowedModels) == 0 {
					t.Fatalf("read combined old enabled=true with new empty permissions; no committed state has this pair")
				}
			})
		}
	}
}

func TestClientKeyRenameDoesNotRestoreConcurrentlyDeletedGrant(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			key := seedKeyConsistency(t, a)
			gate := &keyReadGate{ClientKeyRepository: NewClientKeyRepository(a), read: make(chan struct{}), resume: make(chan struct{})}
			service := clientkeyapp.NewService("fixture", gate, nil, nil, 0, 0, nil, security.RandomTokenSource{})
			defer service.Close(context.Background())
			done := make(chan error, 1)
			go func() {
				name := "renamed"
				_, err := service.Update(ctx, key.ID, clientkeyapp.UpdateInput{Name: &name})
				done <- err
			}()
			select {
			case <-gate.read:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			err := NewModelRepository(b).Delete(ctx, key.AllowedModels[0])
			close(gate.resume)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			actual, err := NewClientKeyRepository(b).Get(ctx, key.ID)
			if err != nil || actual.Name != "renamed" || actual.ModelScope != clientkey.ModelScopeRestricted || len(actual.AllowedModels) != 0 || actual.AllowsModel(999) {
				t.Fatalf("rename restored deleted grant: %+v %v", actual, err)
			}
		})
	}
}

func TestClientKeyGrantRacingModelDeleteRollsBack(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			key := seedKeyConsistency(t, a)
			deleted, resume := make(chan struct{}), make(chan struct{})
			callback := "g14_model_delete_gate"
			if err := a.db.Callback().Delete().After("gorm:delete").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "model_routes" && tx.Error == nil {
					close(deleted)
					select {
					case <-resume:
					case <-ctx.Done():
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer a.db.Callback().Delete().Remove(callback)
			deleteDone := make(chan error, 1)
			go func() { deleteDone <- NewModelRepository(a).Delete(ctx, key.AllowedModels[0]) }()
			select {
			case <-deleted:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			entered := make(chan struct{})
			if err := b.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "client_keys" {
					close(entered)
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer b.db.Callback().Update().Remove(callback)
			patchDone := make(chan error, 1)
			go func() {
				name := "must rollback"
				_, err := NewClientKeyRepository(b).Patch(ctx, key.ID, clientkey.ManagementPatch{Name: &name, AllowedModels: &key.AllowedModels})
				patchDone <- err
			}()
			if dialect == "sqlite" {
				// A writing SQLite transaction waits in BEGIN IMMEDIATE, before
				// GORM reaches the Update callback. Observe its occupied connection.
				pool, err := b.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				for pool.Stats().InUse == 0 {
					select {
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(time.Millisecond):
					}
				}
			} else {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			close(resume)
			if err := <-deleteDone; err != nil {
				t.Fatal(err)
			}
			if err := <-patchDone; !errors.Is(err, repository.ErrInvalidRecord) {
				t.Fatalf("racing grant: %v", err)
			}
			actual, err := NewClientKeyRepository(b).Get(ctx, key.ID)
			if err != nil || actual.Name != key.Name || actual.ModelScope != clientkey.ModelScopeRestricted || len(actual.AllowedModels) != 0 {
				t.Fatalf("failed grant partially committed: %+v %v", actual, err)
			}
		})
	}
}

func TestClientKeySnapshotFailureDoesNotReturnPartialIdentity(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []string{"get", "prefix", "list"} {
			for _, failureKind := range []string{"query", "cancel"} {
				t.Run(dialect+"/"+kind+"/"+failureKind, func(t *testing.T) {
					a, _ := settingsDatabasePair(t, dialect)
					key := seedKeyConsistency(t, a)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					failure := errors.New("model permissions unavailable")
					callback := "g14_permission_read_fail"
					if err := a.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
						if tx.Statement.Table == "client_key_models" {
							if failureKind == "cancel" {
								cancel()
							} else {
								_ = tx.AddError(failure)
							}
						}
					}); err != nil {
						t.Fatal(err)
					}
					keys := NewClientKeyRepository(a)
					var value clientkey.Key
					var err error
					switch kind {
					case "get":
						value, err = keys.Get(ctx, key.ID)
					case "prefix":
						value, err = keys.GetByPrefix(ctx, key.Prefix)
					case "list":
						var values []clientkey.Key
						values, _, err = keys.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 10}})
						if len(values) > 0 {
							t.Errorf("partial list returned on failure: %+v", values)
						}
					}
					_ = a.db.Callback().Query().Remove(callback)
					want := failure
					if failureKind == "cancel" {
						want = context.Canceled
					}
					if !errors.Is(err, want) || value.ID != 0 || value.AllowsModel(key.AllowedModels[0]) {
						t.Fatalf("partial identity returned: %+v err=%v", value, err)
					}
					// A new management operation on the same pool proves rollback released
					// the snapshot rather than leaving an occupied transaction behind.
					next, finish := context.WithTimeout(context.Background(), 5*time.Second)
					defer finish()
					name := "after failure"
					updated, err := keys.Patch(next, key.ID, clientkey.ManagementPatch{Name: &name})
					if err != nil || updated.Name != name || !updated.AllowsModel(key.AllowedModels[0]) {
						t.Fatalf("snapshot failed to release: %+v %v", updated, err)
					}
				})
			}
		}
	}
}
