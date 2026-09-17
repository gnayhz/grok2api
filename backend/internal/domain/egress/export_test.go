package egress

// 本文件保存只服务本包测试的接缝：这些符号包装同包未导出状态，
// 生产路径不再调用它们，因此从生产文件移到这里——Go 的 _test.go
// 只对本包测试可见，既能保持测试可直接断言内部状态，又不会让
// 生产包的导出面/test-only 代码继续增长。

// RequestScopes 是测试专用接缝（原生产文件定义）。
func RequestScopes() []Scope {
	return []Scope{ScopeBuild, ScopeWeb, ScopeConsole, ScopeWebAsset, ScopeConsoleAsset}
}
