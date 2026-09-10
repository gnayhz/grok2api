package clientkey

import (
	"context"
	"testing"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type benchmarkTouchRepository struct {
	repository.ClientKeyRepository
	finished chan struct{}
}

func (r benchmarkTouchRepository) Touch(context.Context, uint64) error {
	r.finished <- struct{}{}
	return nil
}

func BenchmarkAuthenticateCachedUnlimitedKey(b *testing.B) {
	ctx := context.Background()
	raw := security.FormatClientKey("benchmark", "synthetic")
	value := clientkeydomain.Key{ID: 1, Prefix: "benchmark", SecretHash: security.HashToken(raw), Enabled: true}
	repo := benchmarkTouchRepository{finished: make(chan struct{}, 1)}
	service := NewService("benchmark-owner", repo, nil, nil, 0, 0, nil)
	defer service.Close(ctx)
	service.authCache.byPrefix[value.Prefix] = cachedAuthKey{value: value, expiresAt: time.Now().Add(time.Hour)}
	_, release, err := service.Authenticate(ctx, raw)
	if err != nil {
		b.Fatal(err)
	}
	release()
	<-repo.finished
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, release, err := service.Authenticate(ctx, raw)
		if err != nil {
			b.Fatal(err)
		}
		release()
	}
}
