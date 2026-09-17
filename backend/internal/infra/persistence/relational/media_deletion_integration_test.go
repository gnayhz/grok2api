package relational

import (
	"context"
	"errors"
	"fmt"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func seedMediaDeletion(t *testing.T, db *Database, suffix string, status media.Status) (clientkey.Key, media.Job) {
	t.Helper()
	ctx := context.Background()
	key, err := NewClientKeyRepository(db).Create(ctx, clientkey.Key{Name: "deletion", Prefix: "delete-" + suffix, SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 10000000000})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	job := media.Job{ID: "video_deletion_" + suffix, RequestID: "request_deletion_" + suffix, ClientKeyID: key.ID, ClientKeyName: key.Name, Provider: "grok_web", Model: "Web/grok-imagine-video", ModelRouteID: 1, UpstreamModel: "grok-imagine-video", Prompt: "synthetic", Seconds: 3, Quality: "720p", Status: status, CreatedAt: now.Add(-time.Second), UpdatedAt: now, CompletedAt: &now, Quota: media.JobQuota{RecordedAt: &now}, UsageRecordedAt: &now}
	return key, job
}

func TestMediaDeletionEligibilityAcrossCommands(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			keyRepo, jobs := NewClientKeyRepository(db), NewMediaJobRepository(db)
			keys := clientkeyapp.NewService("deletion", keyRepo, nil, nil, 0, 0, nil, security.RandomTokenSource{})
			defer keys.Close(ctx)
			admin := mediaapp.NewServiceWithTickets(NewMediaAssetRepository(db), jobs, nil, nil, nil, mediaapp.Config{})
			var seq int
			for _, stage := range []string{"queued", "in_progress", "completed", "failed", "completed_no_quota", "completed_no_usage", "failed_no_usage", "generated_failed_no_quota", "generated_failed_no_usage", "generated_failed_complete"} {
				for _, entry := range []string{"key_single", "key_batch", "key_repo_single", "key_repo_batch", "media_admin", "media_repo"} {
					t.Run(stage+"/"+entry, func(t *testing.T) {
						seq++
						status := media.StatusCompleted
						if strings.Contains(stage, "failed") {
							status = media.StatusFailed
						} else if stage == "queued" || stage == "in_progress" {
							status = media.Status(stage)
						}
						key, job := seedMediaDeletion(t, db, fmt.Sprint(seq), status)
						if strings.HasPrefix(stage, "generated_") {
							job.Execution = media.VideoExecution{Revision: 1, Phase: media.VideoExecutionGenerated, Route: "web", Endpoint: "https://native.invalid/video", GeneratedAt: job.CompletedAt}
						}
						if strings.HasSuffix(stage, "no_quota") {
							job.Quota.RecordedAt = nil
						}
						if strings.HasSuffix(stage, "no_usage") {
							job.UsageRecordedAt = nil
						}
						if err := jobs.CreateMediaJob(ctx, job); err != nil {
							t.Fatal(err)
						}
						ticket := repository.MediaUploadTicket{TokenHash: fmt.Sprintf("%064d", seq), AssetID: fmt.Sprintf("vid_deletion_ticket_%d", seq), JobID: job.ID, MaxBytes: 1024, AllowedMIME: "video/mp4", CreatedAt: job.CreatedAt, ExpiresAt: job.CreatedAt.Add(time.Hour)}
						if err := NewMediaUploadTicketRepository(db).CreateUploadTicket(ctx, ticket); err != nil {
							t.Fatal(err)
						}
						wantConflict := stage == "queued" || stage == "in_progress" || strings.Contains(stage, "no_")
						var err error
						var deleted int64
						switch entry {
						case "key_single":
							err = keys.Delete(ctx, key.ID)
						case "key_batch":
							deleted, err = keys.BatchDelete(ctx, []uint64{key.ID, key.ID, key.ID + 1000000})
						case "key_repo_single":
							err = keyRepo.Delete(ctx, key.ID)
						case "key_repo_batch":
							deleted, err = keyRepo.DeleteMany(ctx, []uint64{key.ID, key.ID, key.ID + 1000000})
						case "media_admin":
							var n int
							n, err = admin.AdminDeleteVideoJobs(ctx, []string{job.ID, job.ID, "missing"})
							deleted = int64(n)
						case "media_repo":
							err = jobs.DeleteMediaJob(ctx, job.ID)
						}
						if wantConflict {
							if !errors.Is(err, repository.ErrConflict) && !errors.Is(err, clientkeyapp.ErrConflict) && !errors.Is(err, media.ErrJobActive) && !errors.Is(err, media.ErrJobCompletionPending) {
								t.Fatalf("deletion must return domain conflict: deleted=%d err=%v", deleted, err)
							}
							if deleted != 0 {
								t.Fatalf("rejected command returned deleted=%d", deleted)
							}
							if _, err := NewMediaJobRepository(peer).GetMediaJob(ctx, job.ID, key.ID); err != nil {
								t.Fatalf("rejected deletion lost source: %v", err)
							}
							if _, err := NewMediaUploadTicketRepository(peer).GetUploadTicketByHash(ctx, ticket.TokenHash); err != nil {
								t.Fatalf("rejected deletion revoked ticket: %v", err)
							}
						} else {
							if err != nil {
								t.Fatal(err)
							}
							if (strings.HasSuffix(entry, "batch") || entry == "media_admin") && deleted != 1 {
								t.Fatalf("duplicate/missing IDs affected count: %d", deleted)
							}
							if _, err := NewMediaJobRepository(peer).GetMediaJob(ctx, job.ID, key.ID); !errors.Is(err, repository.ErrNotFound) {
								t.Fatalf("eligible source remained: %v", err)
							}
							if _, err := NewMediaUploadTicketRepository(peer).GetUploadTicketByHash(ctx, ticket.TokenHash); !errors.Is(err, repository.ErrNotFound) {
								t.Fatalf("deleted job retained upload ticket: %v", err)
							}
						}
						_, keyErr := NewClientKeyRepository(peer).Get(ctx, key.ID)
						if !wantConflict && strings.HasPrefix(entry, "key_") {
							if !errors.Is(keyErr, repository.ErrNotFound) {
								t.Fatalf("key remained: %v", keyErr)
							}
						} else if keyErr != nil {
							t.Fatalf("unselected/rejected key removed: %v", keyErr)
						}
					})
				}
			}
		})
	}
}

