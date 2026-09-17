package model

import "time"

// EvidenceConfig 是证据策略配置；统计窗与观测保留期独立。
type EvidenceConfig struct {
	// Window 统计滑窗( court 评估消费的聚合窗口)。
	Window time.Duration
	// Retention 观测保留期(滚动清理)。
	Retention time.Duration
	// MinWitnessObs 见证资格的最低他处观测量(互证过滤)。
	MinWitnessObs int
}

// DefaultEvidenceConfig 使用 30 分钟统计窗和 7 天观测保留期。
func DefaultEvidenceConfig() EvidenceConfig {
	return EvidenceConfig{Window: 30 * time.Minute, Retention: 7 * 24 * time.Hour, MinWitnessObs: 1}
}

func (c EvidenceConfig) Normalized() EvidenceConfig {
	if c.Window <= 0 {
		c.Window = 30 * time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 7 * 24 * time.Hour
	}
	if c.MinWitnessObs <= 0 {
		c.MinWitnessObs = 1
	}
	return c
}
