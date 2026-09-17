package accountsync

import (
	"context"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// Onboarding coordinates committed imports/conversions with initial snapshots.
// Each request owns its queue: cancellation joins all work without undoing accounts.
type Onboarding struct {
	importer  accountapp.CredentialTransfer
	converter accountapp.Conversion
	sync      Synchronizer
}

func NewOnboarding(importer accountapp.CredentialTransfer, converter accountapp.Conversion, syncer Synchronizer) *Onboarding {
	return &Onboarding{importer: importer, converter: converter, sync: syncer}
}

func (s *Onboarding) Import(ctx context.Context, provider accountdomain.Provider, documents [][]byte, progress accountapp.BatchProgressObserver, syncProgress func(int, int)) (accountapp.ImportResult, Result, error) {
	pipeline := startSyncPipeline(ctx, s.sync, syncProgress)
	var result accountapp.ImportResult
	var err error
	switch provider {
	case accountdomain.ProviderWeb:
		result, err = s.importer.ImportWebCredentialDocumentsWithProgress(pipeline.ctx, documents, pipeline.Observe, progress)
	case accountdomain.ProviderConsole:
		result, err = s.importer.ImportConsoleCredentialDocumentsWithProgress(pipeline.ctx, documents, pipeline.Observe, progress)
	default:
		result, err = s.importer.ImportCredentialDocumentsWithProgress(pipeline.ctx, documents, pipeline.Observe, progress)
	}
	return result, pipeline.Finish(err != nil), err
}

func (s *Onboarding) SyncToConsole(ctx context.Context, all bool, ids []uint64, strategy accountapp.WebConsoleSyncStrategy, progress accountapp.BatchProgressObserver, syncProgress func(completed, total int)) (accountapp.ImportResult, Result, error) {
	pipeline := startSyncPipeline(ctx, s.sync, syncProgress)
	var (
		result accountapp.ImportResult
		err    error
	)
	if all {
		result, err = s.converter.SyncAllWebAccountsToConsoleWithStrategy(pipeline.ctx, strategy, pipeline.Observe, progress)
	} else {
		result, err = s.converter.SyncWebAccountsToConsoleWithStrategy(pipeline.ctx, ids, strategy, pipeline.Observe, progress)
	}
	syncResult := pipeline.Finish(err != nil)
	return result, syncResult, err
}

func (s *Onboarding) ConvertToBuild(ctx context.Context, all bool, ids []uint64, strategy accountapp.BuildConversionStrategy, progress accountapp.BatchProgressObserver, syncProgress func(completed, total int)) (accountapp.BuildConversionResult, Result, error) {
	pipeline := startSyncPipeline(ctx, s.sync, syncProgress)
	var (
		result accountapp.BuildConversionResult
		err    error
	)
	if all {
		result, err = s.converter.ConvertAllWebAccountsToBuildWithStrategy(pipeline.ctx, strategy, pipeline.Observe, progress)
	} else {
		result, err = s.converter.ConvertWebAccountsToBuildWithStrategy(pipeline.ctx, ids, strategy, pipeline.Observe, progress)
	}
	syncResult := pipeline.Finish(err != nil)
	return result, syncResult, err
}
