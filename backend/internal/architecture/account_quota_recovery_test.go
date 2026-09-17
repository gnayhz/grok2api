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

func TestBuildRecoveryHasOnePolicyAndObservedWrites(t *testing.T) {
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
		for _, imp := range file.Imports {
			if strings.HasSuffix(imp.Path.Value, "/internal/testsupport\"") {
				t.Errorf("%s imports test fixtures in production", path)
			}
		}
		banned := func(name string) {
			switch name {
			case "SaveQuotaRecovery", "ClearQuotaRecovery", "ClaimQuotaProbe", "SaveBilling", "reconcilePaidQuotaRecovery", "UpsertModelQuotaBlock":
				t.Errorf("%s restores unconditional recovery policy %s", path, name)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.FuncDecl:
				banned(v.Name.Name)
			case *ast.CallExpr:
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
					banned(sel.Sel.Name)
					if sel.Sel.Name == "TransitionQuotaRecovery" {
						transitions++
						if path != "../infra/persistence/relational/account_quota_recovery.go" {
							t.Errorf("%s runs recovery policy outside the locked transaction", path)
						}
					}
					if sel.Sel.Name == "ApplyQuotaRecovery" && path != "../application/account/build_detect.go" && path != "../application/account/service.go" && path != "../application/account/quota.go" && path != "../application/selector/selector.go" && path != "../testsupport/account.go" {
						t.Errorf("%s bypasses recovery observation owners", path)
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
		t.Fatalf("recovery transition consumers=%d", transitions)
	}
}
