package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestRetiredAccountMetadataSurvivesUpgrade(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, reader := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			credential, _, err := NewAccountRepository(db).UpsertByIdentity(ctx, account.Credential{
				Provider: account.ProviderBuild, Name: "synthetic-upgrade", SourceKey: "synthetic-upgrade",
				EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			exec := func(sql string, args ...any) {
				t.Helper()
				if err := db.db.WithContext(ctx).Exec(sql, args...).Error; err != nil {
					t.Fatal(err)
				}
			}
			// Recreate the old persisted shape, including values the current
			// runtime no longer consumes. Retirement must not destroy history.
			exec("ALTER TABLE provider_accounts ADD COLUMN egress_assignment_mode text NOT NULL DEFAULT '' CHECK (egress_assignment_mode IN ('','manual','auto'))")
			exec("ALTER TABLE provider_accounts ADD COLUMN egress_assigned_at timestamp")
			assigned := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			exec("UPDATE provider_accounts SET egress_assignment_mode = 'manual', egress_assigned_at = ? WHERE id = ?", assigned, credential.ID)
			exec("CREATE TABLE account_risk_verdicts (account_id bigint PRIMARY KEY, verdict text NOT NULL)")
			exec("INSERT INTO account_risk_verdicts (account_id, verdict) VALUES (?, 'flagged')", credential.ID)
			exec("DELETE FROM schema_migration_markers WHERE name = 'drop_account_risk_verdicts'")
			for i := 0; i < 2; i++ {
				if err := db.InitializeSchema(ctx); err != nil {
					t.Fatalf("upgrade %d: %v", i, err)
				}
			}
			var count int64
			if err := reader.db.Raw("SELECT COUNT(*) FROM provider_accounts WHERE id = ? AND egress_assignment_mode = 'manual' AND egress_assigned_at = ?", credential.ID, assigned).Scan(&count).Error; err != nil || count != 1 {
				t.Fatalf("assignment history lost: count=%d err=%v", count, err)
			}
			if err := reader.db.Raw("SELECT COUNT(*) FROM account_risk_verdicts WHERE account_id = ? AND verdict = 'flagged'", credential.ID).Scan(&count).Error; err != nil || count != 1 {
				t.Fatalf("risk history lost: count=%d err=%v", count, err)
			}
		})
	}
}
