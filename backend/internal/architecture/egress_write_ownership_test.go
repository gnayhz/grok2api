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

// Configuration writes belong to M14; M13 has only read and conditional
// observation ports. Keep removed wide/state writers from returning as a
// convenient fallback around the current binding or routing contracts.
func TestEgressConfigurationAndRuntimeHaveSeparateWriters(t *testing.T) {
	removed := map[string]bool{"ProbeProxyURL": true, "decryptNodeProxyURL": true, "UpsertEgressNodesFromSource": true, "UpdateEgressSourceSync": true, "UpdateEgressNode": true, "UpdateEgressNodeHealth": true, "UpdateEgressNodeClearance": true, "UpdateEgressNodeLastError": true, "UpdateEgressNodeQualityState": true}
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
		source := filepath.ToSlash(path)
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.TypeSpec:
				if value.Name.Name == "OperationsConfigCASWriter" {
					t.Errorf("%s restores optional hygiene CAS capability", source)
				}
			case *ast.FuncDecl:
				if removed[value.Name.Name] {
					t.Errorf("%s restores removed network writer %s", source, value.Name.Name)
				}
			case *ast.CallExpr:
				selected, ok := value.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				method := selected.Sel.Name
				if method == "SaveEgressOperationsConfig" && source != "../application/egress/operations.go" {
					t.Errorf("%s bypasses conditional hygiene commit via ordinary config save", source)
				}
				if removed[method] {
					t.Errorf("%s still calls obsolete network writer %s", source, method)
				}
				if method == "BeginEgressSourceSync" || method == "CommitEgressSourceSync" || method == "FailEgressSourceSync" {
					consumers[method]++
					if source != "../application/egress/sync.go" {
						t.Errorf("%s bypasses source sync owner via %s", source, method)
					}
				}
				if method == "UpdateEgressNodeConfiguration" || method == "SaveEgressOperationsConfig" || method == "SaveEgressOperationsConfigIfCurrent" {
					consumers[method]++
					if !strings.HasPrefix(source, "../application/egress/") {
						t.Errorf("%s bypasses M14 configuration policy via %s", source, method)
					}
					if len(value.Args) == 0 {
						return true
					}
					if policy, ok := value.Args[len(value.Args)-1].(*ast.Ident); ok && policy.Name == "nil" {
						t.Errorf("%s passes no fixed-target policy to %s", source, method)
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
	for _, method := range []string{"BeginEgressSourceSync", "CommitEgressSourceSync", "FailEgressSourceSync", "UpdateEgressNodeConfiguration", "SaveEgressOperationsConfig", "SaveEgressOperationsConfigIfCurrent"} {
		if consumers[method] == 0 {
			t.Errorf("missing current configuration consumer: %s", method)
		}
	}
}
