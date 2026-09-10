package relational

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"gorm.io/gorm"
)

func TestClientKeyModelScopeLegacyMigrationIsAtomicAndOneTime(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			restricted := seedKeyConsistency(t, a)
			unlimited, err := NewClientKeyRepository(a).Create(ctx, clientkey.Key{Name: "legacy unlimited", Prefix: "legacy-all", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			when := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
			if err := a.db.Model(&clientKeyModel{}).Where("id IN ?", []uint64{restricted.ID, unlimited.ID}).Updates(map[string]any{"created_at": when, "updated_at": when, "billed_usage_usd_ticks": 19}).Error; err != nil {
				t.Fatal(err)
			}
			downgrade := func() error {
				return a.db.Transaction(func(tx *gorm.DB) error {
					if err := tx.Migrator().DropConstraint(&clientKeyModel{}, "chk_client_keys_model_scope"); err != nil {
						return err
					}
					return tx.Migrator().DropColumn(&clientKeyModel{}, "model_scope")
				})
			}
			if dialect == "sqlite" {
				err = a.withSQLiteForeignKeysDisabled(ctx, downgrade)
			} else {
				err = downgrade()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := a.db.Exec("CREATE INDEX client_keys_legacy_created ON client_keys(created_at)").Error; err != nil {
				t.Fatal(err)
			}
			if dialect == "sqlite" {
				if err := a.db.Exec("CREATE TRIGGER client_keys_legacy_trigger AFTER INSERT ON client_keys BEGIN SELECT 1; END").Error; err != nil {
					t.Fatal(err)
				}
			}
			// Prime the peer connection against the actual old column set.
			var oldRows []map[string]any
			if err := b.db.Table("client_keys").Find(&oldRows).Error; err != nil || len(oldRows) != 2 {
				t.Fatalf("legacy read: %d %v", len(oldRows), err)
			}
			failure := errors.New("whole schema failure after key scope backfill")
			callback := "g14_schema_fail"
			if err := a.db.Callback().Raw().Before("gorm:raw").Register(callback, func(tx *gorm.DB) {
				if strings.Contains(tx.Statement.SQL.String(), "CREATE INDEX IF NOT EXISTS idx_client_keys_created_id") {
					_ = tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			err = a.InitializeSchema(ctx)
			_ = a.db.Callback().Raw().Remove(callback)
			if !errors.Is(err, failure) {
				t.Fatalf("injected migration: %v", err)
			}
			if a.db.Migrator().HasColumn("client_keys", "model_scope") {
				t.Fatal("failed migration left new column")
			}
			if err := a.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			for _, input := range []struct {
				id    uint64
				scope clientkey.ModelScope
				count int
			}{{restricted.ID, clientkey.ModelScopeRestricted, 1}, {unlimited.ID, clientkey.ModelScopeAll, 0}} {
				value, err := NewClientKeyRepository(b).Get(ctx, input.id)
				if err != nil || value.ModelScope != input.scope || len(value.AllowedModels) != input.count || value.BilledUsageUSDTicks != 19 || !value.CreatedAt.Equal(when) || !value.UpdatedAt.Equal(when) {
					t.Fatalf("migrated key %d: %+v %v", input.id, value, err)
				}
			}
			if !b.db.Migrator().HasIndex(&clientKeyModel{}, "client_keys_legacy_created") {
				t.Fatal("custom index lost")
			}
			if dialect == "sqlite" {
				var count int64
				if err := b.db.Raw("SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='client_keys_legacy_trigger'").Scan(&count).Error; err != nil || count != 1 {
					t.Fatalf("custom trigger %d %v", count, err)
				}
			}
			if err := b.db.Model(&clientKeyModel{}).Where("id = ?", restricted.ID).Update("model_scope", "invalid").Error; err == nil {
				t.Fatal("SQL accepted invalid scope")
			}
			if err := NewModelRepository(b).Delete(ctx, restricted.AllowedModels[0]); err != nil {
				t.Fatal(err)
			}
			if err := a.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			value, err := NewClientKeyRepository(a).Get(ctx, restricted.ID)
			if err != nil || value.ModelScope != clientkey.ModelScopeRestricted || len(value.AllowedModels) != 0 || value.AllowsModel(999) {
				t.Fatalf("reinitialization widened empty restriction: %+v %v", value, err)
			}
			if !a.db.Migrator().HasIndex(&clientKeyModel{}, "client_keys_legacy_created") {
				t.Fatal("reinitialization lost custom index")
			}
		})
	}
}
