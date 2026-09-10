package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalStore 将媒体对象限制在单一根目录内，并使用临时文件与原子硬链接完成提交。
type LocalStore struct {
	root            string
	removeTemporary func(string) error
}

func NewLocalStore(root string) (*LocalStore, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil || strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("媒体存储目录无效")
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("创建媒体存储目录: %w", err)
	}
	return &LocalStore{root: absolute, removeTemporary: os.Remove}, nil
}

func (s *LocalStore) SaveImage(ctx context.Context, id, mimeType string, data []byte) (string, error) {
	return s.saveObject(ctx, "images", ".image-*", id, mimeType, data, imageExtension)
}

func (s *LocalStore) saveObject(ctx context.Context, kindDir, tempPattern, id, mimeType string, data []byte, extFn func(string) (string, bool)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	extension, ok := extFn(mimeType)
	if !ok || len(id) < 2 {
		return "", fmt.Errorf("媒体存储参数无效")
	}
	storageKey := filepath.ToSlash(filepath.Join(kindDir, id[:2], id+extension))
	path, err := s.resolve(storageKey)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("创建媒体目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), tempPattern)
	if err != nil {
		return "", fmt.Errorf("创建媒体临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanupPending := true
	defer func() {
		_ = temporary.Close()
		if cleanupPending {
			if cleanupErr := s.removeTemporary(temporaryPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				slog.Warn("media_temp_cleanup_failed", "path", temporaryPath, "error", cleanupErr)
			}
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		return "", fmt.Errorf("写入媒体: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("同步媒体文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("关闭媒体文件: %w", err)
	}
	// 硬链接提交具有 no-replace 语义，极端 ID 冲突时不会覆盖已有对象。
	if err := os.Link(temporaryPath, path); err != nil {
		return "", fmt.Errorf("提交媒体文件: %w", err)
	}
	cleanupErr := s.removeTemporary(temporaryPath)
	cleanupPending = cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist)
	return storageKey, nil
}

func (s *LocalStore) Open(ctx context.Context, storageKey string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.resolve(storageKey)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("打开媒体文件: %w", err)
	}
	return file, nil
}

// ListMediaObjectFiles 枚举已提交对象与残留临时文件，返回存储相对键
// （正斜杠分隔）→ 修改时间。点前缀的是 saveObject/视频提交 的
// 中间临时文件；其余是已提交对象。孤儿回收用它对账文件系统与元数据。
func (s *LocalStore) ListMediaObjectFiles(ctx context.Context) (map[string]time.Time, map[string]time.Time, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	objects := make(map[string]time.Time)
	temps := make(map[string]time.Time)
	for _, kindDir := range []string{"images", "videos"} {
		base := filepath.Join(s.root, kindDir)
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				if errors.Is(walkErr, os.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			relative, relErr := filepath.Rel(s.root, path)
			if relErr != nil {
				return relErr
			}
			key := filepath.ToSlash(relative)
			if strings.HasPrefix(entry.Name(), ".") {
				temps[key] = info.ModTime().UTC()
			} else {
				objects[key] = info.ModTime().UTC()
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("枚举媒体目录: %w", err)
		}
	}
	return objects, temps, nil
}

func (s *LocalStore) Delete(ctx context.Context, storageKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.resolve(storageKey)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("删除媒体文件: %w", err)
	}
	return nil
}

func (s *LocalStore) resolve(storageKey string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(storageKey)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("媒体存储路径无效")
	}
	full := filepath.Join(s.root, clean)
	relative, err := filepath.Rel(s.root, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("媒体存储路径越界")
	}
	return full, nil
}

func imageExtension(mimeType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg":
		return ".jpg", true
	case "image/png":
		return ".png", true
	case "image/webp":
		return ".webp", true
	case "image/gif":
		return ".gif", true
	default:
		return "", false
	}
}
