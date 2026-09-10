package media

import (
	"errors"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

var ErrInvalidJobAccessPolicy = errors.New("视频任务授权快照无效")

// JobAccessPolicy retains the permission granted when a persistent request was
// accepted. Version zero is legacy missing data, not unrestricted permission.
// Account scope interpretation remains owned by the client-key domain.
type JobAccessPolicy struct {
	Version      uint8
	AccountScope clientkey.AccountScope
}

func NewJobAccessPolicy(scope clientkey.AccountScope) (JobAccessPolicy, error) {
	normalized, valid := clientkey.NormalizeAccountScope(scope)
	if !valid {
		return JobAccessPolicy{}, ErrInvalidJobAccessPolicy
	}
	return JobAccessPolicy{Version: 1, AccountScope: normalized}, nil
}

func (p JobAccessPolicy) IsLegacy() bool { return p == (JobAccessPolicy{}) }

func (p JobAccessPolicy) Scope() (clientkey.AccountScope, error) {
	scope, valid := clientkey.NormalizeAccountScope(p.AccountScope)
	if p.Version != 1 || !valid || scope != p.AccountScope {
		return clientkey.AccountScope{}, ErrInvalidJobAccessPolicy
	}
	return scope, nil
}
