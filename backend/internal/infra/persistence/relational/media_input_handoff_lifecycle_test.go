package relational

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type mediaInputHandoffFixture struct {
	db, peer        *Database
	owner, releaser *mediaapp.Service
	input           media.Asset
	picture         []byte
	job             media.Job
}

func newMediaInputHandoffFixture(t *testing.T, dialect string) mediaInputHandoffFixture {
	t.Helper()
	db, peer := settingsDatabasePair(t, dialect)
	objects, err := localmedia.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := mediaapp.Config{MaxImageBytes: 1 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80}
	owner := mediaapp.NewServiceWithTickets(NewMediaAssetRepository(db), NewMediaJobRepository(db), nil, objects, nil, cfg)
	releaser := mediaapp.NewServiceWithTickets(NewMediaAssetRepository(peer), NewMediaJobRepository(peer), nil, objects, nil, cfg)
	picture, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	input, err := owner.SaveInputImage(context.Background(), picture)
	if err != nil {
		t.Fatal(err)
	}
	_, job := seedMediaDeletion(t, db, "handoff", media.StatusQueued)
	job.InputJSON = fmt.Sprintf(`{"image_url":%q}`, media.InputReference(input.ID))
	return mediaInputHandoffFixture{db: db, peer: peer, owner: owner, releaser: releaser, input: input, picture: picture, job: job}
}
func (fx mediaInputHandoffFixture) release(ctx context.Context) error {
	return fx.releaser.ReleaseInputAssets(ctx, []string{media.InputReference(fx.input.ID)})
}
func (fx mediaInputHandoffFixture) assertInput(t *testing.T, available bool) {
	t.Helper()
	_, body, err := fx.owner.OpenInputAsset(context.Background(), fx.input.ID)
	if !available {
		if body != nil {
			_ = body.Close()
		}
		if !errors.Is(err, mediaapp.ErrInputAssetNotFound) {
			t.Fatalf("input remains available: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	_, err = data.ReadFrom(body)
	_ = body.Close()
	if err != nil || !bytes.Equal(data.Bytes(), fx.picture) {
		t.Fatalf("input bytes changed: %v", err)
	}
}
func assertNoHandoffJob(t *testing.T, fx mediaInputHandoffFixture) {
	t.Helper()
	_, err := NewMediaJobRepository(fx.peer).GetMediaJob(context.Background(), fx.job.ID, fx.job.ClientKeyID)
	if !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("rejected job persisted: %v", err)
	}
}

func TestMediaJobInputHandoffConcurrentCommitOrders(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, order := range []string{"create_first", "release_first"} {
			t.Run(dialect+"/"+order, func(t *testing.T) {
				fx := newMediaInputHandoffFixture(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				entered, proceed := make(chan struct{}), make(chan struct{})
				defer func() {
					select {
					case <-proceed:
					default:
						close(proceed)
					}
				}()
				hook := func(tx *gorm.DB) {
					table := "media_jobs"
					if order == "release_first" {
						table = "media_assets"
					}
					if tx.Statement.Table != table {
						return
					}
					close(entered)
					select {
					case <-proceed:
					case <-ctx.Done():
						tx.AddError(ctx.Err())
					}
				}
				if order == "create_first" {
					if err := fx.db.db.Callback().Create().After("gorm:create").Before("gorm:commit_or_rollback_transaction").Register("handoff_commit_barrier", hook); err != nil {
						t.Fatal(err)
					}
					defer fx.db.db.Callback().Create().Remove("handoff_commit_barrier")
				} else {
					if err := fx.peer.db.Callback().Update().After("gorm:update").Before("gorm:commit_or_rollback_transaction").Register("handoff_commit_barrier", hook); err != nil {
						t.Fatal(err)
					}
					defer fx.peer.db.Callback().Update().Remove("handoff_commit_barrier")
				}
				first, second := make(chan error, 1), make(chan error, 1)
				create := func() error { return NewMediaJobRepository(fx.db).CreateMediaJob(ctx, fx.job) }
				release := func() error { return fx.release(ctx) }
				if order == "create_first" {
					go func() { first <- create() }()
				} else {
					go func() { first <- release() }()
				}
				select {
				case <-entered:
				case err := <-first:
					t.Fatalf("no commit barrier: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if order == "create_first" {
					go func() { second <- release() }()
				} else {
					go func() { second <- create() }()
				}
				if dialect == "postgres" {
					waitMediaDeletionLock(t, ctx, fx.db, "media_assets", second)
				} else {
					// SQLite's second writer waits in BEGIN IMMEDIATE before any query callback.
					select {
					case err := <-second:
						t.Fatalf("second operation bypassed uncommitted owner: %v", err)
					case <-time.After(30 * time.Millisecond):
					}
				}
				close(proceed)
				if err := <-first; err != nil {
					t.Fatal(err)
				}
				err := <-second
				if order == "release_first" {
					if !errors.Is(err, media.ErrVideoInputUnavailable) {
						t.Fatalf("create after release=%v", err)
					}
					assertNoHandoffJob(t, fx)
					fx.assertInput(t, false)
				} else {
					if err != nil {
						t.Fatal(err)
					}
					fx.assertInput(t, true)
				}
			})
		}
	}
}

func TestMediaJobInputHandoffRetainsOnlyActualActiveReferences(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, shape := range []string{"legacy", "escaped", "split_overrides_legacy", "voice_id_only", "unknown_field"} {
			t.Run(dialect+"/"+shape, func(t *testing.T) {
				fx := newMediaInputHandoffFixture(t, dialect)
				ctx := context.Background()
				ref := media.InputReference(fx.input.ID)
				switch shape {
				case "legacy":
					fx.job.InputJSON = fmt.Sprintf(`{"image_urls":[%q]}`, ref)
				case "escaped":
					fx.job.InputJSON = strings.ReplaceAll(fmt.Sprintf(`{"image_url":%q}`, ref), "input_", `input\u005f`)
				case "split_overrides_legacy":
					fx.job.InputJSON = fmt.Sprintf(`{"image_url":"https://remote.invalid/image","image_urls":[%q]}`, ref)
				case "voice_id_only":
					fx.job.InputJSON = fmt.Sprintf(`{"reference_audios":[%q]}`, ref)
				case "unknown_field":
					fx.job.InputJSON = fmt.Sprintf(`{"future":%q}`, ref)
				}
				if err := NewMediaJobRepository(fx.db).CreateMediaJob(ctx, fx.job); err != nil {
					t.Fatal(err)
				}
				if err := fx.release(ctx); err != nil {
					t.Fatal(err)
				}
				fx.assertInput(t, shape == "legacy" || shape == "escaped")
			})
		}
	}
}

func TestMediaJobInputHandoffSharedLifetimeAndHardTTL(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, hardTTL := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/hard_ttl_%v", dialect, hardTTL), func(t *testing.T) {
				fx := newMediaInputHandoffFixture(t, dialect)
				ctx := context.Background()
				jobs := NewMediaJobRepository(fx.db)
				if err := jobs.CreateMediaJob(ctx, fx.job); err != nil {
					t.Fatal(err)
				}
				second := fx.job
				second.ID += "_peer"
				second.RequestID += "_peer"
				if err := NewMediaJobRepository(fx.peer).CreateMediaJob(ctx, second); err != nil {
					t.Fatal(err)
				}
				fx.job.Status = media.StatusFailed
				if err := jobs.UpdateMediaJob(ctx, fx.job); err != nil {
					t.Fatal(err)
				}
				if err := fx.release(ctx); err != nil {
					t.Fatal(err)
				}
				fx.assertInput(t, true)
				if hardTTL {
					// Age the persisted hard deadline after a valid creation, rather than
					// constructing a new job with an already expired input.
					if err := fx.peer.db.Model(&mediaAssetModel{}).Where("id = ?", fx.input.ID).Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
						t.Fatal(err)
					}
					fx.assertInput(t, false)
					if err := fx.release(ctx); err != nil {
						t.Fatal(err)
					}
					if _, err := NewMediaAssetRepository(fx.db).GetMediaAsset(ctx, fx.input.ID); err != nil {
						t.Fatalf("release ignored active job: %v", err)
					}
					if deleted, err := fx.releaser.Cleanup(ctx); err != nil || deleted != 1 {
						t.Fatalf("hard TTL cleanup=%d %v", deleted, err)
					}
				} else {
					second.Status = media.StatusFailed
					if err := NewMediaJobRepository(fx.peer).UpdateMediaJob(ctx, second); err != nil {
						t.Fatal(err)
					}
					if err := fx.release(ctx); err != nil {
						t.Fatal(err)
					}
					fx.assertInput(t, false)
				}
			})
		}
	}
}

func TestMediaJobInputHandoffRejectsUnavailableMetadata(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, fault := range []string{"expired", "wrong_kind", "persistent_asset", "malformed_id"} {
			t.Run(dialect+"/"+fault, func(t *testing.T) {
				fx := newMediaInputHandoffFixture(t, dialect)
				ctx := context.Background()
				update := map[string]any{}
				switch fault {
				case "expired":
					update["expires_at"] = time.Now().UTC().Add(-time.Minute)
				case "wrong_kind":
					update["kind"] = "video"
				case "persistent_asset":
					update["expires_at"] = nil
				case "malformed_id":
					fx.job.InputJSON = `{"image_url":"grok2api-input:not_a_private_input"}`
				}
				if len(update) > 0 {
					if err := fx.db.db.Model(&mediaAssetModel{}).Where("id = ?", fx.input.ID).Updates(update).Error; err != nil {
						t.Fatal(err)
					}
				}
				err := NewMediaJobRepository(fx.peer).CreateMediaJob(ctx, fx.job)
				if !errors.Is(err, media.ErrVideoInputUnavailable) {
					t.Fatalf("invalid metadata was accepted: %v", err)
				}
				assertNoHandoffJob(t, fx)
			})
		}
	}
}
