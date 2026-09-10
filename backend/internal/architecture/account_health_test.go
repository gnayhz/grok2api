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

func TestAccountHealthPolicyUsesDomainTransitions(t *testing.T) {
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
		ast.Inspect(file, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.FuncDecl:
				if v.Name.Name == "UpdateHealth" || v.Name.Name == "UpdateQualityIdleCooldown" || v.Name.Name == "clearAccountPenalty" || v.Name.Name == "migrateLegacyQualityHolds" || v.Name.Name == "preserveQualityHistory" {
					t.Errorf("%s restores absolute health mutation %s", path, v.Name.Name)
				}
			case *ast.CallExpr:
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "UpdateHealth" || sel.Sel.Name == "UpdateQualityIdleCooldown" {
					t.Errorf("%s calls removed absolute health mutation", path)
				}
				if sel.Sel.Name == "TransitionHealth" {
					checked++
					if !strings.HasSuffix(filepath.ToSlash(path), "/infra/persistence/relational/account_health.go") {
						t.Errorf("%s evaluates account state policy outside its atomic persistence boundary", path)
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
	if checked != 1 {
		t.Fatalf("health transition owners=%d, want one atomic application", checked)
	}
}
