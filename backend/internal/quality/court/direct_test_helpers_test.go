package court

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"gorm.io/gorm"
)

type testBench struct {
	registry *registry.Registry
	evidence *evidence.Store
}

func newBench(t testing.TB) *testBench {
	return newBenchWithEvidenceConfig(t, model.DefaultEvidenceConfig())
}

func newBenchWithEvidenceConfig(t testing.TB, cfg model.EvidenceConfig, links ...registry.AccountLinks) *testBench {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "court.db")
	opts := registry.Options{Driver: "sqlite", SQLitePath: path}
	if len(links) > 0 {
		opts.AccountLinks = links[0]
	}
	qualityRegistry, err := registry.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = qualityRegistry.Close() })
	if err := qualityRegistry.DB().Exec("CREATE TABLE provider_accounts (id INTEGER PRIMARY KEY, enabled INTEGER, auth_status TEXT, provider TEXT, risk_status TEXT NOT NULL DEFAULT '', cooldown_until TIMESTAMP)").Error; err != nil {
		t.Fatal(err)
	}
	if err := qualityRegistry.DB().Exec("CREATE TABLE egress_nodes (id INTEGER PRIMARY KEY, enabled INTEGER, proxy_pool INTEGER, rotation_enabled INTEGER, cooldown_until TIMESTAMP)").Error; err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 10; id++ {
		if err := qualityRegistry.DB().Exec("INSERT INTO provider_accounts (id, enabled, auth_status, provider) VALUES (?, 1, 'active', 'grok_build')", id).Error; err != nil {
			t.Fatal(err)
		}
	}
	for id := 1; id <= 8; id++ {
		if err := qualityRegistry.DB().Exec("INSERT INTO egress_nodes (id, enabled, proxy_pool, rotation_enabled) VALUES (?, 1, 0, 0)", id).Error; err != nil {
			t.Fatal(err)
		}
	}
	gormDB, err := openTestEvidenceDB(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, err := evidence.New(ctx, gormDB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &testBench{registry: qualityRegistry, evidence: store}
}

func recordObsAt(t testing.TB, bench *testBench, account, node uint64, outcome model.Outcome, at time.Time) {
	t.Helper()
	if err := bench.evidence.Record(context.Background(), model.Observation{
		At: at, AccountID: account, Exit: model.EpochKey{NodeID: node},
		Source: model.SourceTraffic, Outcome: outcome, Rule: "test",
	}); err != nil {
		t.Fatal(err)
	}
}

func recordObs(t testing.TB, bench *testBench, account, node uint64, outcome model.Outcome) {
	recordObsAt(t, bench, account, node, outcome, time.Now().UTC())
}

func newTestCourt(bench *testBench, cfg Config) *Service {
	return newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, nil)
}

type storeSource struct{ store *evidence.Store }

func (s storeSource) SnapshotWindow(now time.Time) model.Snapshot {
	return s.store.SnapshotWindow(now)
}

func (s storeSource) CrossValidate(snapshot model.Snapshot) model.Estimate {
	return s.store.CrossValidate(snapshot)
}

// Existing court protocol tests use a small fleet fixture. Real routing policy
// and repository integration are exercised by Application and Selector tests.
type fixtureProbeAccounts struct{ reg *registry.Registry }

func (f fixtureProbeAccounts) QualityProbeAccounts(ctx context.Context, _ model.ProbeExperiment) ([]uint64, error) {
	var ids []uint64
	err := f.reg.DB().WithContext(ctx).Table("provider_accounts").Where("enabled = ? AND auth_status = ? AND provider = ? AND COALESCE(risk_status, '') = '' AND (cooldown_until IS NULL OR cooldown_until <= ?)", true, "active", "grok_build", time.Now()).Pluck("id", &ids).Error
	return ids, err
}
func newFixtureCourt(cfg Config, reg *registry.Registry, source EvidenceSource, dispatcher Dispatcher) *Service {
	s := New(cfg, reg, source, dispatcher, registry.NewProbeTaskStore(reg))
	go s.Run(context.Background())
	s.SetProbeAccounts(fixtureProbeAccounts{reg})
	s.SetNodes(fixtureNodes{reg})
	return s
}

// Minimal node fixtures for the Court protocol tests; actual operational facts
// and errors are exercised by the Application's relational egress source.
type fixtureNodes struct{ reg *registry.Registry }

func (f fixtureNodes) ListProfiles(ctx context.Context) ([]proxy.NodeProfile, error) {
	var rows []proxy.NodeProfile
	err := f.reg.DB().WithContext(ctx).Table("egress_nodes").Select("id, enabled, proxy_pool, rotation_enabled AS rotation_webhook, cooldown_until").Find(&rows).Error
	for i := range rows {
		rows[i].CanServeFixedTarget = true
	}
	return rows, err
}
func (f fixtureNodes) Profile(ctx context.Context, id uint64) (proxy.NodeProfile, bool, error) {
	var row proxy.NodeProfile
	err := f.reg.DB().WithContext(ctx).Table("egress_nodes").Select("id, enabled, proxy_pool, rotation_enabled AS rotation_webhook, cooldown_until").Where("id = ?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, false, nil
	}
	row.CanServeFixedTarget = true
	return row, err == nil, err
}
