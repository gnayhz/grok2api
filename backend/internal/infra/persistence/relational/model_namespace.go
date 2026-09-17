package relational

import (
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"gorm.io/gorm"
)

// Names span route rows and compatibility aliases. Lock the Provider namespace
// before taking route row locks, including when the target name has no row yet.
// PostgreSQL transaction locks release on commit/rollback/cancellation; SQLite
// already serializes writers with its immediate transactions. Never hold this
// lock during a Provider call or a read-only inference request.
func lockModelNamespaces(tx *gorm.DB, providers ...account.Provider) error {
	wanted := make(map[account.Provider]bool, len(providers))
	for _, provider := range providers {
		if !provider.IsValid() {
			return fmt.Errorf("invalid model namespace provider %q", provider)
		}
		wanted[provider] = true
	}
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	// Persist this mapping in code: changing lock IDs across versions would
	// allow two live writers to modify the same namespace without exclusion.
	const base int64 = 0x47524f4b4d4f444c
	for index, provider := range []account.Provider{account.ProviderBuild, account.ProviderWeb, account.ProviderConsole} {
		if wanted[provider] {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", base+int64(index)).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func modelRouteProviders(rows []modelRouteModel) []account.Provider {
	providers := make([]account.Provider, 0, len(rows))
	for _, row := range rows {
		providers = append(providers, account.Provider(row.Provider))
	}
	return providers
}
