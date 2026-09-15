package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type versionedTestProber struct {
	ip              string
	started, resume chan struct{}
}

func (p versionedTestProber) ProbeEgressNode(context.Context, egressdomain.Node) (egressdomain.ProbeResult, error) {
	if p.started != nil {
		close(p.started)
		<-p.resume
	}
	return egressdomain.ProbeResult{Status: egressdomain.ProbeStatusHealthy, ExitIP: p.ip, TestedAt: time.Now().UTC()}, nil
}

func TestProbeObservationVersionReachesQualityRegistry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "observations.db")
	db, err := relational.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewEgressRepository(db)
	node, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: "observation", Enabled: true, EncryptedProxyURL: "fixed-proxy", ExitIP: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(ctx, registry.Options{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	source := baseExitIPSource{egress: egressapp.NewService(repo, nil)}
	if _, _, ok, err := source.CurrentExitIdentity(ctx, node.ID); ok || err != nil {
		t.Fatal("legacy IP without version became release evidence")
	}
	if err := r.RecordExitIdentity(ctx, node.ID, model.ExitIdentityFromAggregate("192.0.2.1")); err != nil {
		t.Fatal(err)
	}
	a, b := egressapp.NewService(repo, nil), egressapp.NewService(repo, nil)
	old := versionedTestProber{ip: "192.0.2.1", started: make(chan struct{}), resume: make(chan struct{})}
	a.SetNodeProber(old)
	b.SetNodeProber(versionedTestProber{ip: "192.0.2.2"})
	done := make(chan error, 1)
	go func() { _, err := a.TestNode(ctx, node.ID); done <- err }()
	<-old.started
	defer func() {
		close(old.resume)
		if err := <-done; !errors.Is(err, egressapp.ErrProbeStale) {
			t.Errorf("late result=%v", err)
		}
		ip, version, ok, err := source.CurrentExitIdentity(ctx, node.ID)
		if err != nil || !ok || ip.IPv4 != "192.0.2.2" || version != 2 {
			t.Errorf("source regressed: %s %d %v", ip, version, ok)
		}
		if _, _, _, err := r.ObserveExitIdentity(ctx, node.ID, ip, version); err != nil {
			t.Error(err)
		}
		if allowed, err := r.ExitAllowed(ctx, node.ID); err != nil || allowed {
			t.Errorf("late source released new ban: %v %v", allowed, err)
		}
	}()
	result, err := b.TestNode(ctx, node.ID)
	if err != nil || result.Revision != 2 {
		t.Fatalf("version not allocated before probe: %+v %v", result, err)
	}
	ip, version, ok, err := source.CurrentExitIdentity(ctx, node.ID)
	if err != nil || !ok || version != 2 {
		t.Fatalf("source missing version: %d %v", version, ok)
	}
	if _, epoch, _, err := r.ObserveExitIdentity(ctx, node.ID, ip, version); err != nil || epoch != 1 {
		t.Fatalf("fresh IP not consumed: %d %v", epoch, err)
	}
	id, err := r.OpenInvestigation(ctx, 9, model.EpochKey{NodeID: node.ID, Epoch: 1}, time.Now(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, id, model.VerdictExitGuilty, `{}`, time.Now(), true); err != nil {
		t.Fatal(err)
	}
}
