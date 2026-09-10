package relational

import (
	"context"
	"fmt"
)

// Add the column and its named constraint together. Letting SQLite AutoMigrate
// add the CHECK separately rebuilds the table and loses custom indexes/triggers.
// InitializeSchema supplies the schema lock and the whole migration transaction.
func (d *Database) migrateModelNameSources(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	columnType := "text"
	// Match the SQLite driver's DDL parser as well as SQLite itself: it strips
	// double quotes from defaults, but retains single quotes and then rebuilds.
	defaultValue := `"legacy"`
	if d.dialect == "postgres" {
		columnType = "varchar(16)"
		defaultValue = "'legacy'"
	}
	for _, table := range []string{"model_routes", "model_route_aliases"} {
		if !db.Migrator().HasTable(table) || db.Migrator().HasColumn(table, "name_source") {
			continue
		}
		statement := fmt.Sprintf("ALTER TABLE %s ADD COLUMN name_source %s NOT NULL CONSTRAINT chk_%s_name_source CHECK (name_source IN ('legacy','generated','manual')) DEFAULT %s", table, columnType, table, defaultValue)
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	if db.Migrator().HasTable("model_route_aliases") && !db.Migrator().HasColumn("model_route_aliases", "replaced_by_catalog") {
		columnType = "numeric"
		if d.dialect == "postgres" {
			columnType = "boolean"
		}
		statement := "ALTER TABLE model_route_aliases ADD COLUMN replaced_by_catalog " + columnType + " NOT NULL CONSTRAINT chk_model_route_aliases_replacement CHECK (NOT replaced_by_catalog OR name_source = 'legacy') DEFAULT false"
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
