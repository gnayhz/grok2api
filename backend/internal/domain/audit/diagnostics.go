package audit

// ExecutionDiagnostics contains bounded local observations, never request or
// response text. Durations may be nested; they are not additive CPU timings.
// A missing field on an old audit means unobserved, not zero cost.
type ExecutionDiagnostics struct {
	Version   int                  `json:"version"`
	Stages    []StageDiagnostic    `json:"stages,omitempty"`
	Exchanges []ExchangeDiagnostic `json:"exchanges,omitempty"`
	Failures  []FailureDiagnostic  `json:"failures,omitempty"`
	Truncated bool                 `json:"truncated,omitempty"`
}

type StageDiagnostic struct {
	Stage string `json:"stage"`
	US    int64  `json:"us"`
	Calls int    `json:"calls"`
}

type FailureDiagnostic struct {
	Component string `json:"component"`
	Stage     string `json:"stage"`
	Reason    string `json:"reason"`
}

type ExchangeDiagnostic struct {
	PhysicalID string              `json:"physicalId"`
	AccountID  uint64              `json:"accountId,string"`
	NodeID     uint64              `json:"nodeId,string"`
	Epoch      uint64              `json:"epoch,string"`
	Plane      string              `json:"plane"`
	Stage      string              `json:"stage"`
	Status     int                 `json:"status"`
	US         int64               `json:"us"`
	Failed     bool                `json:"failed,omitempty"`
	Events     []NetworkDiagnostic `json:"events,omitempty"`
	Prompt     *PromptDiagnostic   `json:"prompt,omitempty"`
}

type NetworkDiagnostic struct {
	Stage   string `json:"stage"`
	US      int64  `json:"us"`
	Reused  bool   `json:"reused,omitempty"`
	Resumed bool   `json:"resumed,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
}

// Digests use a process-random HMAC key. Compare only equal key epochs. Prefixes
// cover the first and last 32 items; a missing boundary cannot prove equality.
type PromptDiagnostic struct {
	KeyEpoch     string             `json:"keyEpoch"`
	Session      string             `json:"session"`
	Instructions string             `json:"instructions"`
	Tools        string             `json:"tools"`
	Parameters   string             `json:"parameters"`
	InputItems   int                `json:"inputItems"`
	InputBytes   int                `json:"inputBytes"`
	Prefixes     []PrefixDiagnostic `json:"prefixes,omitempty"`
}

type PrefixDiagnostic struct {
	Items  int    `json:"items"`
	Digest string `json:"digest"`
}
