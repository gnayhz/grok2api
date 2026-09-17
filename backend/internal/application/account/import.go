package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/tokenhash"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type ImportResult struct {
	Created    int
	Updated    int
	Skipped    int
	Failed     int
	AccountIDs []uint64
}

type ImportedAccountObserver func(accountID uint64) error

// ImportCredentialDocumentsWithProgress 合并解析多个 Build 凭据文件，并作为一个批次写入和同步。
func (s *Service) ImportCredentialDocumentsWithProgress(ctx context.Context, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	adapter, ok := s.providers.CredentialCodec(accountdomain.ProviderBuild)
	if !ok {
		return ImportResult{}, fmt.Errorf("CLI Provider 未注册")
	}
	return s.importCredentialDocumentsWithProgress(ctx, adapter, documents, observer, progress)
}

// ImportWebCredentialDocumentsWithProgress 合并解析多个 Web JSON 或 SSO 文本文件，并作为一个批次写入和同步。
func (s *Service) ImportWebCredentialDocumentsWithProgress(ctx context.Context, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	adapter, ok := s.providers.CredentialCodec(accountdomain.ProviderWeb)
	if !ok {
		return ImportResult{}, fmt.Errorf("Grok Web Provider 未注册")
	}
	return s.importCredentialDocumentsWithProgress(ctx, adapter, documents, observer, progress)
}

// ImportConsoleCredentialDocumentsWithProgress 合并解析多个 Console JSON 或 SSO 文本文件，并作为一个批次写入和同步。
func (s *Service) ImportConsoleCredentialDocumentsWithProgress(ctx context.Context, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	adapter, ok := s.providers.CredentialCodec(accountdomain.ProviderConsole)
	if !ok {
		return ImportResult{}, fmt.Errorf("Grok Console Provider 未注册")
	}
	return s.importCredentialDocumentsWithProgress(ctx, adapter, documents, observer, progress)
}

func (s *Service) importCredentialDocumentsWithProgress(ctx context.Context, adapter provider.CredentialCodecAdapter, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	if len(documents) == 0 {
		return ImportResult{}, fmt.Errorf("%w: 没有可导入的账号文件", ErrInvalidImport)
	}
	seeds := make([]provider.CredentialSeed, 0)
	seen := make(map[string]struct{})
	parsedAccounts := 0
	skipped := 0
	for index, document := range documents {
		values, err := adapter.ParseImportedCredentials(document)
		if err != nil {
			if errors.Is(err, provider.ErrCredentialLimit) {
				return ImportResult{}, fmt.Errorf("%w: 单次最多导入 %d 个账号", ErrImportLimit, maxCredentialImportAccounts)
			}
			return ImportResult{}, fmt.Errorf("%w: 第 %d 个文件: %v", ErrInvalidImport, index+1, err)
		}
		parsedAccounts += len(values)
		if parsedAccounts > maxCredentialImportAccounts {
			return ImportResult{}, fmt.Errorf("%w: 单次最多导入 %d 个账号", ErrImportLimit, maxCredentialImportAccounts)
		}
		for _, value := range values {
			if value.SourceKey != "" {
				key := string(value.Provider) + "\x00" + value.SourceKey
				if _, exists := seen[key]; exists {
					skipped++
					continue
				}
				seen[key] = struct{}{}
			}
			seeds = append(seeds, value)
		}
	}
	// Preflight avoids unnecessary OAuth preparation for known deletions.
	// ImportAccounts rechecks current intent inside the actual write transaction.
	tombstoned, err := s.accounts.TombstonedEmails(ctx, seedEmails(seeds))
	if err != nil {
		return ImportResult{}, fmt.Errorf("读取账号墓碑失败: %w", err)
	}
	if len(tombstoned) > 0 {
		kept := seeds[:0]
		for _, seed := range seeds {
			if _, hit := tombstoned[accountdomain.ImportEmail(seed.Email)]; hit {
				skipped++
				if s.logger != nil {
					s.logger.Info("account_import_tombstoned_skipped", "email_hash", tokenhash.HashToken(seed.Email), "name", seed.Name)
				}
				continue
			}
			kept = append(kept, seed)
		}
		seeds = kept
	}
	var result ImportResult
	if preparer, ok := adapter.(provider.CredentialImportPreparer); ok && hasRefreshTokenOnlySeed(seeds) {
		result, err = s.persistPreparedImportedSeeds(ctx, seeds, preparer, observer, progress)
	} else {
		result, err = s.persistImportedSeeds(ctx, seeds, observer, progress)
	}
	result.Skipped += skipped
	return result, err
}

// seedEmails 提取导入种子的邮箱集合(去空)。
func seedEmails(seeds []provider.CredentialSeed) []string {
	out := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		if email := strings.TrimSpace(seed.Email); email != "" {
			out = append(out, email)
		}
	}
	return out
}

func hasRefreshTokenOnlySeed(seeds []provider.CredentialSeed) bool {
	for _, seed := range seeds {
		if strings.TrimSpace(seed.AccessToken) == "" && strings.TrimSpace(seed.RefreshToken) != "" {
			return true
		}
	}
	return false
}

