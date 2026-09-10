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

func TestMediaDeletionAndHandoffKeepTheirOwners(t *testing.T) {
	removed := map[string]bool{"SetMediaJobRepository": true, "CountActiveMediaJobsByClientKeys": true, "DeleteTerminalMediaJobsByClientKeys": true}
	markers := 0
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
					t.Errorf("%s restored split/non-atomic deletion entry %s", path, value.Name.Name)
				}
			case *ast.CallExpr:
				sel, ok := value.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if removed[sel.Sel.Name] {
					t.Errorf("%s restored removed deletion consumer %s", path, sel.Sel.Name)
				}
				if sel.Sel.Name == "MarkMediaJobUsageRecorded" {
					markers++
					if !strings.Contains(source, "/application/gateway/") {
						t.Errorf("%s acknowledges usage outside the completion coordinator", path)
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
	if markers == 0 {
		t.Fatal("no production media usage handoff inspected")
	}
}
