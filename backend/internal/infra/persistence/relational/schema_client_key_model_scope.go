package relational

import "context"

// The one-time backfill can prove restricted intent only from surviving grants.
// Later startups must preserve restricted-empty keys. The outer schema
// transaction includes the new column, constraints and this backfill.
func (d *Database) migrateClientKeyModelScope(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	if !db.Migrator().HasTable("client_keys") || db.Migrator().HasColumn("client_keys", "model_scope") {
		return nil
	}
	columnType, defaultValue := "text", `"all"`
	if d.dialect == "postgres" {
		columnType, defaultValue = "varchar(16)", "'all'"
	}
	if err := db.Exec("ALTER TABLE client_keys ADD COLUMN model_scope " + columnType + " NOT NULL CONSTRAINT chk_client_keys_model_scope CHECK (model_scope IN ('all','restricted')) DEFAULT " + defaultValue).Error; err != nil {
		return err
	}
	if !db.Migrator().HasTable("client_key_models") {
		return nil
	}
	return db.Exec("UPDATE client_keys SET model_scope = 'restricted' WHERE EXISTS (SELECT 1 FROM client_key_models p WHERE p.client_key_id = client_keys.id)").Error
}
