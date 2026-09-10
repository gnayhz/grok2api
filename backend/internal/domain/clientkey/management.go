package clientkey

import (
	"fmt"
	"strings"
	"time"
)

// ManagementPatch carries only explicit administrator commands. Omitted fields
// must not be restored from a previously loaded Key, including model grants.
type ManagementPatch struct {
	Name                 *string
	Enabled              *bool
	ExpiresAt            *time.Time
	ClearExpiresAt       bool
	RPMLimit             *int
	MaxConcurrent        *int
	BillingLimitUSDTicks *int64
	AllowModelAliases    *bool
	ModelScope           *ModelScope
	AllowedModels        *[]uint64
	ProviderScope        *ProviderScope
	TierScope            *TierScope
}

func (p ManagementPatch) Normalize() (ManagementPatch, error) {
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return p, fmt.Errorf("Key 名称不能为空")
		}
		p.Name = &name
	}
	if p.RPMLimit != nil && (*p.RPMLimit < 0 || *p.RPMLimit > MaxRPMLimit) {
		return p, fmt.Errorf("rpmLimit 必须在 0 到 100000 之间")
	}
	if p.MaxConcurrent != nil && (*p.MaxConcurrent < 0 || *p.MaxConcurrent > MaxConcurrent) {
		return p, fmt.Errorf("maxConcurrent 必须在 0 到 1024 之间")
	}
	if p.BillingLimitUSDTicks != nil && (*p.BillingLimitUSDTicks < 0 || *p.BillingLimitUSDTicks > MaxBillingLimitTicks) {
		return p, fmt.Errorf("billingLimitUsdTicks 超出允许范围")
	}
	if p.ProviderScope != nil {
		value, valid := NormalizeProviderScope(*p.ProviderScope)
		if !valid {
			return p, fmt.Errorf("providerScope 无效")
		}
		p.ProviderScope = &value
	}
	if p.TierScope != nil {
		value, valid := NormalizeTierScope(*p.TierScope)
		if !valid {
			return p, fmt.Errorf("tierScope 无效")
		}
		p.TierScope = &value
	}
	if p.ModelScope != nil || p.AllowedModels != nil {
		var scope ModelScope
		var ids []uint64
		if p.ModelScope != nil {
			scope = *p.ModelScope
			if scope == "" {
				return p, fmt.Errorf("modelScope 无效")
			}
		}
		if p.AllowedModels != nil {
			ids = *p.AllowedModels
		}
		value, members, err := NormalizeModelAccess(scope, ids)
		if err != nil {
			return p, err
		}
		p.ModelScope = &value
		if p.AllowedModels != nil || value == ModelScopeAll {
			p.AllowedModels = &members
		}
	}
	return p, nil
}
