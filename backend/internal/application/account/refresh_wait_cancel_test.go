package account

import (
	"context"
	"errors"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type refreshWaitCancelAdapter struct {
	*credentialRefreshAdapter
	operation string
	entered   chan struct{}
	release   chan struct{}
}

func (a *refreshWaitCancelAdapter) RefreshCredential(ctx context.Context, v accountdomain.Credential) (provider.RefreshedCredential, error) {
	if a.operation == "refresh" {
		close(a.entered)
		<-a.release
	}
	return a.credentialRefreshAdapter.RefreshCredential(ctx, v)
}
func (a *refreshWaitCancelAdapter) GetBilling(ctx context.Context, v accountdomain.Credential) (accountdomain.Billing, error) {
	if a.operation == "billing" {
		close(a.entered)
		<-a.release
	}
	return a.credentialRefreshAdapter.GetBilling(ctx, v)
}

func TestSharedAccountRefreshWaiterMustHonorOwnCancellation(t *testing.T) {
	for _, operation := range []string{"refresh", "billing"} {
		t.Run(operation, func(t *testing.T) {
			s, credential, base := newCredentialRefreshTestService(t, time.Now())
			a := &refreshWaitCancelAdapter{credentialRefreshAdapter: base, operation: operation, entered: make(chan struct{}), release: make(chan struct{})}
			var once sync.Once
			finish := func() { once.Do(func() { close(a.release) }) }
			defer finish()
			s.providers = providerimpl.NewRegistry(a)
			call := func(ctx context.Context) error {
				if operation == "refresh" {
					_, err := s.EnsureCredential(ctx, credential, true)
					return err
				}
				_, err := s.RefreshBilling(ctx, credential.ID)
				return err
			}
			owner := make(chan error, 1)
			go func() { owner <- call(context.Background()) }()
			select {
			case <-a.entered:
			case err := <-owner:
				t.Fatalf("owner failed before Provider gate: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("owner did not start")
			}
			waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			waiter := make(chan error, 1)
			go func() { waiter <- call(waitCtx) }()
			<-waitCtx.Done()
			var waiterErr error
			select {
			case waiterErr = <-waiter:
			case <-time.After(time.Second):
				t.Error("canceled waiter remained attached to another caller's Provider operation")
				finish()
				select {
				case waiterErr = <-waiter:
				case <-time.After(3 * time.Second):
					t.Fatal("waiter failed to stop after owner release")
				}
			}
			if !errors.Is(waiterErr, context.DeadlineExceeded) {
				t.Errorf("canceled waiter returned shared result: %v", waiterErr)
			}
			finish()
			select {
			case err := <-owner:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("owner failed to stop")
			}
		})
	}
}