func (s *Service) persistPreparedImportedSeeds(ctx context.Context, seeds []provider.CredentialSeed, preparer provider.CredentialImportPreparer, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	result := ImportResult{AccountIDs: make([]uint64, 0, len(seeds))}
	if progress != nil {
		if err := progress(0, len(seeds)); err != nil {
			return result, err
		}
	}
	prepareCtx, cancelPrepare := context.WithCancel(ctx)
	defer cancelPrepare()
	var (
		mu        sync.Mutex
		firstErr  error
		completed int
		persisted bool
		seen      = make(map[string]struct{}, len(seeds))
	)
	_, batchErr := batch.ForEachObserved(prepareCtx, seeds, batch.Options{Workers: credentialImportPrepareWorkers}, func(itemCtx context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
		if strings.TrimSpace(seed.AccessToken) == "" && strings.TrimSpace(seed.RefreshToken) != "" {
			return preparer.PrepareImportedCredential(itemCtx, seed)
		}
		return seed, nil
	}, func(index int, item batch.Result[provider.CredentialSeed]) {
		mu.Lock()
		defer mu.Unlock()
		completed++
		if !item.Completed || item.Err != nil {
			result.Failed++
			if item.Err != nil {
				s.logger.Warn("account_rt_import_failed", "index", index+1, "error", item.Err)
			}
			reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
			return
		}
		seed := item.Value
		if seed.SourceKey != "" {
			key := string(seed.Provider) + "\x00" + seed.SourceKey
			if _, exists := seen[key]; exists {
				result.Skipped++
				reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
				return
			}
			seen[key] = struct{}{}
		}

		// OAuth providers may invalidate the submitted refresh token as soon as
		// they return its replacement. Persist that replacement before any
		// request-scoped observer or progress callback can abort the import.
		persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
		stored, err := s.persistImportedSeed(persistCtx, seed)
		cancelPersist()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			cancelPrepare()
			return
		}
		if stored.Skipped != "" {
			result.Skipped++
			reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
			return
		}
		persisted = true
		result.AccountIDs = append(result.AccountIDs, stored.ID)
		if stored.Created {
			result.Created++
		} else {
			result.Updated++
		}
		if firstErr == nil && observer != nil {
			if err := observer(stored.ID); err != nil {
				firstErr = err
				cancelPrepare()
			}
		}
		reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
	})
	if persisted {
		s.invalidateBuildBotFlagCache()
		s.WakeCredentialRefresh()
	}
	return result, errors.Join(firstErr, batchErr)
}

func reportCredentialImportProgress(progress BatchProgressObserver, completed, total int, firstErr *error, cancel context.CancelFunc) {
	if progress == nil || *firstErr != nil {
		return
	}
	if err := progress(completed, total); err != nil {
		*firstErr = err
		cancel()
	}
}

func (s *Service) persistImportedSeed(ctx context.Context, seed provider.CredentialSeed) (repository.AccountUpsertResult, error) {
	value, err := s.credentialFromSeed(seed)
	if err != nil {
		return repository.AccountUpsertResult{}, err
	}
	stored, err := s.accounts.ImportAccounts(ctx, []repository.AccountImport{{Credential: value}})
	if err != nil {
		return repository.AccountUpsertResult{}, err
	}
	if len(stored) != 1 {
		return repository.AccountUpsertResult{}, fmt.Errorf("导入账号持久化结果数量无效: %d", len(stored))
	}
	if stored[0].Skipped == "" {
		s.reconcileProviderLinksBestEffort(ctx, stored[0].ID)
	}
	return stored[0], nil
}

func (s *Service) persistImportedSeeds(ctx context.Context, seeds []provider.CredentialSeed, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.persistImportedSeedsWithSources(ctx, seeds, nil, observer, progress)
}

func (s *Service) persistImportedSeedsWithSources(ctx context.Context, seeds []provider.CredentialSeed, sources []accountdomain.CredentialRef, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	if sources != nil && len(sources) != len(seeds) {
		return ImportResult{}, fmt.Errorf("导入账号与来源数量不一致")
	}
	completed, total := 0, len(seeds)
	result := ImportResult{AccountIDs: make([]uint64, 0, len(seeds))}
	defer func() {
		if result.Created+result.Updated > 0 {
			s.invalidateBuildBotFlagCache()
			s.WakeCredentialRefresh()
		}
	}()
	if progress != nil {
		if err := progress(completed, total); err != nil {
			return result, err
		}
	}
	for start := 0; start < len(seeds); start += credentialImportChunkSize {
		end := min(start+credentialImportChunkSize, len(seeds))
		values := make([]repository.AccountImport, 0, end-start)
		for index, seed := range seeds[start:end] {
			value, err := s.credentialFromSeed(seed)
			if err != nil {
				return result, err
			}
			input := repository.AccountImport{Credential: value}
			if sources != nil {
				ref := sources[start+index]
				input.Source = &ref
			}
			values = append(values, input)
		}
		stored, err := s.accounts.ImportAccounts(ctx, values)
		if err != nil {
			return result, err
		}
		if len(stored) != len(values) {
			return result, fmt.Errorf("导入账号持久化结果数量无效: %d", len(stored))
		}
		// The whole chunk committed before callbacks. Preserve all committed
		// counts even if an observer/progress callback cancels delivery.
		for _, value := range stored {
			if value.Skipped != "" {
				result.Skipped++
				continue
			}
			result.AccountIDs = append(result.AccountIDs, value.ID)
			if value.Created {
				result.Created++
			} else {
				result.Updated++
			}
		}
		for _, value := range stored {
			if value.Skipped == "" {
				s.reconcileProviderLinksBestEffort(ctx, value.ID)
				if observer != nil {
					if err := observer(value.ID); err != nil {
						return result, err
					}
				}
			}
			completed++
			if progress != nil {
				if err := progress(completed, total); err != nil {
					return result, err
				}
			}
		}
	}
	return result, nil
}
