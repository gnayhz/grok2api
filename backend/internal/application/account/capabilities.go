package account

import (
	"context"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// These capabilities share the account lifecycle and conditional-write owner.
// Consumers receive only their use-case surface; Service is assembled once by app.
type Administration interface {
	Identities(ctx context.Context, ids []uint64) ([]accountdomain.Identity, error)
	List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]View, int64, error)
	Summary(ctx context.Context) (Summary, error)
	Get(ctx context.Context, id uint64) (View, error)
	Update(ctx context.Context, id uint64, input UpdateInput) (View, error)
	BatchUpdate(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, input UpdateInput) (int64, error)
	Delete(ctx context.Context, id uint64) error
	DeleteWithLinked(ctx context.Context, providerValue accountdomain.Provider, id uint64, targets []accountdomain.Provider) (AccountDeleteResult, error)
	BatchDeleteWithLinked(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, targets []accountdomain.Provider) (AccountDeleteResult, error)
	PreviewLinkedDelete(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, targets []accountdomain.Provider) (repository.LinkedDeleteResolution, error)
	CleanupAccounts(ctx context.Context, providerValue accountdomain.Provider, statuses []CleanupStatus, targets []accountdomain.Provider) (CleanupResult, error)
	PreviewCleanup(ctx context.Context, providerValue accountdomain.Provider, statuses []CleanupStatus, targets []accountdomain.Provider) (repository.CleanupPreview, error)
	AccountsBelongToProvider(ctx context.Context, ids []uint64, providerValue accountdomain.Provider) (bool, error)
	ClearCooldown(ctx context.Context, id uint64) (View, error)
}

type CredentialTransfer interface {
	ImportCredentialDocumentsWithProgress(ctx context.Context, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error)
	ImportWebCredentialDocumentsWithProgress(ctx context.Context, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error)
	ImportConsoleCredentialDocumentsWithProgress(ctx context.Context, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error)
	ExportProviderCredentials(ctx context.Context, providerValue accountdomain.Provider) (ExportResult, error)
	ExportProviderCredentialsCursor(ctx context.Context, providerValue accountdomain.Provider, afterID, snapshotMaxID uint64, limit int) (ExportPageResult, error)
	ExportProviderCredentialsByIDs(ctx context.Context, providerValue accountdomain.Provider, ids []uint64) (ExportResult, error)
}

type Conversion interface {
	SyncAllWebAccountsToConsoleWithStrategy(ctx context.Context, strategy WebConsoleSyncStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error)
	SyncWebAccountsToConsoleWithStrategy(ctx context.Context, ids []uint64, strategy WebConsoleSyncStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error)
	ConvertAllWebAccountsToBuildWithStrategy(ctx context.Context, strategy BuildConversionStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error)
	ConvertWebAccountsToBuildWithStrategy(ctx context.Context, ids []uint64, strategy BuildConversionStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error)
}

// Maintenance exposes administrator-triggered credential, quota and profile work.
type Maintenance interface {
	AcceptWebTerms(ctx context.Context, id uint64) error
	BatchRefreshBilling(ctx context.Context, ids []uint64) (int, int, error)
	BatchRefreshQuota(ctx context.Context, ids []uint64) (int, int, error)
	BatchRefreshTokens(ctx context.Context, ids []uint64) (int, int, int, error)
	BatchResetQuotaState(ctx context.Context, ids []uint64) (int, error)
	DetectBuildAccountsWithProgress(ctx context.Context, ids []uint64, all bool, progress BatchProgressObserver, itemObserver BuildDetectItemObserver) (int, int, error)
	EnableWebNSFW(ctx context.Context, id uint64) error
	PollDeviceLogin(ctx context.Context, sessionID string) (view View, retErr error)
	RefreshAllTokensWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, int, error)
	RefreshBilling(ctx context.Context, id uint64) (accountdomain.Billing, error)
	RefreshQuota(ctx context.Context, id uint64) ([]accountdomain.QuotaWindow, error)
	RefreshToken(ctx context.Context, id uint64) (View, error)
	ResetAllBuildQuotaState(ctx context.Context) (int64, error)
	RunAllWebAccountScriptsWithProgress(ctx context.Context, options WebAccountScriptOptions, progress BatchProgressObserver) (int, int, error)
	RunWebAccountScriptsWithProgress(ctx context.Context, ids []uint64, options WebAccountScriptOptions, progress BatchProgressObserver) (int, int, error)
	SetWebBirthDate(ctx context.Context, id uint64) error
	StartDeviceLogin(ctx context.Context) (DeviceStartResult, error)
	SyncAllBillingWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, error)
	SyncAllConsoleQuotasWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, error)
	SyncAllWebQuotasWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, error)
}
