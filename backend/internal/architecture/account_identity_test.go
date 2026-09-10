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

// Account tables belong to M07. M16 consumes account/selector fact ports and
// owns only investigation state, never the account SQL schema.
func TestQualityDoesNotReadAccountTables(t *testing.T) {
	err := filepath.WalkDir("../quality", func(path string, entry fs.DirEntry, err error) error {
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
			for _, table := range []string{"account_provider_links", "web_console_account_links", "provider_accounts", "account_credentials", "account_model_capabilities", "account_billing_snapshots", "account_quota_recoveries"} {
				if strings.Contains(value, table) {
					t.Errorf("%s reads account-owned table %s", path, table)
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
