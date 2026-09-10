package account

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type tombstoneCurrentRepository struct {
	repository.AccountRepository
	afterDelete func()
	afterRead   func()
}

func (r *tombstoneCurrentRepository) DeleteManyWithLinked(ctx context.Context, p accountdomain.Provider, ids []uint64, targets []accountdomain.Provider, skip bool) (repository.LinkedDeleteOutcome, error) {
	out, err := r.AccountRepository.DeleteManyWithLinked(ctx, p, ids, targets, skip)
	if err == nil && r.afterDelete != nil {
		r.afterDelete()
	}
	return out, err
}

func (r *tombstoneCurrentRepository) TombstonedEmails(ctx context.Context, emails []string) (map[string]struct{}, error) {
	out, err := r.AccountRepository.TombstonedEmails(ctx, emails)
	if err == nil && r.afterRead != nil {
		fn := r.afterRead
		r.afterRead = nil
		fn()
	}
	return out, err
}

func TestCurrentDeletionPreventsResurrection(t *testing.T) {
	for _, timing := range []string{"cancel_after_delete_commit", "delete_after_import_prefilter"} {
		t.Run(timing, func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "tombstone.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			port := &tombstoneCurrentRepository{AccountRepository: repo}
			service := NewService(port, nil, nil, nil, provider.NewRegistry(tombstoneImportAdapter{email: "g20@example.test"}), cipher, nil)
			first, err := service.ImportCredentials(ctx, []byte("doc"))
			if err != nil || first.Created != 1 {
				t.Fatalf("first import: %+v, %v", first, err)
			}
			if timing == "cancel_after_delete_commit" {
				deleteCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				port.afterDelete = cancel
				deleted, err := service.BatchDelete(deleteCtx, first.AccountIDs)
				if err != nil || deleted != 1 {
					t.Fatalf("delete: %d, %v", deleted, err)
				}
				if _, err := repo.Get(ctx, first.AccountIDs[0]); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("delete not committed: %v", err)
				}
			} else {
				port.afterRead = func() {
					deleted, err := service.BatchDelete(ctx, first.AccountIDs)
					if err != nil || deleted != 1 {
						t.Fatalf("delete in prefilter gap: %d, %v", deleted, err)
					}
					marks, err := repo.TombstonedEmails(ctx, []string{"g20@example.test"})
					if err != nil || len(marks) != 1 {
						t.Fatalf("committed tombstone missing: %v, %v", marks, err)
					}
				}
			}
			second, err := service.ImportCredentials(ctx, []byte("doc"))
			if err != nil {
				t.Fatal(err)
			}
			if second.Created != 0 || second.Skipped != 1 {
				t.Fatalf("committed manual deletion must prevent resurrection without explicit clear: %+v", second)
			}
		})
	}
}
