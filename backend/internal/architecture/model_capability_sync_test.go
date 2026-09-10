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

func TestModelCapabilityObservationsHaveOneOwner(t *testing.T) {
	starts, completions := 0, 0
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
			if ident, ok := node.(*ast.Ident); ok && (ident.Name == "ReplaceAccountCapabilities" || ident.Name == "MarkAccountCapabilitySyncFailed") {
				t.Errorf("%s retains unversioned capability writer %s", path, ident.Name)
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "BeginAccountCapabilitySync" && sel.Sel.Name != "CompleteAccountCapabilitySync") {
				return true
			}
			if path != "../application/model/service.go" && path != "../testsupport/model.go" {
				t.Errorf("%s publishes capability observations outside M05", path)
			}
			if path == "../application/model/service.go" {
				if sel.Sel.Name == "BeginAccountCapabilitySync" {
					starts++
				} else {
					completions++
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || completions != 2 {
		t.Fatalf("M05 claim/success/failure owners = %d/%d", starts, completions)
	}
}
