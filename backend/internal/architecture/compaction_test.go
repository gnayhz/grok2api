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

// The Provider may parse its summary generation protocol, but it cannot own a
// second portable-history codec or independently authorize a loss of input.
func TestCompactionHistoryHasOneOwnerAndExplicitController(t *testing.T) {
	codecs, plans, authorizations, controllers := 0, 0, 0, 0
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
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
		path = filepath.ToSlash(path)
		ast.Inspect(file, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.Ident:
				switch v.Name {
				case "expandGatewayCompactionHistory", "gatewayCompactionCodec", "gatewayCompactionEnvelope", "HistoryRecoveryController", "RecoverHistory":
					t.Errorf("%s restores removed compaction/recovery entry %s", path, v.Name)
				}
			case *ast.TypeSpec:
				switch v.Name.Name {
				case "CompactionCodec":
					codecs++
					if path != "../domain/history/compaction.go" {
						t.Errorf("%s owns a duplicate compaction codec", path)
					}
				case "CompactionPreparation":
					plans++
					if path != "../domain/history/compaction.go" {
						t.Errorf("%s owns a duplicate compaction plan", path)
					}
				}
			case *ast.FuncDecl:
				if v.Name.Name == "PrepareInput" {
					authorizations++
					if path != "../application/gateway/history_recovery.go" {
						t.Errorf("%s authorizes input history outside M04", path)
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := v.Key.(*ast.Ident); ok && key.Name == "HistoryControl" {
					controllers++
					if path != "../application/gateway/response_execution.go" {
						t.Errorf("%s substitutes a logical history controller", path)
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
	if codecs != 1 || plans != 1 || authorizations != 1 || controllers != 1 {
		t.Fatalf("owner coverage codecs=%d plans=%d authorizations=%d controllers=%d", codecs, plans, authorizations, controllers)
	}
}
