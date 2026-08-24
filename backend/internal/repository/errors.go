package repository

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound               = errors.New("repository: not found")
	ErrConflict               = errors.New("repository: conflict")
	ErrLimitExceeded          = errors.New("repository: limit exceeded")
	ErrInvalidRecord          = errors.New("repository: invalid record")
	ErrAccountPoolMismatch    = errors.New("repository: account pool mismatch")
	ErrEgressRoutingNodeInUse = errors.New("repository: egress routing target node in use")
	ErrEgressRoutingInvalid   = errors.New("repository: egress routing target invalid")
	// ErrEgressConfigStale 报告条件写冲突:运营配置自调用方读取快照以来已被
	// 其他写入者修改。后台写者(订阅同步卫生检查)必须重读重算后重试,
	// 而不是用旧快照整行覆盖并发提交。
	ErrEgressConfigStale = errors.New("repository: egress operations config changed since read")
)

// InvalidBatchRecordError identifies a deterministic invalid record without
// classifying transient database failures as record-local errors.
type InvalidBatchRecordError struct {
	Index int
	Err   error
}

func (e *InvalidBatchRecordError) Error() string {
	if e == nil {
		return ErrInvalidRecord.Error()
	}
	if e.Err == nil {
		return fmt.Sprintf("%s at batch index %d", ErrInvalidRecord, e.Index)
	}
	return fmt.Sprintf("%s at batch index %d: %v", ErrInvalidRecord, e.Index, e.Err)
}

func (e *InvalidBatchRecordError) Unwrap() error {
	if e == nil || e.Err == nil {
		return ErrInvalidRecord
	}
	return e.Err
}

func (e *InvalidBatchRecordError) Is(target error) bool {
	return target == ErrInvalidRecord
}
