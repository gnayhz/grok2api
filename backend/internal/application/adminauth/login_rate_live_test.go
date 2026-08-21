package adminauth

import (
	"context"
	"errors"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// TestCheckLoginRateUserThresholdLive 复现 round 80 活体疑问：
// memory limiter 下 13 次同用户失败是否触发 ErrLoginRateLimited。
func TestCheckLoginRateUserThresholdLive(t *testing.T) {
	svc := &Service{loginLimiter: memory.NewRateLimiter()}
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		if err := svc.checkLoginRate(ctx, "root", "127.0.0.1"); err != nil {
			t.Fatalf("attempt %d 提前限流: %v", i+1, err)
		}
	}
	if err := svc.checkLoginRate(ctx, "root", "127.0.0.1"); !errors.Is(err, ErrLoginRateLimited) {
		t.Fatalf("第 13 次应 ErrLoginRateLimited, got %v", err)
	}
}
