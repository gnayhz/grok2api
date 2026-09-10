package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// AccountUpdates 表示批量账号更新中允许持久化的字段。
type AccountUpdates struct {
	Enabled          *bool
	Priority         *int
	MaxConcurrent    *int
	MinimumRemaining *float64
}

// AccountAdminPatch contains only the fields explicitly changed by an
// administrator. Omitted fields retain their committed values. Credential
// rotation, authentication, health and upstream observations have separate owners.
type AccountAdminPatch struct {
	AccountUpdates
	Name                      *string
	Risk                      *RiskAttribution
	EncryptedCloudflareCookie *string
	BuildSuperEntitled        *bool
	BuildRouteMode            *account.BuildRouteMode
}

type AccountAdminResult struct {
	Credential     account.Credential
	EnabledChanged bool
}

type AccountUpsertResult struct {
	ID      uint64
	Created bool
	Skipped account.ImportSkipReason
	// Material is the exact reference committed by this import, not a later read.
	Material account.CredentialRef
}

// AccountImport installs explicit credential material. A source identifies the
// exact existing account used for a provider conversion/synchronization; it must
// still exist at that generation when the destination is written.
type AccountImport struct {
	Credential account.Credential
	Source     *account.CredentialRef
	// Target fences replacement of a previously observed linked credential.
	Target *account.CredentialRef
}

// QuotaSnapshotWrite atomically installs a Provider observation only if no
// intervening quota write changed Revision. A full or group replacement also
// removes absent modes and resolves previously accepted consumption facts.
type QuotaSnapshotWrite struct {
	AccountID    uint64
	Revision     uint64
	Tier         account.WebTier
	SyncedAt     time.Time
	Windows      []account.QuotaWindow
	ReplaceAll   bool
	ReplaceModes []string
}

// RiskAttribution is the column-targeted write for long-term risk flags plus
// provenance (trigger/origin/detail). Empty Status clears the flag and meta.
type RiskAttribution struct {
	Status          string
	Trigger         string
	OriginAccountID uint64
	CheckedAt       *time.Time
	Detail          string
}

// BuildBotFlagCredential is the minimal encrypted credential projection used to
// rebuild persisted Build bot-risk metadata outside the request path.
type BuildBotFlagCredential struct {
	AccountID            uint64
	EncryptedAccessToken string
	StoredSource         int
}

type BuildBotFlagSourceUpdate struct {
	AccountID                    uint64
	ExpectedEncryptedAccessToken string
	Source                       int
}

// LinkedDeleteResolution is the server-side expansion of root deletes with optional linked peers.
type LinkedDeleteResolution struct {
	RootIDs          []uint64
	FinalIDs         []uint64
	LinkedByProvider map[account.Provider]int
	// RootGroups maps each root to its final peers and defines the media-skip group boundary.
	RootGroups map[uint64][]uint64
	// PeerProviders records each peer provider for post-delete accounting.
	PeerProviders map[uint64]account.Provider
}

// LinkedDeleteOutcome contains actual rows deleted and root groups skipped atomically.
type LinkedDeleteOutcome struct {
	Resolution              LinkedDeleteResolution
	DeletedIDs              []uint64
	Deleted                 int64
	RootsDeleted            int64
	LinkedDeletedByProvider map[account.Provider]int64
	// SkippedRoots identifies groups protected by queued or in-progress video jobs.
	SkippedRoots []uint64
}

// CleanupPreview contains COUNT-only values for the cleanup confirmation dialog.
type CleanupPreview struct {
	RootsByStatus    map[string]int64
	RootCount        int64
	LinkedByProvider map[account.Provider]int64
	Total            int64
}

// ObservedModelWriter reports whether an observed model update changed the authoritative row.
type ObservedModelWriter interface {
	UpdateObservedModelIfNewer(ctx context.Context, id uint64, model string, observedAt time.Time) (bool, error)
}

// RoutingLayerRepository separates reusable account state from model overlays.
type RoutingLayerRepository interface {
	ListRoutingAccountBases(ctx context.Context, provider account.Provider, quotaMode string) ([]account.RoutingAccountBase, error)
	ListRoutingAccountOverlays(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel string) (account.RoutingOverlaySnapshot, error)
}

