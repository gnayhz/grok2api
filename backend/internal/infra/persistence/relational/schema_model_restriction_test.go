package relational

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"gorm.io/gorm"
)

// Exact old key/columns/constraints, including arbitrary historic reasons.
type legacyModelRestriction struct {
	AccountID     uint64        `gorm:"primaryKey"`
	UpstreamModel string        `gorm:"size:255;primaryKey;not null;check:chk_account_model_quota_blocks_model,length(trim(upstream_model)) BETWEEN 1 AND 255"`
	Reason        string        `gorm:"size:100;not null;check:chk_account_model_quota_blocks_reason,length(trim(reason)) BETWEEN 1 AND 100"`
	CooldownUntil time.Time     `gorm:"not null"`
	UpdatedAt     time.Time     `gorm:"not null"`
	Account       *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (legacyModelRestriction) TableName() string { return "account_model_quota_blocks" }

func modelRestrictionPrimaryKey(t *testing.T, db *Database) []string {
	t.Helper()
	if db.dialect == "sqlite" {
		var rows []struct {
			Name string
			PK   int
		}
		if err := db.db.Raw("PRAGMA table_info(account_model_quota_blocks)").Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, row := range rows {
			if row.PK != 0 {
				keys = append(keys, row.Name)
			}
		}
		slices.Sort(keys)
		return keys
	}
	columns, err := db.db.Migrator().ColumnTypes("account_model_quota_blocks")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, column := range columns {
		if primary, ok := column.PrimaryKey(); ok && primary {
			keys = append(keys, column.Name())
		}
	}
	slices.Sort(keys)
	return keys
}

func TestModelRestrictionLegacyMigrationAndRollback(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			repo := NewAccountRepository(a)
			ctx := context.Background()
			v := newRecoveryAccount(t, repo, "migration")
			if err := a.db.Migrator().DropTable(&accountModelQuotaBlockModel{}); err != nil {
				t.Fatal(err)
			}
			if err := a.db.Migrator().CreateTable(&legacyModelRestriction{}); err != nil {
				t.Fatal(err)
			}
			if dialect == "postgres" {
				if err := a.db.Exec(`ALTER TABLE account_model_quota_blocks RENAME CONSTRAINT account_model_quota_blocks_pkey TO "historic model key"`).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := a.db.Exec("CREATE INDEX model_restriction_custom_index ON account_model_quota_blocks (reason, updated_at)").Error; err != nil {
				t.Fatal(err)
			}
			if dialect == "sqlite" {
				if err := a.db.Exec(`CREATE TRIGGER model_restriction_custom_trigger AFTER UPDATE ON account_model_quota_blocks BEGIN SELECT 1; END`).Error; err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			for i, reason := range []string{"model_quota_depleted", "model_access_denied", "legacy_unknown_reason"} {
				row := legacyModelRestriction{AccountID: v.ID, UpstreamModel: reason, Reason: reason, CooldownUntil: now.Add(time.Duration(i+1) * time.Hour), UpdatedAt: now.Add(-time.Duration(i) * time.Hour)}
				if err := a.db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			}
			var before []accountModelQuotaBlockModel
			if err := a.db.Order("upstream_model").Find(&before).Error; err != nil {
				t.Fatal(err)
			}
			injected := errors.New("fail after primary key replacement")
			hook := "model_restriction_migration_failure"
			fired := false
			if err := a.db.Callback().Raw().After("gorm:raw").Register(hook, func(tx *gorm.DB) {
				statement := tx.Statement.SQL.String()
				if strings.Contains(statement, "ALTER TABLE account_model_quota_blocks_migration RENAME") || strings.Contains(statement, "ALTER TABLE account_model_quota_blocks ADD PRIMARY KEY") {
					fired = true
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			err := a.InitializeSchema(ctx)
			if cleanup := a.db.Callback().Raw().Remove(hook); cleanup != nil {
				t.Fatal(cleanup)
			}
			if !fired || !errors.Is(err, injected) {
				t.Fatalf("failure not exercised: fired=%v err=%v", fired, err)
			}
			if keys := modelRestrictionPrimaryKey(t, b); !slices.Equal(keys, []string{"account_id", "upstream_model"}) {
				t.Fatalf("rollback lost old primary key: %v", keys)
			}
			var after []accountModelQuotaBlockModel
			if err := b.db.Order("upstream_model").Find(&after).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed migration changed old rows")
			}
			if b.db.Migrator().HasTable("account_model_quota_blocks_migration") {
				t.Fatal("failed migration left temporary table")
			}
			if dialect == "sqlite" {
				var enabled int
				if err := a.db.Raw("PRAGMA foreign_keys").Scan(&enabled).Error; err != nil || enabled != 1 {
					t.Fatalf("foreign keys after rollback=%d err=%v", enabled, err)
				}
			}
			// Retry through the other independently opened connection, then initialize
			// again through the first one to prove persistent/idempotent schema state.
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			if err := a.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			if keys := modelRestrictionPrimaryKey(t, b); !slices.Equal(keys, []string{"account_id", "reason", "upstream_model"}) {
				t.Fatalf("new primary key=%v", keys)
			}
			after = nil
			if err := b.db.Order("upstream_model").Find(&after).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("successful migration changed legacy reasons/timestamps")
			}
			for _, index := range []string{"model_restriction_custom_index", "idx_account_model_quota_blocks_due"} {
				if !b.db.Migrator().HasIndex(&accountModelQuotaBlockModel{}, index) {
					t.Errorf("lost index %s", index)
				}
			}
			if dialect == "sqlite" {
				var n int64
				if err := b.db.Raw("SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='model_restriction_custom_trigger'").Scan(&n).Error; err != nil || n != 1 {
					t.Fatalf("trigger count=%d err=%v", n, err)
				}
			}
			result, err := NewAccountRepository(b).ApplyModelRestriction(ctx, v.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: account.ModelAccessDenied, UpstreamModel: "model_quota_depleted", OccurredAt: now})
			if err != nil || !result.Applied {
				t.Fatalf("second same-model reason after migration=%+v err=%v", result, err)
			}
			invalid := accountModelQuotaBlockModel{AccountID: v.ID, UpstreamModel: "", Reason: "reason", CooldownUntil: now, UpdatedAt: now}
			if err := b.db.Create(&invalid).Error; err == nil {
				t.Fatal("migration lost model check")
			}
			invalid.UpstreamModel, invalid.Reason = "valid", ""
			if err := b.db.Create(&invalid).Error; err == nil {
				t.Fatal("migration lost reason check")
			}
			invalid.AccountID, invalid.Reason = v.ID+99999, "valid"
			if err := b.db.Create(&invalid).Error; err == nil {
				t.Fatal("migration lost account foreign key")
			}
			if err := b.db.Delete(&accountModel{}, v.ID).Error; err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := b.db.Model(&accountModelQuotaBlockModel{}).Where("account_id = ?", v.ID).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("cascade count=%d err=%v", count, err)
			}
		})
	}
}
