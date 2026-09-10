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

func TestAccountAdministrationOwnsFieldPatches(t *testing.T) {
	consumers := 0
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
			switch value := node.(type) {
			case *ast.FuncDecl:
				if value.Name.Name == "SetAccountEnabled" || value.Name.Name == "UpdateRiskStatus" {
					t.Errorf("%s restores removed admin state writer %s", path, value.Name.Name)
				}
				if filepath.ToSlash(path) == "../application/account/service.go" && value.Name.Name == "Update" {
					ast.Inspect(value.Body, func(node ast.Node) bool {
						call, ok := node.(*ast.CallExpr)
						if !ok {
							return true
						}
						sel, ok := call.Fun.(*ast.SelectorExpr)
						if !ok {
							return true
						}
						if sel.Sel.Name == "Update" || sel.Sel.Name == "UpdateRiskAttribution" || sel.Sel.Name == "UpdateTokens" {
							t.Errorf("admin command regressed to whole-account or separate state write %s", sel.Sel.Name)
						}
						return true
					})
				}
			case *ast.CallExpr:
				sel, ok := value.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "UpdateAdministration" {
					consumers++
					if filepath.ToSlash(path) != "../application/account/service.go" {
						t.Errorf("%s bypasses the account administration use case", path)
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
	if consumers != 1 {
		t.Fatalf("administration use case consumers=%d, want 1", consumers)
	}
}