// AccountRepository 定义 OAuth 账号和额度快照持久化能力。
type AccountRepository interface {
	ApplyQuotaRecovery(ctx context.Context, ref account.QuotaRecoveryRef, event account.RecoveryEvent) (account.RecoveryResult, error)
	ApplyCredential(ctx context.Context, ref account.CredentialRef, event account.CredentialEvent) (account.CredentialResult, error)
	List(ctx context.Context, query AccountListQuery) ([]account.Credential, int64, error)
	// TombstonedEmails 报告给定 email 集中已墓碑(手动删除)的子集。
	TombstonedEmails(ctx context.Context, emails []string) (map[string]struct{}, error)
	// ClearTombstones 清除墓碑(恢复重新导入通道)。
	ClearTombstones(ctx context.Context, emails []string) (int64, error)
	// ListProviderAccountBatch 以 ID 游标取一批账号；total 仅在 afterID 为 0 时返回。
	ListProviderAccountBatch(ctx context.Context, provider account.Provider, afterID uint64, limit int) ([]account.Credential, int64, error)
	Summarize(ctx context.Context, now time.Time) ([]AccountSummary, error)
	ListEnabled(ctx context.Context, provider account.Provider) ([]account.Credential, error)
	ListEnabledAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error)
	// ListEnabledCredentialRefreshAccountIDs includes enabled active and
	// reauthRequired accounts so an explicit administrator refresh can retry a
	// previously rejected refresh token once.
	ListEnabledCredentialRefreshAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error)
	CountProviderAccountsByIDs(ctx context.Context, provider account.Provider, ids []uint64) (int64, error)
	// CountAvailableAmong counts how many of the given account IDs currently match the
	// same "available/schedulable" predicate used by Summarize for the provider.
	CountAvailableAmong(ctx context.Context, provider account.Provider, ids []uint64, now time.Time) (int64, error)
	// FilterMissingBuildConversionIDs 从指定账号中排除已经关联 Build 的 Web 账号。
	FilterMissingBuildConversionIDs(ctx context.Context, ids []uint64) ([]uint64, error)
	// ListUnlinkedWebAccountIDs 以 ID 游标取未关联 Web 账号；total 仅在 afterID 为 0 时返回。
	ListUnlinkedWebAccountIDs(ctx context.Context, afterID uint64, limit int) ([]uint64, int64, error)
	// ListMissingConsoleSyncAccounts 从指定账号中排除已有对应 Console 账号的 Web 账号。
	ListMissingConsoleSyncAccounts(ctx context.Context, ids []uint64) ([]account.Credential, error)
	// ListMissingConsoleSyncBatch 以 ID 游标取缺少 Console 账号的 Web 账号；total/skipped 仅在 afterID 为 0 时返回。
	ListMissingConsoleSyncBatch(ctx context.Context, afterID uint64, limit int) ([]account.Credential, int64, int64, error)
	HasActive(ctx context.Context, provider account.Provider) (bool, error)
	ListRoutingCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string) ([]account.RoutingCandidate, error)
	// GetRoutingCandidate returns current secret-free facts for one physical
	// claim from a consistent read snapshot; no provider cache or stale fallback.
	GetRoutingCandidate(ctx context.Context, accountID uint64, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string) (account.RoutingCandidate, error)
	GetCredentialMaterial(ctx context.Context, accountID uint64, provider account.Provider) (account.CredentialMaterial, error)
	Get(ctx context.Context, id uint64) (account.Credential, error)
	LinkWebToBuild(ctx context.Context, web, build account.CredentialRef) error
	GetBillings(ctx context.Context, accountIDs []uint64) (map[uint64]account.Billing, error)
	GetQuotaRecoveries(ctx context.Context, accountIDs []uint64) (map[uint64]account.QuotaRecovery, error)
	UpsertByIdentity(ctx context.Context, value account.Credential) (account.Credential, bool, error)
	// ImportAccounts atomically checks current deletion policy and optional
	// source references, then installs eligible material. One result per input,
	// in order; skipped entries have no ID and perform no account mutation.
	ImportAccounts(ctx context.Context, inputs []AccountImport) ([]AccountUpsertResult, error)
	UpdateAdministration(ctx context.Context, id uint64, patch AccountAdminPatch) (AccountAdminResult, error)
	UpdateMany(ctx context.Context, provider account.Provider, ids []uint64, updates AccountUpdates) (int64, error)
	Delete(ctx context.Context, id uint64) error
	DeleteMany(ctx context.Context, ids []uint64) (int64, error)
	// ResolveLinkedDeleteIDs expands root account IDs with one-hop (or Build/Console two-hop via Web)
	// peers from link tables for optional linked deletion. It never guesses by email/name/userId.
	ResolveLinkedDeleteIDs(ctx context.Context, provider account.Provider, rootIDs []uint64, targets []account.Provider) (LinkedDeleteResolution, error)
	// DeleteManyWithLinked locks roots, resolves linked peers, checks media jobs, and deletes
	// the final set and its email tombstones inside a single DB transaction.
	// skipMedia=false rejects on active media; skipMedia=true skips the complete root group.
	DeleteManyWithLinked(ctx context.Context, provider account.Provider, rootIDs []uint64, targets []account.Provider, skipMedia bool) (LinkedDeleteOutcome, error)
	// DeleteAccountStatusBatchWithLinked selects at most limit roots by state and ID cursor,
	// expands links, skips protected groups, and commits deletion plus tombstones.
	// Returns the candidate count and next cursor; background auto-clean is separate.
	DeleteAccountStatusBatchWithLinked(ctx context.Context, provider account.Provider, status string, now time.Time, afterID uint64, limit int, targets []account.Provider) (LinkedDeleteOutcome, int, uint64, error)
	// CountCleanupWithLinked returns root and linked-peer counts using SQL COUNT queries only.
	CountCleanupWithLinked(ctx context.Context, provider account.Provider, statuses []string, now time.Time, targets []account.Provider) (CleanupPreview, error)
	// ListAutoCleanReauthCandidates 以 ID 游标列出达到清理年龄的 reauthRequired 账号。
	ListAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, afterID uint64, limit int) ([]uint64, error)
	// DeleteAutoCleanReauthCandidates 在事务内重新校验状态与年龄并跳过活动视频任务，返回实际删除 ID。
	DeleteAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, candidateIDs []uint64) ([]uint64, error)
	BackfillCredentialRefreshSchedules(ctx context.Context, now time.Time, limit int) (int, error)
	ListCriticalCredentialRefreshIDs(ctx context.Context, now, expiresBefore time.Time, limit int) ([]uint64, error)
	ListDueCredentialRefreshIDs(ctx context.Context, now time.Time, limit int) ([]uint64, error)
	NextCredentialRefreshDueAt(ctx context.Context) (*time.Time, error)
	UpdateObservedModel(ctx context.Context, id uint64, model string, observedAt time.Time) error
	ApplyHealth(ctx context.Context, id uint64, provider account.Provider, event account.HealthEvent) (account.HealthResult, error)
	// UpdateRiskAttribution 写入风控标记及来源元数据（巡检/降智/人工）。
	UpdateRiskAttribution(ctx context.Context, id uint64, attr RiskAttribution) error

	// TouchLastUsed persists request activity without changing routing health or
	// invalidating candidate snapshots.
	TouchLastUsed(ctx context.Context, id uint64, usedAt time.Time) error
	// MarkBuildAPIFallback 幂等写入 Build 账号的 XAI 推理回退标记；非 Build 账号返回错误。
	MarkBuildAPIFallback(ctx context.Context, id uint64, enabled bool) error
	ApplyWebProfile(ctx context.Context, observed account.CredentialRef, event account.WebProfileObservation) (account.WebProfileResult, error)
	ApplyModelRestriction(ctx context.Context, ref account.QuotaRecoveryRef, event account.ModelRestrictionEvent) (account.ModelRestrictionResult, error)
	PruneExpiredModelQuotaBlocks(ctx context.Context, now time.Time, limit int) (int64, error)
	GetBilling(ctx context.Context, accountID uint64) (account.Billing, error)
	GetQuotaRecovery(ctx context.Context, accountID uint64) (account.QuotaRecovery, error)
	ResetQuotaState(ctx context.Context, provider account.Provider, accountIDs []uint64) error
	ResetProviderQuotaState(ctx context.Context, provider account.Provider, activeOnly bool) (int64, error)
	HasQuotaWindows(ctx context.Context, accountID uint64) (bool, error)
	GetQuotaWindows(ctx context.Context, accountIDs []uint64) (map[uint64][]account.QuotaWindow, error)
	GetQuotaRevision(ctx context.Context, accountID uint64) (uint64, error)
	SaveQuotaSnapshot(ctx context.Context, value QuotaSnapshotWrite) error
	ConsumeQuota(ctx context.Context, value account.QuotaConsumption, now time.Time) (account.QuotaConsumptionReceipt, error)
	ListPendingQuotaRefreshes(ctx context.Context, afterID uint64, limit int) ([]account.PendingQuotaRefresh, error)
	ResolveDeletedQuotaConsumptions(ctx context.Context, accountID uint64, now time.Time) error
	UpsertManyByIdentity(ctx context.Context, values []account.Credential) ([]AccountUpsertResult, error)
	ExhaustQuotaWindow(ctx context.Context, accountID uint64, mode string, resetAt *time.Time, now time.Time) error
	ListDueQuotaWindows(ctx context.Context, now time.Time, limit int) ([]account.QuotaWindow, error)
	ListQuotaRecoveryWindows(ctx context.Context, limit int) ([]account.QuotaWindow, error)
	ListStaleWebQuotaAccountIDs(ctx context.Context, before time.Time, limit int) ([]uint64, error)
}
