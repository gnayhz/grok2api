package relational

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// A public name can refer to several capabilities or manual routing targets.
// AutoMigrate cannot replace the old alias-only primary key. InitializeSchema
// supplies the migration lock, transactional DDL and SQLite FK handling.
func (d *Database) migrateModelAliasKey(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	const table = "model_route_aliases"
	if !db.Migrator().HasTable(table) {
		return nil
	}
	var primary []string
	if d.dialect == "sqlite" {
		var columns []struct {
			Name string
			PK   int
		}
		if err := db.Raw("PRAGMA table_info(model_route_aliases)").Scan(&columns).Error; err != nil {
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
	if slices.Equal(primary, []string{"alias", "model_route_id"}) {
		return nil
	}
	if !slices.Equal(primary, []string{"alias"}) {
		return fmt.Errorf("unexpected model alias primary key: %v", primary)
	}
	switch d.dialect {
	case "postgres":
		var name string
		if err := db.Raw("SELECT conname FROM pg_constraint WHERE conrelid = to_regclass(?) AND contype = 'p'", table).Scan(&name).Error; err != nil {
			return err
		}
		if name == "" {
			return fmt.Errorf("model alias primary constraint missing")
		}
		var quoted strings.Builder
		db.Dialector.QuoteTo(&quoted, name)
		if err := db.Exec("ALTER TABLE model_route_aliases DROP CONSTRAINT " + quoted.String()).Error; err != nil {
			return err
		}
		return db.Exec("ALTER TABLE model_route_aliases ADD PRIMARY KEY (alias, model_route_id)").Error
	case "sqlite":
		sourceColumn := "'legacy'"
		if db.Migrator().HasColumn(table, "name_source") {
			sourceColumn = "name_source"
		}
		replacedColumn := "false"
		if db.Migrator().HasColumn(table, "replaced_by_catalog") {
			replacedColumn = "replaced_by_catalog"
		}
		var objects []struct{ SQL string }
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE tbl_name = ? AND type IN ('index', 'trigger') AND sql IS NOT NULL ORDER BY type, name", table).Scan(&objects).Error; err != nil {
			return err
		}
		if err := db.Exec(`CREATE TABLE model_route_aliases_migration (
            alias text NOT NULL,
            model_route_id integer NOT NULL,
            created_at datetime NOT NULL,
            name_source text NOT NULL DEFAULT "legacy",
            replaced_by_catalog numeric NOT NULL DEFAULT false,
            PRIMARY KEY (alias, model_route_id),
            CONSTRAINT chk_model_route_aliases_alias CHECK (length(trim(alias)) BETWEEN 1 AND 255),
            CONSTRAINT chk_model_route_aliases_name_source CHECK (name_source IN ('legacy','generated','manual')),
            CONSTRAINT chk_model_route_aliases_replacement CHECK (NOT replaced_by_catalog OR name_source = 'legacy'),
            CONSTRAINT fk_model_route_aliases_model_route FOREIGN KEY (model_route_id) REFERENCES model_routes(id) ON UPDATE CASCADE ON DELETE CASCADE
        )`).Error; err != nil {
			return err
		}
		if err := db.Exec("INSERT INTO model_route_aliases_migration (alias, model_route_id, name_source, replaced_by_catalog, created_at) SELECT alias, model_route_id, " + sourceColumn + ", " + replacedColumn + ", created_at FROM model_route_aliases").Error; err != nil {
			return err
		}
		if err := db.Exec("DROP TABLE model_route_aliases").Error; err != nil {
			return err
		}
		if err := db.Exec("ALTER TABLE model_route_aliases_migration RENAME TO model_route_aliases").Error; err != nil {
			return err
		}
		for _, object := range objects {
			if err := db.Exec(object.SQL).Error; err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("model alias key migration unsupported for %s", d.dialect)
	}
}
