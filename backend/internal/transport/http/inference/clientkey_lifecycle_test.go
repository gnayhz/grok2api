package inference

import (
	"context"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
)

func closeClientKeyService(t *testing.T, service *clientkeyapp.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Errorf("close client key service before SQL: %v", err)
	}
}
