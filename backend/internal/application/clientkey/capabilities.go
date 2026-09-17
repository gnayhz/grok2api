package clientkey

import (
	"context"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

// Administration 是客户端 Key 的管理面能力：创建、更新、删除、批量操作、
// 列表与密钥揭示。认证、计费预留与 touch 不在管理合同内。
type Administration interface {
	Create(ctx context.Context, input CreateInput) (Created, error)
	Update(ctx context.Context, id uint64, input UpdateInput) (clientkeydomain.Key, error)
	Delete(ctx context.Context, id uint64) error
	BatchSetEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error)
	BatchDelete(ctx context.Context, ids []uint64) (int64, error)
	List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]clientkeydomain.Key, int64, error)
	RevealSecret(ctx context.Context, id uint64) (string, error)
}

// Authorization 是执行入口的认证与授权消费面。
type Authorization interface {
	Authenticate(ctx context.Context, raw string) (clientkeydomain.Key, func(), error)
	CanUseModel(value clientkeydomain.Key, modelID uint64) bool
	Get(ctx context.Context, id uint64) (clientkeydomain.Key, error)
}

// BillingReservations 能力已删除：gateway 侧使用自定义窄接口消费
// ReserveBilling/CancelBilling，本包无需再导出该能力合同（方法保留在 Service 上）。

var (
	_ Administration = (*Service)(nil)
	_ Authorization  = (*Service)(nil)
)
