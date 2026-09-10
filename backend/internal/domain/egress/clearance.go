package egress

import "time"

// ClearanceUpdate is conditional on both configuration and the previously
// observed clearance generation. Concurrent solves cannot overwrite one another.
type ClearanceUpdate struct {
	NodeID             uint64
	EncryptedProxyURL  string
	BindingRevision    uint64
	ExpectedRevision   uint64
	EncryptedCookie    string
	UserAgent          string
	Fingerprint        string
	BindingFingerprint string
	RefreshedAt        time.Time
}
