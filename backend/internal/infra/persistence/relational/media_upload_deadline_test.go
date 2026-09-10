package relational

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type uploadDeadlineTickets struct {
	repository.MediaUploadTicketRepository
	before func(context.Context) error
}

func (r *uploadDeadlineTickets) ConsumeUploadTicket(ctx context.Context, hash string, now time.Time) (repository.MediaUploadTicket, bool, error) {
	if err := r.before(ctx); err != nil {
		return repository.MediaUploadTicket{}, false, err
	}
	return r.MediaUploadTicketRepository.ConsumeUploadTicket(ctx, hash, now)
}

type uploadDeadlineObjects struct {
	repository.MediaObjectStorage
	before func(context.Context) error
}

type uploadDeadlineAssets struct {
	repository.MediaAssetRepository
	before func(context.Context) error
	done   chan struct{}
}

func (r *uploadDeadlineAssets) CreateMediaAsset(ctx context.Context, asset mediadomain.Asset) error {
	defer close(r.done)
	if err := r.before(ctx); err != nil {
		return err
	}
	return r.MediaAssetRepository.CreateMediaAsset(ctx, asset)
}

func (s *uploadDeadlineObjects) BeginVideoUpload(ctx context.Context, id, mime string) (repository.MediaVideoUpload, error) {
	upload, err := s.MediaObjectStorage.BeginVideoUpload(ctx, id, mime)
	if err != nil {
		return nil, err
	}
	return &deadlineVideoUpload{MediaVideoUpload: upload, owner: s}, nil
}

type deadlineVideoUpload struct {
	repository.MediaVideoUpload
	owner *uploadDeadlineObjects
}

func (u *deadlineVideoUpload) Commit(ctx context.Context) (string, error) {
	if u.owner.before != nil {
		f := u.owner.before
		u.owner.before = nil
		if err := f(ctx); err != nil {
			return "", err
		}
	}
	return u.MediaVideoUpload.Commit(ctx)
}

