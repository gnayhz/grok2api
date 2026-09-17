package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// 生产文件里存在一类跨包导出面:包装同包未导出状态,因此无法迁入 _test.go
// (Go 的测试文件只对本包测试可见)。其中一部分只服务其它包测试,一部分是
// 生产 API 的单语句转发。
//
// 两个约束让这类接缝保持诚实,不会悄悄长成第二套策略:
//  1. 冻结清单——新增或删除导出符号必须显式修改本文件,从而进入评审视野。
//  2. 形状约束——纯转发文件(selector/export.go)的每个函数只能转发到同包
//     未导出符号,不得承载状态或策略。该文件同时是 gateway 的生产导出面
//     (AttemptResources / AdmissionBody / 选号会话等)和测试接缝
//     (StickySessionKey / ReplaceAccountStore / LocalQualityAllowed)。
//
// 已迁出的接缝(同包内,已移入 _test.go):account.ExportCredentials、
// account HTTP 的 writeAccountEvent。
var testSeamSurface = map[string][]string{
	"application/gateway/guard_snapshot.go": {
		"Service.SetGuardSnapshotSource",
		"StaticGuardSnapshotSource",
	},
	"application/gateway/history_recovery.go": {
		"NewHistoryController",
	},
	"application/selector/export.go": {
		"AdmissionBody",
		"NewAdmissionBody",
		"NewAttemptResources",
		"OnceCloseBody",
		"QualityMeasurementTimeout",
		"QuotaRecoveryHints",
		"SelectionSession",
		"Selector.Account",
		"Selector.AcquireProbeResources",
		"Selector.BeginSelectionSessionForKey",
		"Selector.HasAccountStore",
		"Selector.HoldLocalQuality",
		"Selector.LocalQualityAllowed",
		"Selector.MarkSoftFailure",
		"Selector.MarkSuccessWithRecovery",
		"Selector.ProtectionStats",
		"Selector.QualityProbeCandidates",
		"Selector.ReplaceAccountStore",
		"StickySessionKey",
	},
	"infra/provider/console/catalog.go": {
		"Catalog",
		"ModelSpec",
		"QuotaMode",
		"QuotaModeImage",
		"QuotaModeVideo",
		"Resolve",
		"ResolveMedia",
	},
	"quality/guard/export.go": {
		"DefaultConfig",
		"New",
	},
	"domain/model/export.go": {
		"CompatibilityAliases",
	},
	"domain/egress/export.go": {
		"TrafficClasses",
	},
	"infra/provider/streamidle/export.go": {
		"ReadCloser.TimedOut",
	},
	"infra/provider/searchresult/export.go": {
		"MaxTitleRunes",
	},
	"infra/egress/manager_test_seam.go": {
		"Manager.Acquire",
	},
	"infra/provider/web/catalog.go": {
		"Catalog",
		"ModelSpec",
		"Resolve",
		"TierSupports",
	},
}

// forwardingOnlySeamFiles must not declare state or policy: every function is a
// one-line forward to an unexported symbol of the same package.
var forwardingOnlySeamFiles = []string{"application/selector/export.go"}

func declaredExportedSurface(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var names []string
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.GenDecl:
			// CONST/VAR 也在冻结面内:导出常量同样能成为跨包测试接缝
			// (如 selector/export.go 的 QualityMeasurementTimeout),不得
			// 只靠 TYPE/FUNC 遍历留下盲区。
			if node.Tok != token.TYPE && node.Tok != token.CONST && node.Tok != token.VAR {
				continue
			}
			for _, spec := range node.Specs {
				switch specDecl := spec.(type) {
				case *ast.TypeSpec:
					if !specDecl.Name.IsExported() {
						continue
					}
					names = append(names, specDecl.Name.Name)
				case *ast.ValueSpec:
					for _, ident := range specDecl.Names {
						if ident.IsExported() {
							names = append(names, ident.Name)
						}
					}
				}
			}
		case *ast.FuncDecl:
			if !node.Name.IsExported() {
				continue
			}
			if node.Recv == nil || len(node.Recv.List) == 0 {
				names = append(names, node.Name.Name)
				continue
			}
			receiver := receiverBaseName(node.Recv.List[0].Type)
			if receiver == "" || !ast.IsExported(receiver) {
				continue
			}
			names = append(names, receiver+"."+node.Name.Name)
		}
	}
	sort.Strings(names)
	return names
}

func receiverBaseName(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.StarExpr:
		return receiverBaseName(node.X)
	case *ast.IndexExpr:
		return receiverBaseName(node.X)
	default:
		return ""
	}
}

// TestTestSeamSurfaceIsFrozen 把"生产文件里的测试接缝"冻结成显式清单。
// 新增接缝必须登记,删除接缝必须同步清理清单,两者都要经过评审。
func TestTestSeamSurfaceIsFrozen(t *testing.T) {
	for relative, expected := range testSeamSurface {
		actual := declaredExportedSurface(t, "../"+relative)
		want := append([]string(nil), expected...)
		sort.Strings(want)
		if strings.Join(actual, "\x00") == strings.Join(want, "\x00") {
			continue
		}
		t.Errorf("%s 的导出面与接缝清单不一致。\n实际: %v\n清单: %v\n"+
			"新增测试接缝请登记到 testSeamSurface;测试接缝若能移入同包 _test.go 就应移出生产文件,不能移出则在清单中登记并说明原因。",
			relative, actual, want)
	}
}

// TestForwardingOnlySeamFilesCarryNoStateOrPolicy 约束纯接缝文件只能转发。
// selector/export.go 是其它包测试读取选号内部状态的唯一入口,一旦它开始
// 持有状态、分支或策略,就变成了第二套生产 API。
//
// 允许的形态:类型别名、转发到同包未导出常量的 const、单语句转发函数。
// 禁止的形态:新增字面量状态、多语句函数体、任何控制流。
func TestForwardingOnlySeamFilesCarryNoStateOrPolicy(t *testing.T) {
	for _, relative := range forwardingOnlySeamFiles {
		path := "../" + relative
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.VAR && gen.Tok != token.CONST) {
				continue
			}
			for _, spec := range gen.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, value := range valueSpec.Values {
					if !referencesUnexported(value) {
						t.Errorf("%s:%d 纯接缝不得声明新的包级状态;const/var 只能转发同包未导出符号",
							relative, fset.Position(valueSpec.Pos()).Line)
					}
				}
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if len(fn.Body.List) != 1 {
				t.Errorf("%s:%d %s 有 %d 条语句;纯接缝函数必须是一条转发语句",
					relative, fset.Position(fn.Pos()).Line, fn.Name.Name, len(fn.Body.List))
				continue
			}
			if !referencesUnexported(fn.Body.List[0]) {
				t.Errorf("%s:%d %s 未触达同包未导出符号,可能已承载策略",
					relative, fset.Position(fn.Pos()).Line, fn.Name.Name)
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n.(type) {
				case *ast.IfStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SelectStmt:
					t.Errorf("%s:%d %s 含控制流;纯接缝不得做决策",
						relative, fset.Position(fn.Pos()).Line, fn.Name.Name)
				}
				return true
			})
		}
	}
}

// referencesUnexported reports whether the expression touches at least one
// package-local unexported identifier or selector.
func referencesUnexported(node ast.Node) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		switch value := n.(type) {
		case *ast.Ident:
			if value.Name != "" && !ast.IsExported(value.Name) {
				found = true
			}
		case *ast.SelectorExpr:
			if value.Sel != nil && !ast.IsExported(value.Sel.Name) {
				found = true
			}
		}
		return true
	})
	return found
}
