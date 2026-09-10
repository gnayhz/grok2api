package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// HTTP may invoke management queries, Court commands, Guard configuration and
// Enforcement commands. It must not regain storage access or evidence policy.
func TestQualityManagementHTTPUsesUseCases(t *testing.T) {
	err := filepath.WalkDir("../transport/http/quality", func(path string, entry fs.DirEntry, err error) error {
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
		for _, imp := range file.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.Contains(name, "/quality/registry") || strings.Contains(name, "/quality/evidence") || strings.Contains(name, "/quality/model") || strings.HasPrefix(name, "gorm.io/") || name == "database/sql" || name == "sort" {
				t.Errorf("%s imports query storage or policy dependency %s", path, name)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "DB", "ListRecentCases", "ListOpenCases", "ListParties", "ListNodeIPArchives", "ListProbeTasks", "ListProbeTasksForCase", "LiveCaseViews", "SnapshotWindow", "PairStats", "CurrentExitStates", "AccountState", "ManagementState", "TransitionAccount", "TransitionExit", "Evaluate":
				t.Errorf("%s bypasses management use case with %s", path, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
