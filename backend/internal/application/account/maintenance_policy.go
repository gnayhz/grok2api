package account

import (
	"sync"
	"time"
)

// maintenancePolicy 拥有维护策略的全部可变状态:自动清理配置与其 revision、
// 唤醒通道与 bot-risk 调度豁免开关。Service 只经组件方法访问;
// 定时器/worker 逻辑仍在 auto_clean.go,状态与锁归一个组件。
type maintenancePolicy struct {
	mu                sync.RWMutex
	autoClean         AutoCleanConfig
	autoCleanRevision uint64
	autoCleanWake     chan struct{}
	excludeBuildBot   bool
}

func newMaintenancePolicy() *maintenancePolicy {
	return &maintenancePolicy{
		autoClean: AutoCleanConfig{
			Enabled: false, Interval: 10 * time.Minute, MinAge: time.Hour, IncludeDisabled: false,
		},
		autoCleanWake: make(chan struct{}, 1),
	}
}

// updateAutoClean 安装归一化后的配置;变化时递增 revision 并唤醒 worker。
func (p *maintenancePolicy) updateAutoClean(value AutoCleanConfig) {
	p.mu.Lock()
	if p.autoClean == value {
		p.mu.Unlock()
		return
	}
	p.autoClean = value
	p.autoCleanRevision++
	p.mu.Unlock()
	select {
	case p.autoCleanWake <- struct{}{}:
	default:
	}
}

func (p *maintenancePolicy) snapshot() (AutoCleanConfig, uint64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.autoClean, p.autoCleanRevision
}

func (p *maintenancePolicy) wakeChan() chan struct{} { return p.autoCleanWake }

func (p *maintenancePolicy) setExcludeBuildBot(value bool) {
	p.mu.Lock()
	p.excludeBuildBot = value
	p.mu.Unlock()
}

func (p *maintenancePolicy) excludeBuildBotFlagged() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.excludeBuildBot
}
