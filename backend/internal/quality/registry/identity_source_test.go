package registry

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

type identityLinksFunc func(context.Context) ([]account.IdentityLink, error)

func (f identityLinksFunc) ListIdentityLinks(ctx context.Context) ([]account.IdentityLink, error) {
	return f(ctx)
}

func TestIdentitySourceFailureAndDetachedReopenPreserveProjection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "identity.db")
	links := []account.IdentityLink{{AccountID: 11, RelatedAccountID: 12}}
	var sourceErr error
	source := identityLinksFunc(func(context.Context) ([]account.IdentityLink, error) { return links, sourceErr })
	r, err := Open(ctx, Options{SQLitePath: path, AccountLinks: source})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	group, members := r.IdentityGroupOf(11)
	members[0] = 99
	_, actual := r.IdentityGroupOf(11)
	if !reflect.DeepEqual(actual, []uint64{11, 12}) {
		t.Fatalf("caller changed immutable projection: %v", actual)
	}

	sourceErr = errors.New("account facts unavailable")
	if err := r.RefreshIdentityGroups(ctx); !errors.Is(err, sourceErr) {
		t.Fatalf("source failure hidden: %v", err)
	}
	reopened, err := Open(ctx, Options{SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if id, members := reopened.IdentityGroupOf(11); id != group || !reflect.DeepEqual(members, []uint64{11, 12}) {
		t.Fatalf("missing source erased persisted projection: %d %v", id, members)
	}
	if unexpected, err := Open(ctx, Options{SQLitePath: path, AccountLinks: source}); err == nil {
		_ = unexpected.Close()
		t.Fatal("failed source accepted during startup")
	}
	sourceErr, links = nil, nil
	if err := r.RefreshIdentityGroups(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RefreshState(ctx); err != nil {
		t.Fatal(err)
	}
	if id, members := reopened.IdentityGroupOf(11); id != 11 || !reflect.DeepEqual(members, []uint64{11}) {
		t.Fatalf("explicit empty facts did not clear projection: %d %v", id, members)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := reopened.RefreshIdentityGroups(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled detached refresh=%v", err)
	}
}
