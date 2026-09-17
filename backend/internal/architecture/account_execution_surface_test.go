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

var accountExecutionMethods = map[string]struct{}{
	"Get": {}, "EnsureCredential": {}, "MarkReauthRequired": {}, "ObserveResponseModel": {},
	"QueueQuotaRefresh": {}, "ConsumeQuota": {}, "ProbePaidQuota": {},
	"ReconcileRateLimit": {}, "ReconcileWebRateLimit": {},
	"ActiveTeamModelRateLimit": {}, "ObserveTeamModelRateLimit": {},
}

func TestGatewayUsesAccountExecutionSurface(t *testing.T) {
	err := filepath.WalkDir("../application/gateway", func(path string, entry fs.DirEntry, err error) error {
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
		aliases := map[string]bool{}
		for _, imported := range file.Imports {
			if strings.Trim(imported.Path.Value, "\"") != internalPrefix+"application/account" {
				continue
			}
			alias := "account"
			if imported.Name != nil {
				alias = imported.Name.Name
			}
			if alias == "." || alias == "_" {
				t.Errorf("%s: account capability needs an explicit package reference", path)
			}
			aliases[alias] = true
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok {
				if pkg, ok := selector.X.(*ast.Ident); ok && aliases[pkg.Name] {
					switch selector.Sel.Name {
					case "Service", "NewService", "Administration", "CredentialTransfer", "Conversion", "Maintenance":
						t.Errorf("%s consumes account management capability %s instead of Execution", path, selector.Sel.Name)
					}
				}
			}

			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if ident.Sel.Name != "accounts" {
				return true
			}
			if _, ok := accountExecutionMethods[sel.Sel.Name]; !ok {
				t.Errorf("%s calls account.%s; gateway execution may only use account.Execution", path, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
