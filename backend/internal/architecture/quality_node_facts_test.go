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

// M16 and the composition root consume node facts through M13, not a second
// SQL schema projection whose missing/error behavior silently becomes policy.
func TestQualityAndCompositionDoNotReadEgressTables(t *testing.T) {
	for _, root := range []string{"../quality", "../app"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
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
				literal, ok := n.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				for _, table := range []string{"egress_nodes", "egress_pools", "egress_pool_members", "egress_subscription_sources"} {
					if strings.Contains(value, table) {
						t.Errorf("%s reads transport-owned table %s", path, table)
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
}