func TestMediaDeletionRetainsPricedRecoverySource(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, entry := range []string{"control", "media_admin", "key_single", "key_batch"} {
			t.Run(dialect+"/"+entry, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx := context.Background()
				key, job := seedMediaDeletion(t, db, "priced", media.StatusCompleted)
				job.UsageRecordedAt = nil
				jobs := NewMediaJobRepository(db)
				if err := jobs.CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				keys := clientkeyapp.NewService("deletion", NewClientKeyRepository(db), nil, nil, 0, 0, nil, security.RandomTokenSource{})
				defer keys.Close(ctx)
				switch entry {
				case "media_admin":
					admin := mediaapp.NewServiceWithTickets(NewMediaAssetRepository(db), jobs, nil, nil, nil, mediaapp.Config{})
					if n, err := admin.AdminDeleteVideoJobs(ctx, []string{job.ID}); n != 0 || !errors.Is(err, media.ErrJobCompletionPending) {
						t.Fatalf("pending usage deleted: %d %v", n, err)
					}
				case "key_single":
					if err := keys.Delete(ctx, key.ID); !errors.Is(err, clientkeyapp.ErrConflict) {
						t.Fatalf("pending usage deleted: %v", err)
					}
				case "key_batch":
					if n, err := keys.BatchDelete(ctx, []uint64{key.ID}); n != 0 || !errors.Is(err, clientkeyapp.ErrConflict) {
						t.Fatalf("pending usage deleted: %d %v", n, err)
					}
				}
				audits := NewAuditRepository(peer)
				recovery := gateway.NewService(nil, audits, nil, keys, nil, nil, nil, security.RandomTokenSource{}, testsupport.NewPhysicalJournalFactory(), nil, 1)
				recovery.ConfigureMedia(NewMediaJobRepository(peer), mediaapp.NewVideoResources(NewMediaJobRepository(peer), nil), 1)
				for range 2 {
					if err := recovery.RecoverVideoJobs(ctx); err != nil {
						t.Fatal(err)
					}
				}
				records, total, err := audits.List(ctx, 0, 10)
				if err != nil || total != 1 || len(records) != 1 || records[0].EventID != "video_usage_"+job.ID || records[0].EstimatedCostInUSDTicks != 2100000000 {
					t.Fatalf("recovery lost/duplicated priced generation: rows=%+v total=%d err=%v", records, total, err)
				}
				billed, err := NewClientKeyRepository(peer).Get(ctx, key.ID)
				if err != nil || billed.BilledUsageUSDTicks != 2100000000 {
					t.Fatalf("recovery billing=%d err=%v", billed.BilledUsageUSDTicks, err)
				}
				if err := keys.Delete(ctx, key.ID); err != nil {
					t.Fatalf("handed-off source cannot be deleted: %v", err)
				}
				if _, total, err := audits.List(ctx, 0, 10); err != nil || total != 1 {
					t.Fatalf("key deletion removed ledger: %d %v", total, err)
				}
			})
		}
	}
}

