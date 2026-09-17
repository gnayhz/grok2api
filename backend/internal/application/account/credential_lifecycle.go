package account

// credentialLifecycle 持有凭据刷新生命周期的可变状态:单飞分组与调度
// 唤醒通道。Service 与调度器直接访问这两个字段。
type credentialLifecycle struct {
	group OperationGroup[string]
	wake  chan struct{}
}

func newCredentialLifecycle() *credentialLifecycle {
	return &credentialLifecycle{wake: make(chan struct{}, 1)}
}
