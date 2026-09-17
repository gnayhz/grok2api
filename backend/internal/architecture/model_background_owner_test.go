package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetachedAccountModelSyncHasModelOwner(t *testing.T) {
	owners, requests := 0, 0
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		path = filepath.ToSlash(path)
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.FuncDecl:
				if value.Name.Name == "queueAccountModelSync" {
					t.Errorf("%s restores Gateway's detached model task", path)
				}
				if value.Name.Name == "QueueAccountSync" {
					owners++
					if !strings.HasPrefix(path, "../application/model/") {
						t.Errorf("%s owns detached model refresh outside M05", path)
					}
				}
			case *ast.SelectorExpr:
				if value.Sel.Name == "QueueAccountSync" {
					requests++
					if path != "../application/gateway/response_attempt.go" {
						t.Errorf("%s adds a catalog-change trigger outside the current request boundary", path)
					}
				}
				if value.Sel.Name == "SyncAccount" && strings.HasPrefix(path, "../application/gateway/") {
					t.Errorf("%s bypasses M05 ownership of detached catalog refresh", path)
				}
			case *ast.Field:
				if strings.HasPrefix(path, "../application/gateway/") {
					for _, name := range value.Names {
						if name.Name == "modelSyncing" || name.Name == "modelSyncMu" {
							t.Errorf("%s retains a second model refresh registry", path)
						}
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if owners != 1 || requests != 1 {
		t.Fatalf("detached model owners=%d catalog-change boundaries=%d", owners, requests)
	}
}
