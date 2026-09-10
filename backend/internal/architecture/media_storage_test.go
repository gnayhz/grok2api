package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The storage implementation owns file descriptors and temporary paths. Media
// use cases may classify os.ErrNotExist, but must consume the storage port.
func TestMediaStorageOwnsFilesystemHandles(t *testing.T) {
	entries, err := os.ReadDir("../application/media")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		set := token.NewFileSet()
		tree, err := parser.ParseFile(set, filepath.Join("../application/media", entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		aliases := make(map[string]bool)
		for _, spec := range tree.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			if path == "os" {
				name := "os"
				if spec.Name != nil {
					name = spec.Name.Name
				}
				aliases[name] = true
			}
		}
		ast.Inspect(tree, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || !aliases[pkg.Name] {
				return true
			}
			switch selector.Sel.Name {
			case "Open", "OpenFile", "Create", "CreateTemp", "ReadFile", "WriteFile", "Remove", "RemoveAll", "Rename", "Link", "Mkdir", "MkdirAll":
				t.Errorf("media use case owns filesystem operation at %v: os.%s", set.Position(call.Pos()), selector.Sel.Name)
			}
			return true
		})
	}
}
