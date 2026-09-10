package media

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

// mediaObjectLister 是对象存储的可选枚举能力（本地驱动实现）。不支持时
// 孤儿回收静默跳过，存储后端行为不变。
type mediaObjectLister interface {
	ListMediaObjectFiles(ctx context.Context) (objects, temps map[string]time.Time, err error)
}

const (
	// orphanSweepInterval 孤儿对账频率：崩溃残留积累速率极低，日频足够。
	orphanSweepInterval = 24 * time.Hour
	// orphanGracePeriod 是文件修改时间宽限期：覆盖多实例共享存储下
	// 「对象已硬链接提交、元数据行对其他实例尚未可见」的竞态窗口
	// （实际为毫秒级），同时容忍时钟偏移。
	orphanGracePeriod = 24 * time.Hour
)

// sweepOrphanObjects 对账文件系统与元数据，回收硬链接提交后、元数据行
// 写入前崩溃残留的孤儿对象与过期临时文件。Cleanup 只枚举 DB 行，这类
// 文件对其不可见且不计入容量统计——不回收则永久泄漏并绕过 MaxTotalBytes。
// 有 DB 行的对象与宽限期内的文件一律保留；删除幂等（ErrNotExist 容忍），
// 多实例并发对账安全。返回删除的文件数。
func (s *Service) sweepOrphanObjects(ctx context.Context, now time.Time) (int, error) {
	lister, ok := s.objects.(mediaObjectLister)
	if !ok {
		return 0, nil
	}
	objects, temps, err := lister.ListMediaObjectFiles(ctx)
	if err != nil {
		return 0, err
	}
	if len(objects) == 0 && len(temps) == 0 {
		return 0, nil
	}
	deleted := 0
	cutoff := now.Add(-orphanGracePeriod)
	remove := func(key string) error {
		if err := s.objects.Delete(ctx, key); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	// A missing entry in a concurrently paginated table is not evidence of an
	// orphan. Query each bounded set of actual candidates through the key index.
	batch := make([]string, 0, repository.MaxMediaAssetLookupKeys)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.assets == nil {
			return errors.New("media asset repository unavailable")
		}
		known, err := s.assets.FindMediaAssetStorageKeys(ctx, batch)
		if err != nil {
			return err
		}
		for _, key := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, live := known[key]; live {
				continue
			}
			if err := remove(key); err != nil {
				return err
			}
			deleted++
		}
		batch = batch[:0]
		return nil
	}
	for key, modified := range objects {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		if modified.After(cutoff) {
			continue
		}
		batch = append(batch, key)
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return deleted, err
			}
		}
	}
	if err := flush(); err != nil {
		return deleted, err
	}
	for key, modified := range temps {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		if modified.After(cutoff) {
			continue
		}
		if err := remove(key); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}
