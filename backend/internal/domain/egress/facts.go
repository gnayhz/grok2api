package egress

import "time"

// NodeFacts is the credential-free operational projection. Transport owns the
// fixed-target capability; consumers keep their own evidence/epoch policies.
type NodeFacts struct {
	ID                  uint64
	Name                string
	Enabled             bool
	ProxyPool           bool
	RotationEnabled     bool
	CanServeFixedTarget bool
	CooldownUntil       *time.Time
	ExitIP              string
	// ExitIPv4/ExitIPv6 是最近一次探活的分族出口地址,供质量层做双族
	// 身份判定(与轮换验证同一把尺子);空串表示该族未观测。
	ExitIPv4      string
	ExitIPv6      string
	ProbeRevision uint64
}
