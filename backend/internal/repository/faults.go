package repository

import (
	"errors"
	"fmt"
)

// StoreFaultKind 是存储适配器报告的稳定故障类别。消费方按操作目的决定
// 动作（容忍旧快照、跳过单号、重试或失败），不再解释驱动错误细节。
type StoreFaultKind string

const (
	// StoreFaultConnection 表示连接级故障（断开、坏连接、驱动连接异常）。
	StoreFaultConnection StoreFaultKind = "connection"
	// StoreFaultLock 表示行锁/表锁暂不可用（SQLite BUSY/LOCKED、
	// PostgreSQL lock_not_available）。
	StoreFaultLock StoreFaultKind = "lock"
	// StoreFaultSerialization 表示序列化失败/可重试的事务冲突。
	StoreFaultSerialization StoreFaultKind = "serialization"
	// StoreFaultTimeout 表示存储侧超时；调用方 deadline/cancel 不属于
	// 存储故障，仍由 context 错误表达。
	StoreFaultTimeout StoreFaultKind = "timeout"
	// StoreFaultConstraint 表示约束冲突类持久失败。
	StoreFaultConstraint StoreFaultKind = "constraint"
	// StoreFaultUnknown 表示未归类故障；消费方不得将其当作瞬态。
	StoreFaultUnknown StoreFaultKind = "unknown"
)

// StoreFault 携带稳定类别并保留原因错误；errors.Is/As 可穿透到原错误。
type StoreFault struct {
	Kind  StoreFaultKind
	Cause error
}

func (e *StoreFault) Error() string {
	if e == nil {
		return "store fault"
	}
	if e.Cause == nil {
		return fmt.Sprintf("store fault: %s", e.Kind)
	}
	return fmt.Sprintf("store fault: %s: %v", e.Kind, e.Cause)
}

func (e *StoreFault) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// StoreFaultOf 返回错误链中的第一个存储故障。
func StoreFaultOf(err error) (*StoreFault, bool) {
	var fault *StoreFault
	if errors.As(err, &fault) {
		return fault, true
	}
	return nil, false
}

// StoreFaultKindOf 返回错误的存储故障类别；非存储故障返回 false。
// 消费方用它取代对 database/sql/driver、SQLite Code() 或 SQLSTATE 的
// 直接解释。
func StoreFaultKindOf(err error) (StoreFaultKind, bool) {
	fault, ok := StoreFaultOf(err)
	if !ok || fault == nil {
		return "", false
	}
	return fault.Kind, true
}

// StoreFaultTransient 报告某类别是否允许按"重试同一操作"处理。
//
// 只有连接、锁、序列化与超时四类是一次性冲突；约束冲突是确定性结果，
// 未归类故障语义未知，二者都不得被当作瞬态跳过或重试（否则确定性失败
// 会被放大成对其它候选的无效尝试）。消费方统一调用本函数，不要各自
// 维护一份类别白名单。
func StoreFaultTransient(kind StoreFaultKind) bool {
	switch kind {
	case StoreFaultConnection, StoreFaultLock, StoreFaultSerialization, StoreFaultTimeout:
		return true
	default:
		return false
	}
}
