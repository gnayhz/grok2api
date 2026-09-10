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

func TestQuotaMutationsKeepConsumptionIdentityAndAccountOwner(t *testing.T) {
	removed := map[string]bool{"DecrementQuota": true, "DecrementWebQuota": true, "DecrementQuotaWindow": true, "DecrementQuotaWindowBy": true, "SaveQuotaWindows": true, "ReplaceQuotaWindows": true, "ReplaceQuotaWindowGroup": true}
	checked := 0
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
		source := filepath.ToSlash(path)
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.FuncDecl:
				if removed[value.Name.Name] {
					t.Errorf("%s: removed quota mutation %s bypasses durable identity or pre-query revision", path, value.Name.Name)
				}
			case *ast.CallExpr:
				selector, ok := value.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if removed[selector.Sel.Name] {
					t.Errorf("%s: caller restored removed quota mutation %s", path, selector.Sel.Name)
				}
				if selector.Sel.Name == "SaveQuotaSnapshot" {
					checked++
					if !strings.Contains(source, "/application/account/") && !strings.Contains(source, "/infra/persistence/relational/") {
						t.Errorf("%s: quota snapshot policy belongs to the account owner", path)
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
	if checked == 0 {
		t.Fatal("no production quota snapshot writer inspected")
	}
}

func TestQuotaRefreshRuntimeSignalsHaveOneApplicationOwner(t *testing.T) {
	inspected := 0
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
		ast.Inspect(file, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.FuncDecl:
				if v.Name.Name == "QuotaRefreshGeneration" || v.Name.Name == "ListQuotaRefreshDirty" {
					t.Errorf("%s restores obsolete quota lifetime/page contract", path)
				}
			case *ast.CallExpr:
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "MarkQuotaRefreshDirty", "ClearQuotaRefreshDirty", "ScanQuotaRefreshDirty":
					inspected++
					if filepath.ToSlash(path) != "../application/account/quota_refresh_queue.go" {
						t.Errorf("%s bypasses M07 quota demand/retry owner: %s", path, sel.Sel.Name)
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
	if inspected < 3 {
		t.Fatal("quota signal consumers not inspected")
	}
}
