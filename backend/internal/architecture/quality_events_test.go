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

func TestCompositionAdaptsQualityReceiptsWithoutIncidentPolicy(t *testing.T) {
	err := filepath.WalkDir("../app", func(path string, entry fs.DirEntry, err error) error {
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
			if typ, ok := n.(*ast.TypeSpec); ok && typ.Name.Name == "qualityObserverAdapter" {
				t.Errorf("%s restores the retired lossy incident path", path)
			}
			if literal, ok := n.(*ast.CompositeLit); ok {
				if sel, ok := literal.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Observation" {
					t.Errorf("%s constructs incident evidence in composition", path)
				}
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "ReportDegradedObservation", "NewAsyncRecorder", "TryRecord":
						t.Errorf("%s owns incident consumption through %s", path, sel.Sel.Name)
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
}
