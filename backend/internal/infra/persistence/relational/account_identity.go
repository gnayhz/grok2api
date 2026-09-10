package relational

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// ListIdentityLinks returns both association types from one database snapshot.
// The UNION is one statement: a concurrent link/delete transaction cannot
// produce a mixture of the two tables from before and after its commit.
func (r *AccountRepository) ListIdentityLinks(ctx context.Context) ([]account.IdentityLink, error) {
	var links []account.IdentityLink
	err := r.db.db.WithContext(ctx).Raw(`
        SELECT web_account_id AS account_id, build_account_id AS related_account_id
        FROM account_provider_links
        UNION ALL
        SELECT web_account_id AS account_id, console_account_id AS related_account_id
        FROM web_console_account_links
    `).Scan(&links).Error
	return links, err
}
