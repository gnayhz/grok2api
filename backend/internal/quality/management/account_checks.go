package management

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

var ErrCheckQueueFull = model.ErrCheckQueueFull

type AccountCheckStore interface {
	CreateAccountCheck(context.Context, model.ProbeTask, int) (uint64, error)
	ListAccountChecks(context.Context, uint64) ([]model.AccountCheck, error)
}

type AccountCheckPreparer interface {
	PrepareAccountCheck(context.Context, uint64, string) (model.ProbeExperiment, error)
}

// AccountChecks owns manual check submission. Accepted tasks are durable and
// execute under the investigator's lifetime, independently of a browser poll.
type AccountChecks struct {
	store   AccountCheckStore
	prepare AccountCheckPreparer
}

func NewAccountChecks(store AccountCheckStore, prepare AccountCheckPreparer) *AccountChecks {
	return &AccountChecks{store: store, prepare: prepare}
}

func (s *AccountChecks) Start(ctx context.Context, accountID uint64, publicModel string) (uint64, error) {
	publicModel = strings.TrimSpace(publicModel)
	if accountID == 0 || publicModel == "" || len(publicModel) > 200 {
		return 0, errors.New("account and model required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	experiment, err := s.prepare.PrepareAccountCheck(ctx, accountID, publicModel)
	if err != nil {
		return 0, err
	}
	return s.store.CreateAccountCheck(ctx, model.ProbeTask{Direction: model.ProbeAccountCheck, DefendantAccountID: accountID, Experiment: experiment}, model.AccountCheckQueueLimit)
}

func (s *AccountChecks) List(ctx context.Context, accountID uint64) ([]model.AccountCheck, error) {
	if accountID == 0 {
		return nil, errors.New("account required")
	}
	return s.store.ListAccountChecks(ctx, accountID)
}
