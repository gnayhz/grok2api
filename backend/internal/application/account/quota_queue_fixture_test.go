package account

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// NewQuotaQueueWebFixture is set by the external composition test package.
// It is test-only: the account implementation cannot construct physical egress.
var NewQuotaQueueWebFixture func(*relational.Database, security.Cryptor, string) (provider.Adapter, func(context.Context) error)
