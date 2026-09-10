package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func eventRegistries(t *testing.T, driver string) (*registry.Registry, *registry.Registry) {
	t.Helper()
	opts := registry.Options{Driver: driver, SQLitePath: filepath.Join(t.TempDir(), "events.db")}
	if driver == "postgres" {
		dsn := os.Getenv("GROK_EVOLUTION_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("requires isolated GROK_EVOLUTION_POSTGRES_DSN")
		}
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		sql, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("events_%d", time.Now().UnixNano())
		if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			sql.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Exec("DROP SCHEMA " + schema + " CASCADE"); sql.Close() })
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		opts.PostgresDSN = dsn + separator + "search_path=" + schema
	}
	first, err := registry.Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := registry.Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	return first, second
}
func eventServices(t *testing.T, reg *registry.Registry) (*Service, *journal.Store, *evidence.Store, *court.Service) {
	t.Helper()
	store := journal.New(reg.DB())
	ev, err := evidence.New(context.Background(), reg.DB(), evidence.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := court.DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	judge := court.New(cfg, reg, qualityEvidenceSource{store: ev}, nil)
	t.Cleanup(func() { judge.Close(context.Background()) })
	return New(store, ev, judge), store, ev, judge
}
func incidentReceipt() Receipt {
	return Receipt{Attempt: attemptmeta.Identity{ID: "traffic/1", AccountID: 7, Provider: "grok_build", Model: "grok-4.6", Revision: 3, RuleVersion: "rules", Path: attemptmeta.Path{NodeID: 11, Epoch: 1, Status: attemptmeta.PathRegistered}, Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "high"}}, At: time.Now().UTC(), Outcome: Degraded, Rule: "visible-without-thinking"}
}

type evidenceFault struct {
	Evidence
	after   bool
	failure error
}

func (s evidenceFault) Record(ctx context.Context, obs model.Observation) error {
	if s.after {
		if err := s.Evidence.Record(ctx, obs); err != nil {
			return err
		}
	}
	return s.failure
}

type incidentFault struct {
	Incidents
	after   bool
	failure error
}

func (s incidentFault) ReportDegradedObservation(ctx context.Context, obs model.Observation) error {
	if s.after {
		if err := s.Incidents.ReportDegradedObservation(ctx, obs); err != nil {
			return err
		}
	}
	return s.failure
}

type releaseFault struct {
	Journal
	after   bool
	failure error
}

func (s releaseFault) Release(ctx context.Context, id string, at time.Time) error {
	if s.after {
		if err := s.Journal.Release(ctx, id, at); err != nil {
			return err
		}
	}
	return s.failure
}

func TestEventConsumptionRecoveryAcrossReplicas(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			for _, stage := range []string{"evidence_before", "evidence_after", "case_before", "case_after", "release_before", "release_after", "ack"} {
				t.Run(stage, func(t *testing.T) {
					ctx := context.Background()
					a, b := eventRegistries(t, driver)
					if _, _, err := a.AdvanceEpoch(ctx, 11, "198.51.100.11"); err != nil {
						t.Fatal(err)
					}
					first, store, ev, judge := eventServices(t, a)
					receipt := incidentReceipt()
					if err := first.RecordQualityEvent(ctx, receipt, time.Minute); err != nil {
						t.Fatal(err)
					}
					if allowed, err := store.AccountAllowed(ctx, 7, time.Now().UTC()); err != nil || allowed {
						t.Fatalf("receipt has no hold: %v %v", allowed, err)
					}
					if cases, err := a.ListOpenCases(ctx); err != nil || len(cases) != 0 {
						t.Fatalf("request path opened case: %v %v", cases, err)
					}
					failure := errors.New("injected consumer boundary failure")
					switch stage {
					case "evidence_before", "evidence_after":
						first.evidence = evidenceFault{ev, strings.HasSuffix(stage, "after"), failure}
					case "case_before", "case_after":
						first.incidents = incidentFault{judge, strings.HasSuffix(stage, "after"), failure}
					case "release_before", "release_after":
						first.journal = releaseFault{store, strings.HasSuffix(stage, "after"), failure}
					}
					processed, err := store.ProcessOne(ctx, "first", func(ctx context.Context, event journal.Event) error {
						if err := first.handle(ctx, event); err != nil {
							return err
						}
						if stage == "ack" {
							return a.DB().Migrator().RenameTable("q_guard_capacity", "e09_hidden_capacity")
						}
						return nil
					})
					if !processed || err == nil {
						t.Fatalf("boundary failure lost: worked=%v err=%v", processed, err)
					}
					if stage == "ack" {
						if err := a.DB().Migrator().RenameTable("e09_hidden_capacity", "q_guard_capacity"); err != nil {
							t.Fatal(err)
						}
					}
					var pending journal.OutboxRow
					if err := b.DB().First(&pending, "event_id = ?", receipt.Attempt.ID+"/admission").Error; err != nil || pending.ProcessedAt != nil {
						t.Fatalf("failed work acked: %+v err=%v", pending, err)
					}
					var hold journal.RestrictionRow
					if err := b.DB().First(&hold, "owner = ?", receipt.Attempt.ID+"/admission").Error; err != nil {
						t.Fatal(err)
					}
					expectReleased := stage == "release_after" || stage == "ack"
					if (hold.ReleasedAt != nil) != expectReleased {
						t.Fatalf("hold released before next owner: %+v", hold)
					}
					// Emulate retry/expired lease after process loss; no record or policy changes.
					if err := b.DB().Model(&journal.OutboxRow{}).Where("event_id = ?", pending.EventID).Updates(map[string]any{"ready_at": time.Now().UTC().Add(-time.Second), "lease_until": nil}).Error; err != nil {
						t.Fatal(err)
					}
					resumed, resumedStore, resumedEvidence, _ := eventServices(t, b)
					if worked, err := resumedStore.ProcessOne(ctx, "resumed", resumed.handle); err != nil || !worked {
						t.Fatalf("resume failed: %v %v", worked, err)
					}
					if count, err := resumedEvidence.Count(ctx); err != nil || count != 1 {
						t.Fatalf("duplicate/lost evidence count=%d err=%v", count, err)
					}
					if cases, err := b.ListOpenCases(ctx); err != nil || len(cases) != 1 {
						t.Fatalf("duplicate/lost case count=%d err=%v", len(cases), err)
					}
					stats, err := resumed.Backlog(ctx)
					if err != nil || stats.Pending != 0 || stats.InFlight != 0 {
						t.Fatalf("backlog=%+v err=%v", stats, err)
					}
					if err := b.DB().First(&hold, "owner = ?", receipt.Attempt.ID+"/admission").Error; err != nil || hold.ReleasedAt == nil {
						t.Fatalf("hold not handed off: %+v %v", hold, err)
					}
					if allowed, err := resumedStore.AccountAllowed(ctx, 7, time.Now().UTC().Add(time.Hour)); err != nil || allowed {
						t.Fatalf("case protection missing after receipt expiry: %v %v", allowed, err)
					}
					if worked, err := resumedStore.ProcessOne(ctx, "duplicate", resumed.handle); err != nil || worked {
						t.Fatalf("duplicate work claimed: %v %v", worked, err)
					}
				})
			}
		})
	}
}

