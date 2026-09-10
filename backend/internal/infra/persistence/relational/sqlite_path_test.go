package relational

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteFilesystemPathsRemainDistinct(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for index, suffix := range []string{"#one", "#two", "?mode=memory", "%20 + 空格"} {
		path := filepath.Join(root, "database"+suffix, "state.db")
		for pass := 0; pass < 2; pass++ {
			db, err := OpenSQLite(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if pass == 0 {
				if err := db.db.Exec("CREATE TABLE path_identity (id INTEGER)").Error; err != nil {
					t.Fatal(err)
				}
				if err := db.db.Exec("INSERT INTO path_identity VALUES (?)", index).Error; err != nil {
					t.Fatal(err)
				}
			}
			var got int
			if err := db.db.Raw("SELECT id FROM path_identity").Scan(&got).Error; err != nil || got != index {
				t.Fatalf("path %q collided or did not persist: got=%d error=%v", path, got, err)
			}
			var foreignKeys int
			if err := db.db.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil || foreignKeys != 1 {
				t.Fatalf("path changed connection policy: %d %v", foreignKeys, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("opened wrong file for %q: %v", path, err)
			}
		}
	}
}
