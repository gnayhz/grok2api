package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// 出口调度的三条策略只能由 internal/domain/egress 定义:池模式判定
// (IsPoolMode/Node.IsPoolModeNode)、冷却豁免与质量隔离
// (CooldownBlocksScheduling)、旋转端点健康投影 (RotatingEndpointHealth)。
// 基础设施层与应用层曾各自复制这三条规则,其中固定目标副本整段绕过出口 IP
// 质量隔离,同一份状态在池成员/自动调度里不可用、在固定目标上却可调度。
//
// 规则按"必须委托 domain"表述而不是"禁止某种写法":调用点保留各自的
// 快照、记忆化解密与明文 URL 取数,但策略判断本身必须调用 domain 唯一实现。
// 这样新增调用路径时,漏接统一口径会直接失败。
func TestEgressSchedulingPolicyDelegatesToDomain(t *testing.T) {
	required := []struct {
		path string
		fn   string
		want []string
	}{
		{"../infra/egress/routing_runtime.go", "isProxyPoolNode", []string{"domain.IsPoolMode"}},
		{"../infra/egress/routing_runtime.go", "isProxyPoolNodeDirect", []string{"domain.IsPoolMode"}},
		{"../infra/egress/routing_runtime.go", "snapshotProxyPoolFlag", []string{"domain.IsPoolMode"}},
		{"../infra/egress/routing_runtime.go", "acquire", []string{"domain.CooldownBlocksScheduling", "domain.RotatingEndpointHealth"}},
		{"../infra/egress/routing_runtime.go", "acquireFixedTarget", []string{"domain.CooldownBlocksScheduling"}},
		{"../infra/egress/pool.go", "appendPoolCandidates", []string{"domain.CooldownBlocksScheduling"}},
		{"../infra/egress/manager.go", "leaseForNodeWithOptions", []string{"IsPoolModeNode"}},
		{"../application/egress/service.go", "publicNode", []string{"domain.IsPoolMode", "domain.RotatingEndpointHealth"}},
		{"../application/egress/probe_dead.go", "markProbeDead", []string{"domain.IsPoolMode"}},
	}
	for _, rule := range required {
		t.Run(rule.fn, func(t *testing.T) {
			calls := calledNamesInFunction(t, rule.path, rule.fn)
			for _, want := range rule.want {
				if !calls[want] {
					t.Errorf("%s:%s must delegate the policy to domain via %s", rule.path, rule.fn, want)
				}
			}
		})
	}
}

// calledNamesInFunction 收集一个函数体内出现过的调用名:包级调用记录
// "pkg.Func",方法调用记录裸方法名(接收者可变)。
func calledNamesInFunction(t *testing.T, path, fn string) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]bool{}
	found := false
	for _, declaration := range file.Decls {
		decl, ok := declaration.(*ast.FuncDecl)
		if !ok || decl.Name.Name != fn || decl.Body == nil {
			continue
		}
		found = true
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				calls[fun.Sel.Name] = true
				if pkg, ok := fun.X.(*ast.Ident); ok {
					calls[pkg.Name+"."+fun.Sel.Name] = true
				}
			case *ast.Ident:
				calls[fun.Name] = true
			}
			return true
		})
	}
	if !found {
		t.Fatalf("%s: function %s not found; the architecture rule needs updating", path, fn)
	}
	return calls
}
