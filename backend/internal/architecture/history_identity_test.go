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

func TestHistoryIdentityPolicyAndStateHaveOneOwner(t *testing.T) {
	resolvers, signals, scopes, legacy, transports := 0, 0, 0, 0, 0
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
		owned := strings.HasPrefix(path, "../application/history/") || strings.HasPrefix(path, "../domain/history/")
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.BasicLit:
				if v.Kind != token.STRING {
					break
				}
				value, err := strconv.Unquote(v.Value)
				if err != nil {
					break
				}
				for _, prefix := range []string{"grok2api:build-session:", "grok2api:build-affinity:", "grok2api:build-replay:", "grok2api:build-soft-", "grok2api:build-composer-", "grok2api:reasoning-replay:", "aux:claude-title:", "codex:window:", "g2a:identity:1:"} {
					if strings.Contains(value, prefix) && !owned {
						t.Errorf("%s encodes history identity outside its owner", path)
					}
				}
			case *ast.Ident:
				switch v.Name {
				case "PromptCacheSeed", "resolveBuildSessionIdentity", "resolveBuildSessionIdentityWithAnchors", "ensureBuildComposerSessionIdentity", "softConversations", "extractPromptCacheSeed":
					t.Errorf("%s restores removed identity policy %s", path, v.Name)
				}
			case *ast.TypeSpec:
				if v.Name.Name == "IdentityResolver" {
					resolvers++
					if path != "../application/history/identity.go" {
						t.Errorf("%s owns session resolver", path)
					}
				}
				if v.Name.Name == "softConversationRegistry" && path != "../application/history/soft_conversation_registry.go" {
					t.Errorf("%s owns soft history hints", path)
				}
			case *ast.FuncDecl:
				switch v.Name.Name {
				case "ResolveClientSeed":
					signals++
					if path != "../domain/history/client_signals.go" {
						t.Errorf("%s chooses client identity", path)
					}
				case "ReplayScope":
					scopes++
					if path != "../domain/history/identity.go" {
						t.Errorf("%s constructs durable scope", path)
					}
				case "LegacyReplayScopes":
					legacy++
					if path != "../domain/history/identity.go" {
						t.Errorf("%s constructs legacy scope", path)
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := v.Key.(*ast.Ident); ok && key.Name == "SessionSignals" && strings.HasPrefix(path, "../transport/") {
					transports++
					call, ok := v.Value.(*ast.CallExpr)
					if !ok {
						t.Errorf("%s constructs unparsed session identity", path)
					} else if f, ok := call.Fun.(*ast.Ident); !ok || f.Name != "extractClientSignals" {
						t.Errorf("%s chooses session instead of supplying protocol facts", path)
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
	if resolvers != 1 || signals != 1 || scopes != 1 || legacy != 1 || transports != 3 {
		t.Fatalf("owner coverage resolver=%d signals=%d scope=%d legacy=%d HTTP=%d", resolvers, signals, scopes, legacy, transports)
	}
}
