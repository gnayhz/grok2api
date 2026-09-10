package account

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// Signaling Done observes the actual wait select, without timing assumptions.
type identityWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *identityWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestIdentitySyncPanicReleasesWaitersAndKeepsOwnerStack(t *testing.T) {
	var group OperationGroup[accountdomain.CredentialRef]
	ref := accountdomain.CredentialRef{AccountID: 1, Provider: accountdomain.ProviderWeb, Generation: 1}
	entered, release := make(chan struct{}), make(chan struct{})
	owner := make(chan any, 1)
	go func() {
		defer func() { owner <- recover() }()
		_, _ = group.Do(context.Background(), ref, func() (any, error) { close(entered); <-release; panic("synthetic-secret-payload") })
	}()
	<-entered
	waitCtx := &identityWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() {
		_, err := group.Do(waitCtx, ref, func() (any, error) { t.Error("waiter unexpectedly executed"); return nil, nil })
		waiter <- err
	}()
	select {
	case <-waitCtx.waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not join")
	}
	close(release)
	select {
	case v := <-owner:
		if v != "synthetic-secret-payload" {
			t.Fatal("owner lost panic")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not unwind")
	}
	if err := awaitIdentityResult(t, waiter); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("waiter did not get a safe interrupted result: %v", err)
	}
	if _, err := group.Do(context.Background(), ref, func() (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
}