func TestOldEpochEventArchivesWithoutRestrictingNewEpoch(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			a, b := eventRegistries(t, driver)
			if _, _, err := a.AdvanceEpoch(ctx, 11, "198.51.100.11"); err != nil {
				t.Fatal(err)
			}
			svc, store, ev, _ := eventServices(t, a)
			receipt := incidentReceipt()
			if err := svc.RecordQualityEvent(ctx, receipt, time.Minute); err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.AdvanceEpoch(ctx, 11, "198.51.100.12"); err != nil {
				t.Fatal(err)
			}
			if worked, err := store.ProcessOne(ctx, "late", svc.handle); err != nil || !worked {
				t.Fatalf("worked=%v err=%v", worked, err)
			}
			if count, err := ev.Count(ctx); err != nil || count != 1 {
				t.Fatalf("old evidence lost %d %v", count, err)
			}
			if cases, err := b.ListOpenCases(ctx); err != nil || len(cases) != 0 {
				t.Fatalf("old event opened new case %v %v", cases, err)
			}
			if allowed, err := b.ExitAllowed(ctx, 11); err != nil || !allowed {
				t.Fatalf("new epoch restricted %v %v", allowed, err)
			}
			if allowed, err := store.AccountAllowed(ctx, 7, time.Now().UTC()); err != nil || !allowed {
				t.Fatalf("obsolete temporary hold remained %v %v", allowed, err)
			}
		})
	}
}

type blockingEvidence struct {
	Evidence
	started chan struct{}
	once    sync.Once
}

func (e *blockingEvidence) Record(ctx context.Context, obs model.Observation) error {
	e.once.Do(func() { close(e.started) })
	<-ctx.Done()
	return ctx.Err()
}
func TestEventWorkerCancellationAndPeerClaim(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			a, b := eventRegistries(t, driver)
			svc, store, ev, _ := eventServices(t, a)
			receipt := incidentReceipt()
			if err := svc.RecordQualityEvent(ctx, receipt, time.Minute); err != nil {
				t.Fatal(err)
			}
			wait := &blockingEvidence{Evidence: ev, started: make(chan struct{})}
			svc.evidence = wait
			workerCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- svc.Run(workerCtx) }()
			select {
			case <-wait.started:
			case <-time.After(3 * time.Second):
				cancel()
				t.Fatal("worker did not start")
			}
			if worked, err := journal.New(b.DB()).ProcessOne(ctx, "peer", svc.handle); err != nil || worked {
				cancel()
				t.Fatalf("peer stole live claim %v %v", worked, err)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel err=%v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not cancel")
			}
			if stats, err := store.Stats(ctx); err != nil || stats.Pending != 1 {
				t.Fatalf("cancel lost event: %+v %v", stats, err)
			}
			if err := b.DB().Model(&journal.OutboxRow{}).Where("event_id = ?", receipt.Attempt.ID+"/admission").Updates(map[string]any{"lease_until": nil, "ready_at": time.Now().UTC().Add(-time.Second)}).Error; err != nil {
				t.Fatal(err)
			}
			resumed, resumedStore, _, _ := eventServices(t, b)
			if worked, err := resumedStore.ProcessOne(ctx, "replacement", resumed.handle); err != nil || !worked {
				t.Fatalf("replacement %v %v", worked, err)
			}
		})
	}
}
