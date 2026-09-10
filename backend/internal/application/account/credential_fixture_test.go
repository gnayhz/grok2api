package account

import (
	"context"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"time"
)

func rotateOAuthFixture(repo repository.AccountRepository, ctx context.Context, id uint64, accessToken, refreshToken string, expiresAt time.Time, botFlag int) (accountdomain.Credential, error) {
	current, err := repo.Get(ctx, id)
	if err != nil {
		return accountdomain.Credential{}, err
	}
	result, err := repo.ApplyCredential(ctx, current.CredentialRef(), accountdomain.CredentialEvent{Kind: accountdomain.CredentialRefreshed, AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt, BuildBotFlagSource: botFlag})
	return result.Credential, err
}
