package relational

import (
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

// Compile the M05 product contract before pagination, so unavailable legacy
// configurations do not create short pages or incorrect active-scope totals.
// CASE selects one Provider and capability without an OR branch/index scan for
// every product family. All values remain bound SQL parameters.
func modelCapabilityPredicate() (string, []any) {
	rules := model.CapabilityRules()
	var predicate strings.Builder
	predicate.WriteString("(model_routes.upstream_model <> '' AND (CASE model_routes.provider")
	args := make([]any, 0, len(rules)*3)
	for index, rule := range rules {
		if index == 0 || rules[index-1].Provider != rule.Provider {
			if index > 0 {
				predicate.WriteString(" ELSE FALSE END")
			}
			predicate.WriteString(" WHEN ? THEN CASE model_routes.capability")
			args = append(args, rule.Provider)
		}
		predicate.WriteString(" WHEN ? THEN model_routes.upstream_model ")
		if rule.ExcludeModels {
			predicate.WriteString("NOT ")
		}
		predicate.WriteString("IN ?")
		args = append(args, rule.Capability, rule.UpstreamModels)
	}
	predicate.WriteString(" ELSE FALSE END ELSE FALSE END))")
	return predicate.String(), args
}
