package history

import (
	"context"
	"errors"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"time"
)

var (
	ErrResponseNotFound = errors.New("Response 不存在或已过期")
	ErrResponseRead     = errors.New("response ownership read failed")
	ErrResponseDelete   = errors.New("response ownership delete failed")
)

// ResponseStorageRequested preserves the default resource behavior when the
// client omits store or sends null. An explicit false disables the new response
// resource, not independent history, generation or audit records.
func ResponseStorageRequested(store *bool) bool {
	return store == nil || *store
}

// NativeResponseState is the injected history port for Web's compatibility
// resources. Providers supply genuine upstream facts; M12 owns durable policy.
type NativeResponseState interface {
	LookupWeb(context.Context, string, time.Time) (inferencedomain.WebResponseState, error)
	RecordWeb(context.Context, inferencedomain.WebResponseState, time.Time) error
	DeleteWeb(context.Context, string) error
}
