package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

func uploadExpiryDatabasePair(t *testing.T, dialect string) (*relational.Database, *relational.Database) {
	t.Helper()
	ctx := context.Background()
	var open func() (*relational.Database, error)
	if dialect == "sqlite" {
		path := filepath.Join(t.TempDir(), "upload.db")
		open = func() (*relational.Database, error) { return relational.OpenSQLite(ctx, path) }
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("isolated TEST_POSTGRES_DSN required")
		}
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("g44_upload_%d", time.Now().UnixNano())
		if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			_ = admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			defer admin.Close()
			if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		open = func() (*relational.Database, error) { return relational.OpenPostgres(ctx, parsed.String(), 4, 2) }
	}
	db, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	peer, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	return db, peer
}

type expiryObservedTickets struct {
	repository.MediaUploadTicketRepository
	preflight chan struct{}
}

func (r *expiryObservedTickets) GetUploadTicketByHash(ctx context.Context, hash string) (repository.MediaUploadTicket, error) {
	ticket, err := r.MediaUploadTicketRepository.GetUploadTicketByHash(ctx, hash)
	select {
	case r.preflight <- struct{}{}:
	default:
	}
	return ticket, err
}

func TestHTTPUploadExpiredDuringBodyCannotCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, scenario := range []string{"valid", "already-expired", "expires-during-body"} {
			t.Run(dialect+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				db, peer := uploadExpiryDatabasePair(t, dialect)
				disk, err := localmedia.NewLocalStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				source := relational.NewMediaUploadTicketRepository(db)
				observer := &expiryObservedTickets{MediaUploadTicketRepository: source, preflight: make(chan struct{}, 1)}
				service := mediaapp.NewServiceWithTickets(relational.NewMediaAssetRepository(db), relational.NewMediaJobRepository(db), observer, disk, nil, mediaapp.Config{PublicBaseURL: "https://api.example", MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80})
				router := gin.New()
				NewHandler(service, nil).RegisterPublic(router)
				server := httptest.NewServer(router)
				defer server.Close()
				token := strings.Repeat("a", 64)
				sum := sha256.Sum256([]byte(token))
				hash := hex.EncodeToString(sum[:])
				now := time.Now().UTC()
				expiry := now.Add(5 * time.Second)
				if scenario == "already-expired" {
					expiry = now.Add(-time.Second)
				}
				if scenario == "expires-during-body" {
					expiry = now.Add(300 * time.Millisecond)
				}
				assetID := "vid_expires_during_upload_0001"
				if err = source.CreateUploadTicket(ctx, repository.MediaUploadTicket{TokenHash: hash, AssetID: assetID, JobID: "expiry_job", MaxBytes: 1024, AllowedMIME: "video/mp4", CreatedAt: now, ExpiresAt: expiry}); err != nil {
					t.Fatal(err)
				}
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				req, err := http.NewRequest(http.MethodPut, server.URL+"/v1/media/uploads/"+token, reader)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "video/mp4")
				type result struct {
					status int
					err    error
				}
				finished := make(chan result, 1)
				go func() {
					resp, err := server.Client().Do(req)
					if err != nil {
						finished <- result{err: err}
						return
					}
					_, err = io.Copy(io.Discard, resp.Body)
					closeErr := resp.Body.Close()
					finished <- result{status: resp.StatusCode, err: errors.Join(err, closeErr)}
				}()
				select {
				case <-observer.preflight:
				case <-time.After(5 * time.Second):
					t.Fatal("HTTP preflight never read ticket")
				}
				if scenario == "expires-during-body" {
					time.Sleep(time.Until(expiry.Add(20 * time.Millisecond)))
				}
				payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 64)...)
				if scenario != "already-expired" {
					if _, err = writer.Write(payload); err != nil {
						t.Fatal(err)
					}
				}
				if err = writer.Close(); err != nil {
					t.Fatal(err)
				}
				var got result
				select {
				case got = <-finished:
				case <-time.After(5 * time.Second):
					t.Fatal("upload did not finish")
				}
				if got.err != nil {
					t.Fatal(got.err)
				}
				if scenario == "valid" {
					if got.status != http.StatusNoContent {
						t.Fatalf("valid upload status=%d", got.status)
					}
					ticket, err := relational.NewMediaUploadTicketRepository(peer).GetUploadTicketByHash(ctx, hash)
					if err != nil || ticket.ConsumedAt == nil {
						t.Fatalf("valid ticket not consumed: %v", err)
					}
					response, err := server.Client().Get(server.URL + "/v1/media/videos/" + assetID)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := io.ReadAll(response.Body)
					closeErr := response.Body.Close()
					if err != nil || closeErr != nil || response.StatusCode != http.StatusOK || !bytes.Equal(raw, payload) {
						t.Fatalf("committed HTTP content: status=%d err=%v/%v", response.StatusCode, err, closeErr)
					}
					replay, err := http.NewRequest(http.MethodPut, server.URL+"/v1/media/uploads/"+token, bytes.NewReader(payload))
					if err != nil {
						t.Fatal(err)
					}
					replay.Header.Set("Content-Type", "video/mp4")
					response, err = server.Client().Do(replay)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
					if response.StatusCode != http.StatusConflict {
						t.Fatalf("ticket replay status=%d", response.StatusCode)
					}
					return
				}
				if got.status != http.StatusGone {
					t.Errorf("expired upload accepted: status=%d want410", got.status)
				}
				ticket, err := relational.NewMediaUploadTicketRepository(peer).GetUploadTicketByHash(ctx, hash)
				if err != nil {
					t.Fatal(err)
				}
				if ticket.ConsumedAt != nil {
					t.Error("expired ticket was consumed")
				}
				if _, err = relational.NewMediaAssetRepository(peer).GetMediaAsset(ctx, assetID); !errors.Is(err, repository.ErrNotFound) {
					t.Errorf("expired upload registered asset: %v", err)
				}
				objects, temps, err := disk.ListMediaObjectFiles(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(objects) != 0 || len(temps) != 0 {
					t.Errorf("expired upload left files: objects=%d temps=%d", len(objects), len(temps))
				}
			})
		}
	}
}
