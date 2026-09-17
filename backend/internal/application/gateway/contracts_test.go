package gateway

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestResultDeliveryHooksAreExportedContract(t *testing.T) {
	var result Result
	_ = result.BeginDelivery
	_ = result.CommitDelivery
	_ = result.CommitCompletion
	_ = result.Finalize
}

func TestPhysicalAttemptBudgetBelongsToAttemptResources(t *testing.T) {
	var fn func(*Service, context.Context, provider.ResponseResourceRequest, *selector.AttemptResources) (*provider.Response, error)
	fn = (*Service).runPhysicalAttempt
	_ = fn
}
