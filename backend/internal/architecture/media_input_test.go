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

func TestPersistedVideoInputHasOneInterpreter(t *testing.T) {
	removed := map[string]bool{"encodeVideoInput": true, "encodeVideoInputFull": true, "decodeVideoInput": true, "decodeVideoInputParts": true, "decodeVideoInputFull": true, "decodeVideoInputDetailed": true, "decodeVideoOperation": true, "videoInputReferences": true}
	consumers := map[string]bool{}
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
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
		ast.Inspect(tree, func(node ast.Node) bool {
			if fn, ok := node.(*ast.FuncDecl); ok && removed[fn.Name.Name] {
				t.Errorf("%s restored old input interpreter %s", path, fn.Name.Name)
			}
			if call, ok := node.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "DecodeVideoInput" {
					if strings.Contains(filepath.ToSlash(path), "/application/gateway/") {
						consumers["execution"] = true
					}
					if strings.Contains(filepath.ToSlash(path), "/infra/persistence/relational/") {
						consumers["persistence"] = true
					}
				}
			}
			// These are persistence fields, not public HTTP/Provider DTOs. Keep their
			// JSON interpretation in M17; the old Gateway struct is no longer allowed.
			if field, ok := node.(*ast.Field); ok && field.Tag != nil && strings.Contains(field.Tag.Value, `json:"reference_urls"`) && !strings.Contains(filepath.ToSlash(path), "/domain/media/") {
				t.Errorf("%s duplicates persisted video input shape", path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !consumers["execution"] || !consumers["persistence"] {
		t.Fatalf("input interpreter has no real consumers: %v", consumers)
	}
}
