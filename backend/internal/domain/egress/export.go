package egress

// 本文件是登记在 internal/architecture/test_seams_test.go 冻结清单中的
// 跨包测试接缝:TrafficClasses 把可调度交通类按稳定顺序暴露给存储层
// 与域测试做逐类行为对账;生产调度按类名索引,不遍历本清单。
func TrafficClasses() []TrafficClass {
	return []TrafficClass{TrafficClassInference, TrafficClassCredential, TrafficClassBilling, TrafficClassModelSync, TrafficClassVideo, TrafficClassProbe}
}
