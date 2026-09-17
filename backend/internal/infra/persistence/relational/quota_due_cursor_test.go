package relational

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestDueQuotaWindowCursorAndProviderFilter(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, reader := settingsDatabasePair(t, dialect)
			writer, repo := NewAccountRepository(db), NewAccountRepository(reader)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			var webID uint64
			for _, provider := range []account.Provider{account.ProviderConsole, account.ProviderWeb} {
				credential, _, err := writer.UpsertByIdentity(ctx, account.Credential{
					Provider: provider, Name: "synthetic-cursor", SourceKey: "synthetic-cursor-" + string(provider),
					EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
				})
				if err != nil {
					t.Fatal(err)
				}
				modes := []string{"billing", "console"}
				if provider == account.ProviderWeb {
					modes, webID = []string{"fast"}, credential.ID
				}
				var windows []account.QuotaWindow
				for _, mode := range modes {
					windows = append(windows, account.QuotaWindow{AccountID: credential.ID, Mode: mode, Total: 10, ResetAt: &now, Source: account.QuotaSourceUpstream, SyncedAt: &now, UpdatedAt: now})
				}
				if err := saveQuotaWindowsFixture(writer, ctx, credential.ID, "", now, windows); err != nil {
					t.Fatal(err)
				}
			}
			var cursor *repository.QuotaWindowCursor
			var modes []string
			for i := 0; i < 4; i++ {
				windows, err := repo.ListDueQuotaWindows(ctx, now, repository.DueQuotaWindowQuery{Limit: 1, After: cursor})
				if err != nil {
					t.Fatal(err)
				}
				if len(windows) == 0 {
					break
				}
				w := windows[0]
				if w.ResetAt == nil || w.AccountID == 0 || w.Provider == "" {
					t.Fatalf("incomplete projection: %#v", w)
				}
				modes = append(modes, w.Mode)
				cursor = &repository.QuotaWindowCursor{ResetAt: *w.ResetAt, AccountID: w.AccountID, Mode: w.Mode}
			}
			if !slices.Equal(modes, []string{"billing", "console", "fast"}) {
				t.Fatalf("cursor skipped or repeated tied windows: %v", modes)
			}
			windows, err := repo.ListDueQuotaWindows(ctx, now, repository.DueQuotaWindowQuery{Limit: 1, Provider: account.ProviderWeb})
			if err != nil || len(windows) != 1 || windows[0].AccountID != webID {
				t.Fatalf("provider filter must precede limit: windows=%#v err=%v", windows, err)
			}
		})
	}
}
