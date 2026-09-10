package relational

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestQuotaConsumptionSnapshotFencingAndRecovery(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx, now := context.Background(), time.Now().UTC().Truncate(time.Microsecond)
			credential, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "quota-facts", SourceKey: "quota-facts", AuthType: account.AuthTypeSSO, EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			id, mode := credential.ID, account.QuotaModeWebVideo720p
			window := func() account.QuotaWindow {
				t.Helper()
				values, err := rb.GetQuotaWindows(ctx, []uint64{id})
				if err != nil {
					t.Fatal(err)
				}
				for _, value := range values[id] {
					if value.Mode == mode {
						return value
					}
				}
				t.Fatal("missing quota window")
				return account.QuotaWindow{}
			}
			revision := func() uint64 {
				t.Helper()
				value, err := rb.GetQuotaRevision(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			snapshot := func(expected uint64, remaining int) error {
				return ra.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: id, Revision: expected, SyncedAt: now, Windows: []account.QuotaWindow{{Mode: mode, Remaining: remaining, Total: 20}}, ReplaceAll: true})
			}
			if err := snapshot(0, 20); err != nil {
				t.Fatal(err)
			}
			// Simulate the existing schema: migration must preserve the window and
			// adopt version zero, not fabricate a remotely observed identity.
			for _, name := range []string{"revision", "snapshot_version"} {
				if err := a.db.Migrator().DropConstraint(&quotaWindowModel{}, "chk_account_quota_windows_"+name); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"revision", "snapshot_version"} {
				if err := a.db.Migrator().DropColumn(&quotaWindowModel{}, name); err != nil {
					t.Fatal(err)
				}
			}
			for _, model := range []any{&quotaStateModel{}, &quotaConsumptionModel{}} {
				if err := a.db.Migrator().DropTable(model); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			if got := window(); got.SnapshotVersion != 0 || got.Remaining != 20 {
				t.Fatalf("migration changed observation: %+v", got)
			}
			legacy := account.QuotaConsumption{EventID: "legacy-video", AccountID: id, Mode: mode, Units: 1}
			if receipt, err := rb.ConsumeQuota(ctx, legacy, now); err != nil || receipt.State != account.QuotaConsumptionPendingRefresh {
				t.Fatalf("legacy = %+v %v", receipt, err)
			}
			if got := window().Remaining; got != 20 {
				t.Fatalf("unknown snapshot decremented: %d", got)
			}
			if err := snapshot(revision(), 20); err != nil {
				t.Fatal(err)
			}
			if receipt, err := ra.ConsumeQuota(ctx, legacy, now); err != nil || receipt.State != account.QuotaConsumptionRefreshed {
				t.Fatalf("legacy refresh = %+v %v", receipt, err)
			}
			fact := account.QuotaConsumption{EventID: "generated-video", AccountID: id, Mode: mode, SnapshotVersion: window().SnapshotVersion, Units: 1}
			before := revision()
			var wg sync.WaitGroup
			results := make(chan error, 8)
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					receipt, err := repo.ConsumeQuota(ctx, fact, now)
					if err == nil && receipt.State != account.QuotaConsumptionApplied {
						err = errors.New("missing applied receipt")
					}
					results <- err
				}()
			}
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := window(); got.Remaining != 19 || got.SnapshotVersion != fact.SnapshotVersion || revision() != before+1 {
				t.Fatalf("duplicate changed estimate/version: %+v revision=%d", got, revision())
			}
			conflict := fact
			conflict.Units = 2
			if _, err := rb.ConsumeQuota(ctx, conflict, now); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("payload rewrite = %v", err)
			}
			if err := snapshot(before, 20); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("query before generation overwrote consumption: %v", err)
			}
			if got := window().Remaining; got != 19 {
				t.Fatalf("stale query changed remaining: %d", got)
			}
			if err := snapshot(revision(), 15); err != nil {
				t.Fatal(err)
			}
			if _, err := rb.ConsumeQuota(ctx, fact, now); err != nil {
				t.Fatal(err)
			}
			if got := window().Remaining; got != 15 {
				t.Fatalf("late replay hit new snapshot: %d", got)
			}
			stale := fact
			stale.EventID = "late-other-video"
			before = revision()
			if receipt, err := rb.ConsumeQuota(ctx, stale, now); err != nil || receipt.State != account.QuotaConsumptionPendingRefresh {
				t.Fatalf("stale = %+v %v", receipt, err)
			}
			pending, err := ra.ListPendingQuotaRefreshes(ctx, 0, 1)
			if err != nil || len(pending) != 1 || pending[0].AccountID != id || pending[0].Mode != mode {
				t.Fatalf("durable pending = %+v %v", pending, err)
			}
			if got := window().Remaining; got != 15 {
				t.Fatalf("old-window fact changed current remaining: %d", got)
			}
			if err := snapshot(before, 17); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("query predating accepted fact = %v", err)
			}
			if err := snapshot(revision(), 14); err != nil {
				t.Fatal(err)
			}
			if receipt, err := rb.ConsumeQuota(ctx, stale, now); err != nil || receipt.State != account.QuotaConsumptionRefreshed {
				t.Fatalf("later observation = %+v %v", receipt, err)
			}
			// Two independent observers can install at most one response for their
			// shared revision, even if their wall clocks are identical.
			before = revision()
			writes := make(chan error, 2)
			for _, repo := range []*AccountRepository{ra, rb} {
				go func() {
					writes <- repo.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: id, Revision: before, SyncedAt: now, Windows: []account.QuotaWindow{{Mode: mode, Remaining: 12, Total: 20}}})
				}()
			}
			passed, rejected := 0, 0
			for range 2 {
				if err := <-writes; err == nil {
					passed++
				} else if errors.Is(err, repository.ErrConflict) {
					rejected++
				} else {
					t.Fatal(err)
				}
			}
			if passed != 1 || rejected != 1 {
				t.Fatalf("snapshot CAS successes=%d conflicts=%d", passed, rejected)
			}
			// An expired window has no numeric authority for a new completion.
			expired := now.Add(-time.Second)
			if err := ra.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: id, Revision: revision(), SyncedAt: now, Windows: []account.QuotaWindow{{Mode: mode, Remaining: 12, Total: 20, ResetAt: &expired}}}); err != nil {
				t.Fatal(err)
			}
			expiredFact := fact
			expiredFact.EventID = "expired-video"
			expiredFact.SnapshotVersion = window().SnapshotVersion
			if receipt, err := rb.ConsumeQuota(ctx, expiredFact, now); err != nil || receipt.State != account.QuotaConsumptionPendingRefresh {
				t.Fatalf("expired = %+v %v", receipt, err)
			}
			// Account deletion must not remove identity tombstones or apply a late
			// event to a subsequently unrelated account.
			if err := ra.Delete(ctx, id); err != nil {
				t.Fatal(err)
			}
			if receipt, err := rb.ConsumeQuota(ctx, fact, now); err != nil || receipt.State != account.QuotaConsumptionApplied {
				t.Fatalf("deleted replay = %+v %v", receipt, err)
			}
			if receipt, err := rb.ConsumeQuota(ctx, expiredFact, now); err != nil || receipt.State != account.QuotaConsumptionAccountDeleted {
				t.Fatalf("deleted pending = %+v %v", receipt, err)
			}
			unknown := fact
			unknown.EventID = "after-delete"
			if receipt, err := rb.ConsumeQuota(ctx, unknown, now); err != nil || receipt.State != account.QuotaConsumptionAccountDeleted {
				t.Fatalf("deleted new fact = %+v %v", receipt, err)
			}
			if _, err := rb.GetQuotaRevision(ctx, id); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("deleted account got quota state: %v", err)
			}
		})
	}
}
