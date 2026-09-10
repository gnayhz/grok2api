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

// Inference consumers receive the egress wrapper so they cannot bypass the
// logical request's budget or the physical observation/close lifecycle.
// Standalone SSO probes have a separate lifecycle and are outside this rule.
func TestInferenceWebSocketsUseNetworkOwner(t *testing.T) {
	for _, root := range []string{"../application/gateway", "../infra/provider", "../transport/http/inference"} {
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
			imports := map[string]string{}
			for _, spec := range file.Imports {
				name, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if name != "github.com/bogdanfinn/websocket" && name != "github.com/gorilla/websocket" {
					continue
				}
				alias := "websocket"
				if spec.Name != nil {
					alias = spec.Name.Name
				}
				if alias == "." {
					t.Errorf("%s: websocket dot import bypasses network ownership check", path)
				}
				imports[alias] = name
			}
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				imported := imports[ident.Name]
				if imported == "" {
					return true
				}
				switch selector.Sel.Name {
				case "Dialer", "DefaultDialer", "NewClient":
					t.Errorf("%s: inference websocket dialing belongs to infra/egress", path)
				case "Conn":
					if imported == "github.com/bogdanfinn/websocket" || root != "../transport/http/inference" {
						t.Errorf("%s: upstream connection must retain infra/egress observations", path)
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
