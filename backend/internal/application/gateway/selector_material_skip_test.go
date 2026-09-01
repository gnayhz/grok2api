package gateway

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// transientMaterialError 模拟存储层瞬态故障（行锁/超时类）。
func transientMaterialError() error {
	return fmt.Errorf("load credentials: %w", context.DeadlineExceeded)
}

func TestSelectorSkipsAccountOnTransientCredentialMaterialFailure(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = []account.RoutingAccountBase{
		{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20}},
		{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10}},
	}
	repo.materialErrors = map[uint64]error{1: transientMaterialError()}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 2 {
		t.Fatalf("lease credential = %d, want transient-failure account skipped", lease.Credential.ID)
	}
	if !reflect.DeepEqual(repo.materialCalls, []uint64{1, 2}) {
		t.Fatalf("material calls = %v, want [1 2]", repo.materialCalls)
	}
}

func TestSelectorSurfacesStorageErrorAfterTransientSkipLimit(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	bases := make([]account.RoutingAccountBase, 0, credentialMaterialFailureSkipLimit+1)
	materialErrors := make(map[uint64]error, credentialMaterialFailureSkipLimit+1)
	for id := uint64(1); id <= uint64(credentialMaterialFailureSkipLimit+1); id++ {
		bases = append(bases, account.RoutingAccountBase{Credential: account.Credential{ID: id, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100 - int(id)}})
		materialErrors[id] = transientMaterialError()
	}
	repo.bases = bases
	repo.materialErrors = materialErrors
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	if lease != nil {
		lease.Release()
		t.Fatal("lease acquired despite systemic storage failure")
	}
	if err == nil || !strings.Contains(err.Error(), "连续加载账号凭据材料失败") {
		t.Fatalf("error = %v, want surfaced storage failure after skip limit", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want root storage cause preserved", err)
	}
}

func TestSelectorKeepsRootCauseOnPermanentCredentialMaterialFailure(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = []account.RoutingAccountBase{
		{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20}},
		{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10}},
	}
	repo.materialErrors = map[uint64]error{1: repository.ErrInvalidRecord}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	if lease != nil {
		lease.Release()
		t.Fatal("lease acquired despite permanent storage failure")
	}
	if !errors.Is(err, repository.ErrInvalidRecord) {
		t.Fatalf("error = %v, want permanent storage root cause", err)
	}
	var unavailable *SelectionUnavailableError
	if errors.As(err, &unavailable) {
		t.Fatalf("error = %v, permanent failure must not look like pool exhaustion", err)
	}
}
