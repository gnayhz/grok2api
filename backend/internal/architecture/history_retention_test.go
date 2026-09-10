package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestHistoryRetentionPolicyIsNotInComposition(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../app/application.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	responseTask, journalTask := 0, 0
	ast.Inspect(file, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok {
			switch ident.Name {
			case "cleanupExpiredResponses", "recordResponseCleanup", "responseOwnershipCleanupBatchSize", "webResponseStateCleanupBatchSize", "responseCleanupMaxBatches", "responseCleanupBudget", "responseCleanupLockTTL":
				t.Errorf("composition restored history policy %s", ident.Name)
			}
		}
		if call, ok := node.(*ast.CallExpr); ok {
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				switch selector.Sel.Name {
				case "CleanupResponses":
					responseTask++
				case "CleanupConversations":
					journalTask++
				case "DeleteExpired", "Prune":
					t.Errorf("composition directly calls history retention store %s", selector.Sel.Name)
				}
			}
		}
		return true
	})
	if responseTask != 1 || journalTask != 1 {
		t.Fatalf("retention task consumers=%d/%d", responseTask, journalTask)
	}
}
