package searchresult

// 本文件是登记在 internal/architecture/test_seams_test.go 冻结清单中的
// 跨包测试接缝:MaxTitleRunes 只服务 web 工具测试构造边界长度标题;
// 生产截断行为使用同包未导出常量。
const MaxTitleRunes = maxTitleRunes
