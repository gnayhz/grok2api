package streamidle

// 本文件是登记在 internal/architecture/test_seams_test.go 冻结清单中的
// 跨包测试接缝:TimedOut 只服务 cli 侧生命周期测试读取不可变超时状态;
// 生产消费方经 Read 错误感知超时,不轮询本方法。
func (r *ReadCloser) TimedOut() bool { return r.timedOut.Load() }
