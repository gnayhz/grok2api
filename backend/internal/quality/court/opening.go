package court

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func (s *Service) isCurrentExitEpoch(key model.EpochKey) bool {
	return key.NodeID != 0 && s.registry.CurrentEpoch(key.NodeID) == key.Epoch
}

// marshalOpeningEvidence stores only stable identifiers and the original
// epoch. Raw IPs, credentials, and provider responses never enter the case.
func marshalOpeningEvidence(defendant uint64, exit model.EpochKey) (string, error) {
	payload := map[string]any{
		"trigger":   "traffic_degraded",
		"defendant": defendant,
		"exit":      map[string]uint64{"node": exit.NodeID, "epoch": exit.Epoch},
	}
	return marshalEvidence(payload)
}

func marshalEvidence(payload map[string]any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("serialize quality evidence: %w", err)
	}
	return string(raw), nil
}

// decodeEvidence preserves exact seeds, IDs and revisions when annotating an
// existing envelope. float64 would change the frozen experiment above 2^53.
func decodeEvidence(raw string, into *map[string]any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(into)
}