func TestUploadTicketDeadlineBoundsSQLAndPublication(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, scenario := range []string{"expiry-sql", "cancel-sql", "expiry-commit", "cancel-commit", "expiry-register", "cancel-register"} {
			t.Run(dialect+"/"+scenario, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				disk, err := localmedia.NewLocalStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				source := NewMediaUploadTicketRepository(db)
				token := strings.Repeat("b", 64)
				sum := sha256.Sum256([]byte(token))
				hash := hex.EncodeToString(sum[:])
				now := time.Now().UTC()
				expiry := now.Add(time.Minute)
				if strings.HasPrefix(scenario, "expiry") {
					expiry = now.Add(400 * time.Millisecond)
				}
				assetID := "vid_upload_deadline_00000001"
				if err = source.CreateUploadTicket(ctx, repository.MediaUploadTicket{TokenHash: hash, AssetID: assetID, JobID: "deadline_job", MaxBytes: 1024, AllowedMIME: "video/mp4", CreatedAt: now, ExpiresAt: expiry}); err != nil {
					t.Fatal(err)
				}
				type barrier struct {
					release  func()
					deadline time.Time
					bounded  bool
				}
				entered := make(chan barrier, 1)
				var tickets repository.MediaUploadTicketRepository = source
				var assets repository.MediaAssetRepository = NewMediaAssetRepository(db)
				var objects repository.MediaObjectStorage = disk
				var registered chan struct{}
				if strings.HasSuffix(scenario, "sql") || strings.HasSuffix(scenario, "register") {
					before := func(work context.Context) error {
						var release func()
						if dialect == "sqlite" {
							pool, err := db.db.DB()
							if err != nil {
								return err
							}
							pool.SetMaxOpenConns(1)
							conn, err := pool.Conn(ctx)
							if err != nil {
								return err
							}
							release = func() { _ = conn.Close() }
						} else {
							tx := peer.db.Begin()
							if tx.Error != nil {
								return tx.Error
							}
							table := "media_upload_tickets"
							if strings.HasSuffix(scenario, "register") {
								table = "media_assets"
							}
							if err := tx.Exec("LOCK TABLE " + table + " IN ACCESS EXCLUSIVE MODE").Error; err != nil {
								_ = tx.Rollback()
								return err
							}
							release = func() { _ = tx.Rollback() }
						}
						deadline, bounded := work.Deadline()
						entered <- barrier{release, deadline, bounded}
						return nil
					}
					if strings.HasSuffix(scenario, "register") {
						registered = make(chan struct{})
						assets = &uploadDeadlineAssets{MediaAssetRepository: assets, before: before, done: registered}
					} else {
						tickets = &uploadDeadlineTickets{MediaUploadTicketRepository: source, before: before}
					}
				} else {
					objects = &uploadDeadlineObjects{MediaObjectStorage: disk, before: func(work context.Context) error {
						deadline, bounded := work.Deadline()
						entered <- barrier{func() {}, deadline, bounded}
						<-work.Done()
						return work.Err()
					}}
				}
				service := mediaapp.NewServiceWithTickets(assets, NewMediaJobRepository(db), tickets, objects, nil, mediaapp.Config{MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80})
				payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 64)...)
				finished := make(chan error, 1)
				go func() {
					_, err := service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload))
					finished <- err
				}()
				var gate barrier
				select {
				case gate = <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("upload did not reach blocked operation")
				}
				defer func() { gate.release() }()
				if !gate.bounded || gate.deadline.After(expiry) {
					t.Errorf("upload publication not bounded by ticket: deadline=%v bounded=%v", gate.deadline, gate.bounded)
				}
				if strings.HasPrefix(scenario, "cancel") {
					cancel()
				}
				if registered != nil {
					select {
					case <-registered:
						// The actual INSERT has returned after its deadline/cancel.
						// Let compensating ticket release reacquire SQL resources.
						gate.release()
						gate.release = func() {}
					case <-time.After(2 * time.Second):
						t.Fatal("asset registration ignored upload lifetime")
					}
				}
				select {
				case err = <-finished:
				case <-time.After(2 * time.Second):
					cancel()
					t.Fatal("upload did not stop within ticket/request lifetime")
				}
				gate.release()
				gate.release = func() {}
				if strings.HasPrefix(scenario, "expiry") {
					if !errors.Is(err, mediaapp.ErrUploadTicketExpired) {
						t.Fatalf("expiry classification: %v", err)
					}
				} else if !errors.Is(err, context.Canceled) || errors.Is(err, mediaapp.ErrUploadTicketExpired) {
					t.Fatalf("request cancellation classification: %v", err)
				}
				ticket, err := NewMediaUploadTicketRepository(peer).GetUploadTicketByHash(context.Background(), hash)
				if err != nil || ticket.ConsumedAt != nil {
					t.Fatalf("failed upload retained consumption: %+v %v", ticket, err)
				}
				if _, err = NewMediaAssetRepository(peer).GetMediaAsset(context.Background(), assetID); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("failed upload registered asset: %v", err)
				}
				files, temps, err := disk.ListMediaObjectFiles(context.Background())
				if err != nil || len(files) != 0 || len(temps) != 0 {
					t.Fatalf("failed upload retained files: %d/%d %v", len(files), len(temps), err)
				}
				if scenario == "cancel-commit" {
					asset, err := service.ReceiveVideoUpload(context.Background(), token, "video/mp4", bytes.NewReader(payload))
					if err != nil {
						t.Fatalf("valid retry failed: %v", err)
					}
					_, body, err := service.OpenVideo(context.Background(), asset.ID)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := io.ReadAll(body)
					closeErr := body.Close()
					if err != nil || closeErr != nil || !bytes.Equal(raw, payload) {
						t.Fatalf("retry content: %v %v", err, closeErr)
					}
				}
			})
		}
	}
}
