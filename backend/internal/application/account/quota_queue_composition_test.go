package account_test

import (
	"context"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	netbudget "github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// External tests act as a composition root, just as application wiring does.
// Package account's production code and internal fixtures see a Provider port.
func init() {
	accountapp.NewQuotaQueueWebFixture = func(db *relational.Database, cipher security.Cryptor, baseURL string) (provider.Adapter, func(context.Context) error) {
		network := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
		adapter := web.NewAdapter(web.Config{BaseURL: baseURL, StatsigMode: "manual", StatsigManualValue: "synthetic"}, network, cipher, nil, nil)
		return adapter, network.Close
	}
}
