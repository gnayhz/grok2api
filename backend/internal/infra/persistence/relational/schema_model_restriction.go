package relational

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Model denials and model quota exhaustion used to share one key. Change the
// primary key explicitly: AutoMigrate does not replace existing primary keys.
// InitializeSchema supplies the migration lock and the whole-DDL transaction.
func (d *Database) migrateModelRestrictionKey(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	const table = "account_model_quota_blocks"
	if !db.Migrator().HasTable(table) {
		return nil
	}
	var primary []string
	if d.dialect == "sqlite" {
		// Inspect the database's key metadata, not the driver's SQL-text parser.
		var columns []struct {
			Name string
			PK   int
		}
		if err := db.Raw("PRAGMA table_info(account_model_quota_blocks)").Scan(&columns).Error; err != nil {
			return err
		}
		for _, column := range columns {
			if column.PK > 0 {
				primary = append(primary, column.Name)
			}
		}
	} else {
		columns, err := db.Migrator().ColumnTypes(table)
		if err != nil {
			return err
		}
		for _, column := range columns {
			if key, known := column.PrimaryKey(); known && key {
				primary = append(primary, column.Name())
			}
		}
	}
	slices.Sort(primary)
	if slices.Equal(primary, []string{"account_id", "reason", "upstream_model"}) {
		return nil
	}
	if !slices.Equal(primary, []string{"account_id", "upstream_model"}) {
		return fmt.Errorf("unexpected model restriction primary key: %v", primary)
	}
	switch d.dialect {
	case "postgres":
		var name string
		if err := db.Raw("SELECT conname FROM pg_constraint WHERE conrelid = to_regclass(?) AND contype = 'p'", table).Scan(&name).Error; err != nil {
			return err
		}
		if name == "" {
			return fmt.Errorf("model restriction primary constraint missing")
		}
		var quoted strings.Builder
		db.Dialector.QuoteTo(&quoted, name)
		if err := db.Exec("ALTER TABLE account_model_quota_blocks DROP CONSTRAINT " + quoted.String()).Error; err != nil {
			return err
		}
		return db.Exec("ALTER TABLE account_model_quota_blocks ADD PRIMARY KEY (account_id, upstream_model, reason)").Error
	case "sqlite":
		// Retain table-owned explicit indexes/triggers as well as every old reason.
		var objects []struct{ SQL string }
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE tbl_name = ? AND type IN ('index', 'trigger') AND sql IS NOT NULL ORDER BY type, name", table).Scan(&objects).Error; err != nil {
			return err
		}
		if err := db.Exec(`CREATE TABLE account_model_quota_blocks_migration (
   account_id integer NOT NULL,
   upstream_model text NOT NULL,
   reason text NOT NULL,
   cooldown_until datetime NOT NULL,
   updated_at datetime NOT NULL,
   PRIMARY KEY (account_id,upstream_model,reason),
   CONSTRAINT fk_account_model_quota_blocks_account FOREIGN KEY (account_id) REFERENCES provider_accounts(id) ON UPDATE CASCADE ON DELETE CASCADE,
   CONSTRAINT chk_account_model_quota_blocks_model CHECK (length(trim(upstream_model)) BETWEEN 1 AND 255),
   CONSTRAINT chk_account_model_quota_blocks_reason CHECK (length(trim(reason)) BETWEEN 1 AND 100)
  )`).Error; err != nil {
			return err
		}
		if err := db.Exec(`INSERT INTO account_model_quota_blocks_migration (account_id, upstream_model, reason, cooldown_until, updated_at)
   SELECT account_id, upstream_model, reason, cooldown_until, updated_at FROM account_model_quota_blocks`).Error; err != nil {
			return err
		}
		if err := db.Exec("DROP TABLE account_model_quota_blocks").Error; err != nil {
			return err
		}
		if err := db.Exec("ALTER TABLE account_model_quota_blocks_migration RENAME TO account_model_quota_blocks").Error; err != nil {
			return err
		}
		for _, object := range objects {
			if err := db.Exec(object.SQL).Error; err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("model restriction key migration unsupported for %s", d.dialect)
	}
}
