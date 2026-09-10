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

func TestMediaImageImportHasApplicationAndNetworkOwners(t *testing.T) {
	sourceConstructors, importCalls := 0, 0
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		name := filepath.ToSlash(path)
		tree, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, im := range tree.Imports {
			target, _ := strconv.Unquote(im.Path.Value)
			if strings.Contains(name, "/transport/http/media/") && (target == "net" || target == "crypto/tls" || target == "syscall" || strings.Contains(target, "/infra/") || strings.HasSuffix(target, "/pkg/netguard")) {
				t.Errorf("%s restored outbound network ownership via %s", path, target)
			}
			if strings.Contains(name, "/infra/mediafetch/") && (strings.Contains(target, "/application/") || strings.Contains(target, "/transport/") || strings.Contains(target, "/infra/provider") || strings.Contains(target, "/infra/egress")) {
				t.Errorf("%s coupled import transport to a business/Provider owner", path)
			}
		}
		ast.Inspect(tree, func(node ast.Node) bool {
			if fn, ok := node.(*ast.FuncDecl); ok && fn.Name.Name == "fetchRemoteImage" {
				t.Errorf("%s restored removed HTTP import use case", path)
			}
			if call, ok := node.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "NewImageSource" {
						sourceConstructors++
						if !strings.Contains(name, "/app/") {
							t.Errorf("%s constructs image transport outside composition", path)
						}
					}
					if sel.Sel.Name == "FetchImage" {
						importCalls++
						if !strings.Contains(name, "/application/media/") {
							t.Errorf("%s fetches media input outside its use case", path)
						}
					}
					if strings.Contains(name, "/transport/http/media/") {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" && (sel.Sel.Name == "NewRequest" || sel.Sel.Name == "NewRequestWithContext") {
							t.Errorf("%s creates outbound request inside HTTP handler", path)
						}
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
	if sourceConstructors != 1 || importCalls != 1 {
		t.Fatalf("missing or duplicated real import ownership: constructors=%d use_cases=%d", sourceConstructors, importCalls)
	}
}
