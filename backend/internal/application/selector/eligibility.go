package selector

import "context"

type AccountEligibility interface {
	AccountSchedulable(accountID uint64) bool
}

type AccountAdmission interface {
	CheckAccountAdmission(context.Context, uint64) (bool, error)
}
