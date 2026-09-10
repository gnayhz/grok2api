package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/google/uuid"
)

type providerLinkRepository interface {
	ReconcileProviderLinks(ctx context.Context, accountID uint64) error
	ApplyIdentity(ctx context.Context, observed accountdomain.CredentialRef, identity accountdomain.IdentityObservation) (accountdomain.IdentityResult, error)
}

// SyncAccountIdentity best-effort fills stable Web/Console identity metadata and reconciles trusted links.
// Definitive unauthorized signals mark the current Provider account as reauthRequired and remove it from scheduling;
// other synchronization failures do not affect account health.
func (s *Service) SyncAccountIdentity(ctx context.Context, id uint64) error {
	links, ok := s.accounts.(providerLinkRepository)
	if !ok {
		return nil
	}
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return mapRepositoryError(err)
	}
	_, err = s.identitySyncs.Do(ctx, value.CredentialRef(), func() (any, error) {
		return nil, s.syncAccountIdentity(ctx, value, links)
	})
	return err
}

func (s *Service) syncAccountIdentity(ctx context.Context, value accountdomain.Credential, links providerLinkRepository) error {
	id := value.ID
	if value.Provider != accountdomain.ProviderWeb && value.Provider != accountdomain.ProviderConsole {
		return nil
	}
	// Web Gateway 要求 uid 为 UUID，因此旧账号即使已经保存 email，也必须用
	// SSO Session 补齐或纠正 user_id。Console 不依赖 Gateway uid，继续沿用
	// 任一稳定身份字段已存在即不重复访问上游的行为。
	if accountIdentityComplete(value) {
		return mapRepositoryError(links.ReconcileProviderLinks(ctx, id))
	}
	if s.providers == nil {
		return fmt.Errorf("Provider 注册表未初始化")
	}
	adapter, ok := s.providers.AccountIdentity(value.Provider)
	if !ok {
		return nil
	}
	identity, err := adapter.SyncAccountIdentity(ctx, value)
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			markErr := s.markSSOCredentialRejected(ctx, value, fmt.Sprintf("%s SSO credential rejected", value.Provider))
			return errors.Join(err, markErr)
		}
		return err
	}
	if value.Provider == accountdomain.ProviderWeb {
		identityID, parseErr := uuid.Parse(strings.TrimSpace(identity.UserID))
		if parseErr != nil || identityID == uuid.Nil {
			return fmt.Errorf("Grok Web Session 未返回合法的 Gateway 用户 UUID")
		}
	}
	_, err = links.ApplyIdentity(ctx, value.CredentialRef(), accountdomain.IdentityObservation{Email: identity.Email, UserID: identity.UserID, TeamID: identity.TeamID})
	return mapRepositoryError(err)
}

func accountIdentityComplete(value accountdomain.Credential) bool {
	if value.Provider == accountdomain.ProviderWeb {
		identityID, err := uuid.Parse(strings.TrimSpace(value.UserID))
		return err == nil && identityID != uuid.Nil
	}
	return strings.TrimSpace(value.UserID) != "" || strings.TrimSpace(value.Email) != ""
}

func (s *Service) reconcileProviderLinksBestEffort(ctx context.Context, id uint64) {
	links, ok := s.accounts.(providerLinkRepository)
	if !ok {
		return
	}
	if err := links.ReconcileProviderLinks(ctx, id); err != nil {
		s.logger.Warn("account_provider_link_reconcile_failed", "account_id", id, "error", err)
	}
}

func (s *Service) syncAccountIdentityBestEffort(ctx context.Context, id uint64) error {
	if err := s.SyncAccountIdentity(ctx, id); err != nil {
		s.logger.Warn("account_identity_sync_failed", "account_id", id, "error", err)
		return err
	}
	return nil
}
