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

func TestAccountHTTPConsumesCapabilitiesAndOnboarding(t *testing.T) {
	err := filepath.WalkDir("../transport/http/account", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		accounts := map[string]bool{}
		for _, imp := range file.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if name != internalPrefix+"application/account" {
				continue
			}
			alias := "account"
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			if alias == "." || alias == "_" {
				t.Errorf("%s: account capability requires an explicit package reference", path)
			}
			accounts[alias] = true
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); ok && accounts[pkg.Name] && (selector.Sel.Name == "Service" || selector.Sel.Name == "NewService") {
				t.Errorf("%s regains the whole account service", path)
			}
			switch selector.Sel.Name {
			case "ImportCredentialDocumentsWithProgress", "ImportWebCredentialDocumentsWithProgress", "ImportConsoleCredentialDocumentsWithProgress",
				"ConvertAllWebAccountsToBuildWithStrategy", "ConvertWebAccountsToBuildWithStrategy",
				"SyncAllWebAccountsToConsoleWithStrategy", "SyncWebAccountsToConsoleWithStrategy", "SyncStream", "SyncStreamObserved":
				t.Errorf("%s bypasses the onboarding use case with %s", path, selector.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
