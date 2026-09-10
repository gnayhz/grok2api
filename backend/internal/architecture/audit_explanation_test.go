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

func TestAuditHTTPConsumesOwnedExplanations(t *testing.T) {
	billing, stream := 0, 0
	err := filepath.WalkDir("../transport/http", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		tree, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		auditAliases := map[string]bool{}
		for _, im := range tree.Imports {
			name, _ := strconv.Unquote(im.Path.Value)
			if strings.HasSuffix(name, "/domain/audit") {
				alias := "audit"
				if im.Name != nil {
					alias = im.Name.Name
				}
				auditAliases[alias] = true
				if alias == "." {
					t.Errorf("%s obscures audit policy imports", path)
				}
			}
		}
		ast.Inspect(tree, func(node ast.Node) bool {
			if fn, ok := node.(*ast.FuncDecl); ok && (fn.Name.Name == "auditDegradeClass" || fn.Name.Name == "auditOutputTokensPerSecond") {
				t.Errorf("%s restores removed HTTP audit policy %s", path, fn.Name.Name)
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && auditAliases[id.Name] {
				switch sel.Sel.Name {
				case "ReconstructOfficialCost", "ClassifyTerminalBurst", "GenerationWindowMS", "OutputTokensPerSecond":
					t.Errorf("%s owns record interpretation through raw primitive %s", path, sel.Sel.Name)
				}
			}
			if strings.Contains(filepath.ToSlash(path), "/http/audit/") {
				switch sel.Sel.Name {
				case "ExplainBilling":
					billing++
				case "ObserveStream":
					stream++
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if billing != 1 || stream != 1 {
		t.Fatalf("audit DTO must share record interpretation: billing=%d stream=%d", billing, stream)
	}
}
