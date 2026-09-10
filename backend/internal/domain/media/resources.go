package media

import "errors"

var (
	// ErrAssetNotFound is a confirmed missing or non-public asset, not a storage failure.
	ErrAssetNotFound = errors.New("媒体资源不存在")
	ErrVideoNotFound = errors.New("视频任务不存在")
	// ErrVideoResourceRead marks local resource availability failures. The cause
	// remains internal; neither job status nor upstream health changes on this error.
	ErrVideoResourceRead = errors.New("视频资源状态暂不可用")
)
