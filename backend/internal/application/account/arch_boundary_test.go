package account

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAccountLayerStaysDecoupledFromEgress 是业务与代理解耦的架构守卫:
// 账号域(domain/account 与 application/account)不得导入出口层
// (application/egress / infra/egress)。代理必须在请求即将离开系统时介入,
// 账号层的任何出口依赖都是职责越界的回归(历史上账号层曾为
// SanitizeCloudflareCookies 导入出口应用包, 该净化已移至 pkg/cfcookies)。
func TestAccountLayerStaysDecoupledFromEgress(t *testing.T) {
	roots := []string{
		filepath.Join("..", "..", "domain", "account"),
		".",
	}
	forbidden := []string{
		"github.com/chenyme/grok2api/backend/internal/application/egress",
		"github.com/chenyme/grok2api/backend/internal/infra/egress",
	}
	violations := []string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				return walkErr
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if parseErr != nil {
				t.Errorf("parse %s: %v", path, parseErr)
				return nil
			}
			for _, imported := range file.Imports {
				importPath := strings.Trim(imported.Path.Value, `"`)
				for _, banned := range forbidden {
					if importPath == banned {
						violations = append(violations, entry.Name()+" imports "+banned)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(violations) > 0 {
		t.Fatalf("account layer must not depend on the egress layer:\n%s", strings.Join(violations, "\n"))
	}
}
