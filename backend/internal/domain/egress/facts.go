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
	ProbeRevision       uint64
}
