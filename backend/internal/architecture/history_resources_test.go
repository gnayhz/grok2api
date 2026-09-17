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

func TestResponseResourcePolicyHasOneOwner(t *testing.T) {
	owners, lookups, records, forgets, nativeReads, nativeDeletes := 0, 0, 0, 0, 0, 0
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		path = filepath.ToSlash(path)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (path == "../infra/provider/web/chat.go" || path == "../infra/provider/web/chat_open.go") {
					if sel.Sel.Name == "LookupWeb" {
						nativeReads++
					}
					if sel.Sel.Name == "DeleteWeb" {
						nativeDeletes++
					}
				}
			}
			switch n := node.(type) {
			case *ast.TypeSpec:
				if n.Name.Name == "ResponseResources" {
					owners++
					if path != "../application/history/resources.go" {
						t.Errorf("resource policy owner in %s", path)
					}
				}
			case *ast.ValueSpec:
				for _, name := range n.Names {
					if name.Name == "responseOwnershipTTL" && path != "../application/history/resources.go" {
						t.Errorf("resource retention policy outside M12: %s", path)
					}
				}
			case *ast.CallExpr:
				selected, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				receiver, ok := selected.X.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if strings.HasPrefix(path, "../application/gateway/") && receiver.Sel.Name == "responses" {
					switch selected.Sel.Name {
					case "Lookup":
						lookups++
					case "Record":
						records++
					case "Forget":
						forgets++
					default:
						t.Errorf("Gateway bypasses resource use case with %s in %s", selected.Sel.Name, path)
					}
				}
				if strings.HasPrefix(path, "../infra/provider/web/") && (receiver.Sel.Name == "states" || receiver.Sel.Name == "store") && (selected.Sel.Name == "GetWebState" || selected.Sel.Name == "DeleteWebState" || selected.Sel.Name == "SaveWebState") {
					t.Errorf("Web resource encodes store policy in %s", path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if owners != 1 || lookups != 2 || records != 1 || forgets != 2 || nativeReads != 1 || nativeDeletes != 1 {
		t.Fatalf("owner=%d lookup=%d record=%d forget=%d nativeRead=%d nativeDelete=%d", owners, lookups, records, forgets, nativeReads, nativeDeletes)
	}
}
