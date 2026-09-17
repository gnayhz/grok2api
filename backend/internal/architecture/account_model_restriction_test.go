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

func TestModelRestrictionHasOneAtomicPolicyOwner(t *testing.T) {
	transitions := 0
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
			if call, ok := node.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "TransitionModelRestriction":
						transitions++
						if path != "../infra/persistence/relational/account_model_restriction.go" {
							t.Errorf("%s evaluates model policy outside the locked owner", path)
						}
					case "ApplyModelRestriction":
						if path != "../application/selector/selector.go" && path != "../application/account/build_detect.go" && path != "../testsupport/account.go" {
							t.Errorf("%s bypasses model outcome owners", path)
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
	if transitions != 1 {
		t.Fatalf("model policy owners=%d", transitions)
	}
}
