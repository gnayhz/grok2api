package account

import (
	"context"
	"errors"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// round 70 回归：Device OAuth 轮询状态机的全部转换此前只有活体验证，
// 无单元护栏。此处覆盖 pending / slow-down(+5s 补偿) / denied 删除会话 /
// 未知会话=expired 的关键分支。

type deviceStateAdapter struct {
	provider.Adapter
	pollErr error
	polls   int
}

func (a *deviceStateAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }

func (a *deviceStateAdapter) StartDeviceAuthorization(ctx context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{DeviceCode: "dc", UserCode: "UC", VerificationURI: "https://x", VerificationURIComplete: "https://x", Interval: time.Second, ExpiresIn: time.Minute}, nil
}

func (a *deviceStateAdapter) PollDeviceAuthorization(ctx context.Context, deviceCode string) (provider.CredentialSeed, error) {
	a.polls++
	return provider.CredentialSeed{}, a.pollErr
}

func TestPollDeviceLoginStateMachine(t *testing.T) {
	ctx := context.Background()
	store := memory.NewDeviceSessionStore()
	adapter := &deviceStateAdapter{}
	service := NewService(nil, nil, store, nil, provider.NewRegistry(adapter), nil, nil)

	// 未知会话 -> ErrDeviceDenied（等价于过期，不泄露存在性）。
	if _, err := service.PollDeviceLogin(ctx, "no-such-session"); !errors.Is(err, ErrDeviceDenied) {
		t.Fatalf("unknown session: err = %v, want ErrDeviceDenied", err)
	}

	// 建立真实会话（NextPollAt = now + interval）。
	started, err := service.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 立即轮询 -> slow-down（未到 NextPollAt）。
	if _, err := service.PollDeviceLogin(ctx, started.SessionID); !errors.Is(err, ErrDeviceSlowDown) {
		t.Fatalf("early poll: err = %v, want ErrDeviceSlowDown", err)
	}
	if adapter.polls != 0 {
		t.Fatalf("early poll must not reach upstream, polls = %d", adapter.polls)
	}

	// 到期后：pending 转换会推进 NextPollAt。
	session, err := store.Get(ctx, started.SessionID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	session.NextPollAt = time.Now().UTC().Add(-time.Second)
	if err := store.Update(ctx, session); err != nil {
		t.Fatal(err)
	}
	adapter.pollErr = provider.ErrAuthorizationPending
	if _, err := service.PollDeviceLogin(ctx, started.SessionID); !errors.Is(err, ErrDevicePending) {
		t.Fatalf("pending poll: err = %v, want ErrDevicePending", err)
	}
	if adapter.polls != 1 {
		t.Fatalf("upstream polls = %d, want 1", adapter.polls)
	}

	// 上游 slow-down：interval +5s 补偿并再次节流。
	session, _ = store.Get(ctx, started.SessionID, time.Now().UTC())
	session.NextPollAt = time.Now().UTC().Add(-time.Second)
	_ = store.Update(ctx, session)
	adapter.pollErr = provider.ErrSlowDown
	if _, err := service.PollDeviceLogin(ctx, started.SessionID); !errors.Is(err, ErrDeviceSlowDown) {
		t.Fatalf("upstream slow-down: err = %v, want ErrDeviceSlowDown", err)
	}
	session, _ = store.Get(ctx, started.SessionID, time.Now().UTC())
	if session.Interval < 6*time.Second {
		t.Fatalf("slow-down must add 5s backoff, interval = %s", session.Interval)
	}

	// denied：会话删除，后续轮询回到 expired/denied。
	session.NextPollAt = time.Now().UTC().Add(-time.Second)
	_ = store.Update(ctx, session)
	adapter.pollErr = provider.ErrAuthorizationDenied
	if _, err := service.PollDeviceLogin(ctx, started.SessionID); !errors.Is(err, ErrDeviceDenied) {
		t.Fatalf("denied poll: err = %v, want ErrDeviceDenied", err)
	}
	if _, err := store.Get(ctx, started.SessionID, time.Now().UTC()); err == nil {
		t.Fatal("denied session must be deleted")
	}
}
