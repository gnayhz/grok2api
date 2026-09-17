package model

// 本文件是登记在 test_seams_test.go 冻结清单中的跨包测试接缝:
// CompatibilityAliases 把 M05 的别名声明只读暴露给 console 适配层测试
// 做完整性对账;生产解析一律走 ResolveCompatibilityAlias。
func CompatibilityAliases() []CompatibilityAlias {
	return append([]CompatibilityAlias(nil), aliases...)
}
