package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/chenyme/grok2api/backend/internal/pkg/mediafile"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// BeginVideoUpload keeps the temporary file private to the driver. The returned
// handle owns it until Commit or Abort; the caller never reopens a file by path.
func (s *LocalStore) BeginVideoUpload(ctx context.Context, id, mimeType string) (repository.MediaVideoUpload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	extension, ok := mediafile.VideoExtension(mimeType)
	if !ok || len(id) < 2 {
		return nil, fmt.Errorf("视频存储参数无效")
	}
	key := filepath.ToSlash(filepath.Join("videos", id[:2], id+extension))
	path, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建视频目录: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("视频对象已存在")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".video-*")
	if err != nil {
		return nil, fmt.Errorf("创建视频临时文件: %w", err)
	}
	upload := &localVideoUpload{store: s, ctx: ctx, file: file, temp: file.Name(), key: key, path: path}
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(err, upload.Abort(context.WithoutCancel(ctx)))
	}
	return upload, nil
}

type localVideoUpload struct {
	store     *LocalStore
	ctx       context.Context
	file      *os.File
	temp      string
	key       string
	path      string
	committed bool
	failed    error
}

func (u *localVideoUpload) Write(p []byte) (int, error) {
	if err := u.ctx.Err(); err != nil {
		return 0, err
	}
	if u.file == nil || u.failed != nil {
		return 0, io.ErrClosedPipe
	}
	n, err := u.file.Write(p)
	if err != nil {
		u.failed = err
	}
	return n, err
}

func (u *localVideoUpload) Commit(ctx context.Context) (string, error) {
	if u.committed {
		return u.key, nil
	}
	if u.failed != nil {
		return "", u.failed
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if u.file == nil {
		return "", io.ErrClosedPipe
	}
	// The original writable handle provides Sync on both Unix and Windows.
	// Close is part of publication: a sync/close failure cannot publish a file.
	syncErr := u.file.Sync()
	closeErr := u.file.Close()
	u.file = nil
	if err := errors.Join(syncErr, closeErr); err != nil {
		u.failed = fmt.Errorf("同步或关闭视频文件: %w", err)
		return "", u.failed
	}
	if err := ctx.Err(); err != nil {
		u.failed = err
		return "", err
	}
	if err := os.Link(u.temp, u.path); err != nil {
		u.failed = fmt.Errorf("提交视频文件: %w", err)
		return "", u.failed
	}
	u.committed = true
	if err := u.store.removeTemporary(u.temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("media_temp_cleanup_failed", "path", u.temp, "error", err)
	}
	return u.key, nil
}

func (u *localVideoUpload) Abort(_ context.Context) error {
	var closeErr error
	if u.file != nil {
		closeErr = u.file.Close()
		u.file = nil
	}
	removeErr := u.store.removeTemporary(u.temp)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}
