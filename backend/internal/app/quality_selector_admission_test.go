package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type qualityAdmissionMaterialRepository struct {
	*relational.AccountRepository
	after func()
}

func (r *qualityAdmissionMaterialRepository) GetCredentialMaterial(ctx context.Context, id uint64, provider account.Provider) (account.CredentialMaterial, error) {
	value, err := r.AccountRepository.GetCredentialMaterial(ctx, id, provider)
	if err == nil {
		r.after()
	}
	return value, err
}

// The production adapter and both quality stores are real. A second SQL pool
// commits a restriction/release between the initial hint and final admission.
func TestQualitySelectorAdmissionUsesCurrentDurableAuthority(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			opts := qualityregistry.Options{Driver: dialect, SQLitePath: filepath.Join(t.TempDir(), "quality-admission.db"), MaxOpenConns: 4, MaxIdleConns: 2}
			var db *relational.Database
			var err error
			if dialect == "postgres" {
				opts.PostgresDSN = os.Getenv("TEST_POSTGRES_DSN")
				if opts.PostgresDSN == "" {
					t.Skip("isolated TEST_POSTGRES_DSN required")
				}
				db, err = relational.OpenPostgres(ctx, opts.PostgresDSN, 4, 2)
			} else {
				db, err = relational.OpenSQLite(ctx, opts.SQLitePath)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			reader, err := qualityregistry.Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			writer, err := qualityregistry.Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			readJournal, writeJournal := journal.New(reader.DB()), journal.New(writer.DB())
			repo := relational.NewAccountRepository(db)
			for _, scenario := range []string{"committed_hold", "released_with_stale_hint", "registry_without_journal"} {
				t.Run(scenario, func(t *testing.T) {
					name := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
					v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "synthetic", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
					if err != nil {
						t.Fatal(err)
					}
					eligibility := qualityAccountEligibility{registry: reader, journal: readJournal}
					if scenario == "registry_without_journal" {
						eligibility.journal = nil
					}
					limiter := memory.NewConcurrencyLimiter()
					material := &qualityAdmissionMaterialRepository{AccountRepository: repo, after: func() {
						current, err := limiter.Current(ctx, repository.AccountConcurrencyKey(v.ID))
						if err != nil || current != 1 {
							t.Fatalf("mutation must follow real slot claim: %d %v", current, err)
						}
						now := time.Now().UTC()
						if scenario == "committed_hold" {
							err := writeJournal.Record(ctx, journal.Event{Attempt: attemptmeta.Identity{ID: name, RequestID: name, AccountID: v.ID, Provider: string(v.Provider)}, Stage: "admission", Outcome: "degraded", At: now, HoldUntil: now.Add(time.Minute)})
							if err != nil {
								t.Fatal(err)
							}
							if !eligibility.AccountSchedulable(v.ID) {
								t.Fatal("fixture must retain an older permissive registry hint")
							}
							return
						}
						caseID, err := reader.CreateCase(ctx, now, `{"fixture":"selector admission"}`)
						if err != nil {
							t.Fatal(err)
						}
						if err := reader.TransitionAccount(ctx, qualityregistry.AccountTransitionRequest{AccountID: v.ID, To: model.AccountRemanded, CaseID: caseID}); err != nil {
							t.Fatal(err)
						}
						if scenario == "released_with_stale_hint" {
							if err := writer.TransitionAccount(ctx, qualityregistry.AccountTransitionRequest{AccountID: v.ID, To: model.AccountActive}); err != nil {
								t.Fatal(err)
							}
						}
						if eligibility.AccountSchedulable(v.ID) {
							t.Fatal("fixture must retain the reader's restrictive registry hint")
						}
					}}
					selector := gateway.NewSelector(material, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
					selector.UpdateConfig(time.Hour, time.Second, time.Minute, 0)
					selector.SetQualityEligibility(eligibility)
					lease, err := selector.AcquirePinned(ctx, v.Provider, v.ID, 0, "grok-test", "", false)
					if scenario == "released_with_stale_hint" {
						if err != nil || lease == nil {
							t.Fatalf("stale hint overrode durable release: %v", err)
						}
					} else if err == nil || lease != nil {
						t.Errorf("current quality restriction escaped: lease=%v err=%v", lease, err)
					}
					if lease != nil {
						lease.Release()
					}
					current, err := limiter.Current(ctx, repository.AccountConcurrencyKey(v.ID))
					if err != nil || current != 0 {
						t.Fatalf("capacity leak: %d %v", current, err)
					}
				})
			}
		})
	}
}
