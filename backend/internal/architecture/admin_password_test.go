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

func TestAdminPasswordProofOwnsSessionAndPasswordWrites(t *testing.T) {
	consumers := map[string]int{}
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
		ast.Inspect(file, func(n ast.Node) bool {
			if decl, ok := n.(*ast.FuncDecl); ok && decl.Name.Name == "Create" && decl.Recv != nil {
				for _, field := range decl.Recv.List {
					ptr, ok := field.Type.(*ast.StarExpr)
					if !ok {
						continue
					}
					if id, ok := ptr.X.(*ast.Ident); ok && id.Name == "AdminSessionRepository" {
						t.Errorf("%s restores unverified session creation", path)
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "CreateForPassword" || sel.Sel.Name == "UpdatePasswordAndRevokeSessions" || sel.Sel.Name == "RevokeByTokenHash" {
				consumers[sel.Sel.Name]++
				if !strings.HasSuffix(filepath.ToSlash(path), "/application/adminauth/service.go") {
					t.Errorf("%s owns password verification outside M02", path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CreateForPassword", "UpdatePasswordAndRevokeSessions", "RevokeByTokenHash"} {
		if consumers[name] != 1 {
			t.Errorf("%s consumers=%d", name, consumers[name])
		}
	}
}
