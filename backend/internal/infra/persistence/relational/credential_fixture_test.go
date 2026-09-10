package relational

import (
	"context"
	account "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"time"
)

func rotateOAuthFixture(repo repository.AccountRepository, ctx context.Context, id uint64, accessToken, refreshToken string, expiresAt time.Time, botFlag int) (account.Credential, error) {
	current, err := repo.Get(ctx, id)
	if err != nil {
		return account.Credential{}, err
	}
	result, err := repo.ApplyCredential(ctx, current.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRefreshed, AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt, BuildBotFlagSource: botFlag})
	return result.Credential, err
}
