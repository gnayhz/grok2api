package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

func TestVideoUploadHandlePublishesWithoutReplacementAndAbortsSafely(t *testing.T) {
	ctx := context.Background()
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.BeginVideoUpload(ctx, "vid_handle_000000000001", "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Abort(ctx)
	second, err := store.BeginVideoUpload(ctx, "vid_handle_000000000001", "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Abort(ctx)
	if _, err := first.Write([]byte("original")); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	actual := first.(*localVideoUpload)
	file := actual.file
	if _, err := store.Open(ctx, actual.key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted object is visible: %v", err)
	}
	key, err := first.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("publication retained file descriptor: %v", err)
	}
	if _, err := second.Commit(ctx); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second commit replaced the first: %v", err)
	}
	for _, upload := range []interface{ Abort(context.Context) error }{first, second} {
		if err := upload.Abort(ctx); err != nil {
			t.Fatal(err)
		}
		if err := upload.Abort(ctx); err != nil {
			t.Fatalf("abort is not idempotent: %v", err)
		}
	}
	if repeated, err := first.Commit(ctx); err != nil || repeated != key {
		t.Fatalf("commit acknowledgement cannot repeat: %q %v", repeated, err)
	}
	if _, err := first.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write accepted after publication: %v", err)
	}
	body, err := store.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil || closeErr != nil || !bytes.Equal(raw, []byte("original")) {
		t.Fatalf("original content lost: %q %v/%v", raw, err, closeErr)
	}
	info, err := os.Stat(actual.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private file permission changed: %v %v", info, err)
	}
	objects, temps, err := store.ListMediaObjectFiles(ctx)
	if err != nil || len(objects) != 1 || len(temps) != 0 {
		t.Fatalf("unexpected objects/temps: %d/%d %v", len(objects), len(temps), err)
	}
}

func TestVideoUploadHandleFailureClosesAndRemovesStaging(t *testing.T) {
	for _, scenario := range []string{"abort", "cancel-write", "cancel-commit", "write-failure", "sync-failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store, err := NewLocalStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			upload, err := store.BeginVideoUpload(ctx, "vid_failure_000000001", "video/mp4")
			if err != nil {
				t.Fatal(err)
			}
			actual := upload.(*localVideoUpload)
			file := actual.file
			if _, err = upload.Write([]byte("first")); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "cancel-write":
				cancel()
				if _, err := upload.Write([]byte("second")); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled writer accepted data: %v", err)
				}
			case "cancel-commit":
				cancel()
				if _, err := upload.Commit(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled commit: %v", err)
				}
			case "write-failure", "sync-failure":
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				if scenario == "write-failure" {
					if _, err := upload.Write([]byte("second")); !errors.Is(err, os.ErrClosed) {
						t.Fatalf("closed writer accepted data: %v", err)
					}
				}
				if _, err := upload.Commit(ctx); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("failed file was published: %v", err)
				}
			}
			// A descriptor deliberately closed outside the owner reports its Close
			// error on the first Abort. Cleanup still has to remove the file.
			_ = upload.Abort(context.WithoutCancel(ctx))
			if err := upload.Abort(context.WithoutCancel(ctx)); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("abort retained descriptor: %v", err)
			}
			if _, err := upload.Commit(context.Background()); err == nil {
				t.Fatal("aborted upload was published")
			}
			objects, temps, err := store.ListMediaObjectFiles(context.Background())
			if err != nil || len(objects) != 0 || len(temps) != 0 {
				t.Fatalf("failed upload retained objects/temps: %d/%d %v", len(objects), len(temps), err)
			}
		})
	}
}

func TestVideoUploadHandleRetriesTemporaryRemoval(t *testing.T) {
	ctx := context.Background()
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remove := store.removeTemporary
	calls := 0
	store.removeTemporary = func(path string) error {
		calls++
		if calls == 1 {
			return errors.New("temporary removal unavailable")
		}
		return remove(path)
	}
	upload, err := store.BeginVideoUpload(ctx, "vid_cleanup_00000001", "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = upload.Write([]byte("content")); err != nil {
		t.Fatal(err)
	}
	if _, err = upload.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = upload.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	objects, temps, err := store.ListMediaObjectFiles(ctx)
	if err != nil || calls != 2 || len(objects) != 1 || len(temps) != 0 {
		t.Fatalf("cleanup retry lost object or staging: calls=%d files=%d/%d err=%v", calls, len(objects), len(temps), err)
	}
}
