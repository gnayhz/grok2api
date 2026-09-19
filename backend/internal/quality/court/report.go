package court

// Case reports project the shared resource proof and independent party decisions.
import (
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type ExperimentReport struct {
	Proof          *model.ResourceCheckReport `json:"proof,omitempty"`
	Policy         ExperimentPolicy           `json:"policy"`
	Verdict        model.Verdict              `json:"verdict"`
	Reason         string                     `json:"reason"`
	Limitations    []string                   `json:"limitations"`
	Phase          string                     `json:"phase"`
	AccountCleared bool                       `json:"account_cleared"`
	ExitCleared    bool                       `json:"exit_cleared"`
}

func appendUnique(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}
