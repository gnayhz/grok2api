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

func TestModelPublicationHasApplicationOwner(t *testing.T) {
	calls := map[string]int{}
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
			if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING && strings.Contains(literal.Value, "grok-imagine-image-quality-lite") && path != "../domain/model/name_ownership.go" {
				t.Errorf("%s owns retired model naming policy outside M05", path)
			}
			if ident, ok := node.(*ast.Ident); ok && (ident.Name == "UpsertRoutes" || ident.Name == "UpsertDiscovered" || ident.Name == "discoveredRouteDefaults") {
				t.Errorf("%s retains obsolete catalog policy/writer %s", path, ident.Name)
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if strings.HasPrefix(path, "../application/model/") && sel.Sel.Name == "QuotaMode" {
				t.Errorf("%s infers model product eligibility from account quota metadata", path)
			}
			if sel.Sel.Name != "MergeRoutes" && sel.Sel.Name != "ReplaceProviderRoutes" {
				return true
			}
			if path != "../application/model/catalog.go" && path != "../testsupport/model.go" {
				t.Errorf("%s publishes model catalog outside M05", path)
			}
			if path == "../application/model/catalog.go" {
				calls[sel.Sel.Name]++
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls["MergeRoutes"] != 1 || calls["ReplaceProviderRoutes"] != 1 {
		t.Fatalf("M05 publication consumers: %v", calls)
	}
}
