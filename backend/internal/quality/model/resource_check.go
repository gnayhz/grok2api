package model

import "time"

const ResourceCheckVersion = "resource-proof-v2"
const LegacyResourceCheckVersion = "resource-quality-check-v1"
const ProbeResourceCheck ProbeDirection = "resource_check"
const ProbeCaseProof ProbeDirection = "case_proof"
const CaseProofVersion = "resource-proof-case-v1"
const ResourceCheckMaxAccounts = 8
const ResourceCheckMaxNodes = 4
const ResourceCheckTimeout = 12 * time.Minute
const ResourceCheckQueueTimeout = 4 * time.Hour
const ResourceCheckWindow = 180 * time.Second

// Candidate membership is never evidence of health.
type ResourceCheckPlan struct {
	DeadlineAt        time.Time        `json:"deadline_at,omitempty"`
	UnavailableReason string           `json:"unavailable_reason,omitempty"`
	Kind              string           `json:"kind"`
	ResourceID        uint64           `json:"resource_id"`
	Targets           []ResourceTarget `json:"targets"`
	Accounts          []uint64         `json:"accounts"`
	Nodes             []uint64         `json:"nodes"`
	MaxCalls          int              `json:"max_calls"`
	Seed              uint64           `json:"seed"`
}
type ResourceTarget struct {
	Kind              string `json:"kind"`
	ResourceID        uint64 `json:"resource_id"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}
type ResourceSubmission struct {
	ResourceID uint64 `json:"resource_id"`
	ID         uint64 `json:"id,omitempty"`
	Error      string `json:"error,omitempty"`
}
type ResourceObservation struct {
	ID            int                `json:"id"`
	Window        int                `json:"window"`
	AccountID     uint64             `json:"account_id"`
	NodeID        uint64             `json:"node_id"`
	IdentityGroup uint64             `json:"identity_group"`
	Purpose       string             `json:"purpose"`
	Class         string             `json:"class"`
	StartedAt     time.Time          `json:"started_at"`
	FinishedAt    time.Time          `json:"finished_at"`
	Sample        AccountCheckSample `json:"sample"`
}
type ResourceProof struct {
	ResourceTarget
	IdentityGroup uint64    `json:"identity_group"`
	Outcome       string    `json:"outcome"`
	Reason        string    `json:"reason"`
	Rule          string    `json:"rule,omitempty"`
	Evidence      []int     `json:"evidence"`
	Window        int       `json:"window"`
	ValidUntil    time.Time `json:"valid_until"`
}
type ResourceCheckGroup struct {
	ControlAccount uint64               `json:"control_account"`
	ControlNode    uint64               `json:"control_node"`
	AccountID      uint64               `json:"account_id"`
	NodeID         uint64               `json:"node_id"`
	IdentityGroup  uint64               `json:"identity_group"`
	Control        []AccountCheckSample `json:"control"`
	Samples        []AccountCheckSample `json:"samples"`
	After          *AccountCheckSample  `json:"after,omitempty"`
	Outcome        string               `json:"outcome"`
	Reason         string               `json:"reason"`
	ControlDelta   int64                `json:"control_delta"`
	Delta          int64                `json:"delta"`
}

// Groups decode historical reports only; v2 never executes the old protocol.
type ResourceCheckReport struct {
	Version         string                `json:"version"`
	Kind            string                `json:"kind"`
	ResourceID      uint64                `json:"resource_id"`
	Outcome         string                `json:"outcome"`
	Reason          string                `json:"reason"`
	Calls           int                   `json:"calls"`
	MaxCalls        int                   `json:"max_calls"`
	Revision        uint64                `json:"revision"`
	Window          int                   `json:"window"`
	WindowStartedAt time.Time             `json:"window_started_at"`
	Generations     int                   `json:"generations"`
	PathChecks      int                   `json:"path_checks"`
	Observations    []ResourceObservation `json:"observations"`
	Results         []ResourceProof       `json:"results"`
	Groups          []ResourceCheckGroup  `json:"groups"`
}
type ResourceCheck struct {
	ID         uint64               `json:"id"`
	Kind       string               `json:"kind"`
	ResourceID uint64               `json:"resource_id"`
	Model      string               `json:"model"`
	State      ProbeTaskState       `json:"state"`
	CreatedAt  time.Time            `json:"created_at"`
	FinishedAt *time.Time           `json:"finished_at,omitempty"`
	Report     *ResourceCheckReport `json:"report,omitempty"`
}

func IsManualProbe(direction ProbeDirection) bool {
	return direction == ProbeAccountCheck || direction == ProbeResourceCheck
}