func TestClientKeyDeletionRollsBackMediaPagesAndNotifiesAfterCommit(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key, job := seedMediaDeletion(t, db, "pages", media.StatusFailed)
			jobs, keys := NewMediaJobRepository(db), NewClientKeyRepository(db)
			for i := range 205 {
				job.ID = fmt.Sprintf("video_delete_pages_%03d", i)
				if err := jobs.CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
			ticket := repository.MediaUploadTicket{TokenHash: strings.Repeat("c", 64), AssetID: "vid_deletion_page_ticket", JobID: "video_delete_pages_000", MaxBytes: 1024, AllowedMIME: "video/mp4", CreatedAt: job.CreatedAt, ExpiresAt: job.CreatedAt.Add(time.Hour)}
			if err := NewMediaUploadTicketRepository(db).CreateUploadTicket(ctx, ticket); err != nil {
				t.Fatal(err)
			}
			if ok, err := keys.ReserveBillingUsage(ctx, key.ID, "delete_reservation", 100, time.Now().UTC().Add(time.Hour), repository.BillingReservationScope{OwnerID: "deletion"}); !ok || err != nil {
				t.Fatalf("reservation: %t %v", ok, err)
			}
			var notifications atomic.Int32
			keys.SetInvalidationObserver(func(ctx context.Context, event repository.InvalidationEvent) {
				notifications.Add(1)
				if _, err := NewClientKeyRepository(peer).Get(ctx, key.ID); !errors.Is(err, repository.ErrNotFound) {
					t.Errorf("notification before key deletion committed: %v", err)
				}
			})
			injected := errors.New("injected key delete failure after media pages")
			if err := db.db.Callback().Delete().Before("gorm:delete").Register("deletion_failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "client_keys" {
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			deleted, err := keys.DeleteMany(ctx, []uint64{key.ID})
			if removeErr := db.db.Callback().Delete().Remove("deletion_failure"); removeErr != nil {
				t.Fatal(removeErr)
			}
			if deleted != 0 || !errors.Is(err, injected) || notifications.Load() != 0 {
				t.Fatalf("failed transaction was partially acknowledged: %d %v notify=%d", deleted, err, notifications.Load())
			}
			var count int64
			if err := peer.db.Model(&mediaJobModel{}).Where("client_key_id = ?", key.ID).Count(&count).Error; err != nil || count != 205 {
				t.Fatalf("earlier media pages not rolled back: %d %v", count, err)
			}
			if _, err := NewMediaUploadTicketRepository(peer).GetUploadTicketByHash(ctx, ticket.TokenHash); err != nil {
				t.Fatalf("failed key deletion revoked media ticket: %v", err)
			}
			assertSettlementKey(t, peer, key.ID, 0, 100)
			// A conflict in a later page of a different selected key must also
			// restore every earlier page and its ticket, not just the current key.
			other, pending := seedMediaDeletion(t, db, "other", media.StatusFailed)
			pending.ID, pending.UsageRecordedAt = "z_video_delete_pending", nil
			if err := jobs.CreateMediaJob(ctx, pending); err != nil {
				t.Fatal(err)
			}
			if n, err := keys.DeleteMany(ctx, []uint64{other.ID, key.ID}); n != 0 || !errors.Is(err, repository.ErrConflict) || notifications.Load() != 0 {
				t.Fatalf("mixed batch partially deleted: %d %v notify=%d", n, err, notifications.Load())
			}
			if err := peer.db.Model(&mediaJobModel{}).Count(&count).Error; err != nil || count != 206 {
				t.Fatalf("mixed-key conflict did not restore previous pages: %d %v", count, err)
			}
			if _, err := NewMediaUploadTicketRepository(peer).GetUploadTicketByHash(ctx, ticket.TokenHash); err != nil {
				t.Fatalf("mixed-key conflict revoked media ticket: %v", err)
			}
			if deleted, err := keys.DeleteMany(ctx, []uint64{key.ID}); err != nil || deleted != 1 || notifications.Load() != 1 {
				t.Fatalf("retry did not commit and notify once: %d %v notify=%d", deleted, err, notifications.Load())
			}
		})
	}
}

func TestClientKeyDeletionKeepsInternalIdentity(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, _ := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewClientKeyRepository(db)
			internal, err := repo.Create(ctx, clientkey.Key{Name: "internal", Prefix: "internal-delete", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, InternalKind: "probe"})
			if err != nil {
				t.Fatal(err)
			}
			ordinary, _ := seedMediaDeletion(t, db, "ordinary", media.StatusFailed)
			service := clientkeyapp.NewService("deletion", repo, nil, nil, 0, 0, nil, security.RandomTokenSource{})
			defer service.Close(ctx)
			if err := service.Delete(ctx, internal.ID); !errors.Is(err, clientkeyapp.ErrSystemManaged) {
				t.Fatalf("single internal identity not protected: %v", err)
			}
			if n, err := service.BatchDelete(ctx, []uint64{ordinary.ID, internal.ID}); n != 0 || !errors.Is(err, clientkeyapp.ErrSystemManaged) {
				t.Fatalf("mixed internal batch mutated: %d %v", n, err)
			}
			if _, err := repo.Get(ctx, ordinary.ID); err != nil {
				t.Fatalf("rejected mixed batch removed ordinary identity: %v", err)
			}
			if err := repo.Delete(ctx, internal.ID); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("repository single deletion bypassed internal protection: %v", err)
			}
			if n, err := repo.DeleteMany(ctx, []uint64{ordinary.ID, internal.ID}); n != 1 || err != nil {
				t.Fatalf("repository must exclude internal identity: %d %v", n, err)
			}
			if _, err := repo.Get(ctx, internal.ID); err != nil {
				t.Fatalf("repository batch removed internal identity: %v", err)
			}
		})
	}
}
