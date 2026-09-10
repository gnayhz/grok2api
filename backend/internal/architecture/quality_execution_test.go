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

func TestCompositionDoesNotOwnProbeExecution(t *testing.T) {
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
			if typ, ok := n.(*ast.TypeSpec); ok && typ.Name.Name == "qualityProbeExecutor" {
				t.Errorf("%s reinstates the retired composition executor", path)
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "ProbeAccountDifferentialOnPath", "ProbeExitJury", "ProbeIdentityMatches", "WithProbeExperiment", "WithProbeIdentity", "UnsupportedReason", "ReclaimRunningProbes", "ReclaimStaleRunningProbes", "CancelOrphanProbes", "Table", "AutoMigrate", "Exec", "Raw":
				t.Errorf("%s owns probe execution policy through %s", path, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
