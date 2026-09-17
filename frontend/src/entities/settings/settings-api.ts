import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";

export type SettingsConfigDTO = {
  server: { maxConcurrentRequests: number };
  providerBuild: { baseURL: string; fallbackBaseURL: string; clientVersion: string; clientIdentifier: string; tokenAuth: string; tokenAuthConfigured: boolean; userAgent: string; responseHeaderTimeout: string; streamIdleTimeout: string };
  providerWeb: {
    baseURL: string; quotaTimeout: string; chatTimeout: string; streamIdleTimeout: string; imageTimeout: string; videoTimeout: string;
    statsigMode: "manual" | "url"; statsigManualValue?: string; statsigManualConfigured: boolean; statsigSignerURL: string;
    clearanceMode: ClearanceMode; flareSolverrURL: string; clearanceTimeout: string; clearanceRefresh: string;
    mediaConcurrency: number; allowNSFW: boolean;
    recoveryBackoffBase: string; recoveryBackoffMax: string;
  };
  providerConsole: { baseURL: string; chatTimeout: string; streamIdleTimeout: string };
  batch: { importConcurrency: number; conversionConcurrency: number; syncConcurrency: number; refreshConcurrency: number; randomDelay: string };
  media: {
    maxImageBytes: number; maxTotalBytes: number; cleanupThresholdPercent: number;
    cleanupInterval: string;
  };
  frontend: { publicApiBaseURL: string };
  routing: {
    stickyTTL: string; cooldownBase: string; cooldownMax: string; capacityWait: string; maxAttempts: number; videoMaxAttempts: number; preferFreeBuild: boolean; markBuildChatDeniedAsReauth: boolean;
    accountIsolatedConnections: boolean;
    segmentedSelector: { enabled: boolean; minCandidates: number; windowSize: number };
  };
  audit: { bufferSize: number; batchSize: number; flushInterval: string; commitDelayMS: number; retentionDays?: number; retentionPeriod?: string; retentionSource?: string; fileRetentionPeriod?: string; fileRetentionSource?: string };
  clientKeyDefaults: { rpmLimit: number; maxConcurrent: number };
  accounts: {
    markBuildForbiddenReauth: boolean;
    buildForbiddenReauthCodes: string[];
    excludeBuildBotFlaggedFromScheduling: boolean;
    autoCleanReauthEnabled: boolean;
    autoCleanReauthInterval: string;
    autoCleanReauthMinAge: string;
    autoCleanIncludeDisabled: boolean;
  };
  // 旧后端不返回这两节;withSettingsDefaults 提供本地默认。
  requestRetry?: {
    enabled: boolean; maxAttempts: number; onExhausted: string;
    accountCooldown: string;
    evidenceTimeout: string; createdTimeout: string; idleAccountCooldown: string;
  };
  egressRotation?: {
    enabled: boolean; maxAttemptsPerQuarantine: number; minNodeInterval: string; maxGlobalPerHour: number;
    webhookTimeout: string; webhookRetries: number; settleDelay: string; probeTimeout: string; probeInterval: string;
  };
};

export type ClearanceMode = "manual" | "flaresolverr" | "on_demand";

export type SettingsApplyStatus = {
  name: string; appliedRevision: string; pending: boolean; error?: string; lastAttemptAt?: string;
};
export type SettingsNotificationStatus = {
  revision: string; state: "disabled" | "observed" | "pending" | "published" | "failed";
  error?: string; lastAttemptAt?: string;
};
export type SettingsSnapshotDTO = {
  config: SettingsConfigDTO;
  recommendedProviderBuild: { clientVersion: string; userAgent: string };
  updatedAt: string;
  revision: string;
  restartRequired: string[];
  // Optional during rolling upgrades: missing metadata means unknown status.
  appliedRevision?: string;
  applyPending?: boolean;
  applyTargets?: SettingsApplyStatus[];
  notification?: SettingsNotificationStatus;
  /** 文件配置基线的 requestRetry 节:与 config.requestRetry 不同即处于运行时覆盖态。 */
  fileRequestRetry?: NonNullable<SettingsConfigDTO["requestRetry"]>;
};

const settingsConfigValidator = hasShape({
  server: hasShape({ maxConcurrentRequests: isNumber }),
  providerBuild: hasShape({ baseURL: isString, fallbackBaseURL: isString, clientVersion: isString, clientIdentifier: isString, tokenAuth: isString, tokenAuthConfigured: isBoolean, userAgent: isString, responseHeaderTimeout: isString, streamIdleTimeout: isString }),
  providerWeb: hasShape({
    baseURL: isString, quotaTimeout: isString, chatTimeout: isString, streamIdleTimeout: isOptional(isString), imageTimeout: isString, videoTimeout: isString,
    statsigMode: isOneOf("manual", "url"), statsigManualValue: isOptional(isString), statsigManualConfigured: isBoolean,
    statsigSignerURL: isString, clearanceMode: isOneOf("manual", "flaresolverr", "on_demand"), flareSolverrURL: isString,
    clearanceTimeout: isString, clearanceRefresh: isString, mediaConcurrency: isNumber, allowNSFW: isBoolean, recoveryBackoffBase: isString, recoveryBackoffMax: isString,
  }),
  providerConsole: hasShape({ baseURL: isString, chatTimeout: isString, streamIdleTimeout: isOptional(isString) }),
  batch: hasShape({ importConcurrency: isNumber, conversionConcurrency: isNumber, syncConcurrency: isNumber, refreshConcurrency: isNumber, randomDelay: isString }),
  media: hasShape({ maxImageBytes: isNumber, maxTotalBytes: isNumber, cleanupThresholdPercent: isNumber, cleanupInterval: isString }),
  frontend: hasShape({ publicApiBaseURL: isString }),
  routing: hasShape({
    stickyTTL: isString, cooldownBase: isString, cooldownMax: isString, capacityWait: isString, maxAttempts: isNumber, videoMaxAttempts: isNumber, preferFreeBuild: isBoolean, markBuildChatDeniedAsReauth: isBoolean,
    accountIsolatedConnections: isOptional(isBoolean),
    segmentedSelector: isOptional(hasShape({ enabled: isBoolean, minCandidates: isNumber, windowSize: isNumber })),
  }),
  audit: hasShape({
    bufferSize: isNumber, batchSize: isNumber, flushInterval: isString, commitDelayMS: isOptional(isNumber),
    retentionDays: isOptional(isNumber), retentionPeriod: isOptional(isString), retentionSource: isOptional(isString),
    fileRetentionPeriod: isOptional(isString), fileRetentionSource: isOptional(isString),
  }),
  clientKeyDefaults: hasShape({ rpmLimit: isNumber, maxConcurrent: isNumber }),
  // Older backends may omit accounts; withSettingsDefaults supplies a safe local default.
  accounts: isOptional(hasShape({
    markBuildForbiddenReauth: isOptional(isBoolean),
    buildForbiddenReauthCodes: isOptional(isArrayOf(isString)),
    excludeBuildBotFlaggedFromScheduling: isOptional(isBoolean),
    autoCleanReauthEnabled: isBoolean,
    autoCleanReauthInterval: isString,
    autoCleanReauthMinAge: isString,
    autoCleanIncludeDisabled: isBoolean,
  })),
  requestRetry: isOptional(hasShape({
    enabled: isBoolean, maxAttempts: isNumber, onExhausted: isString,
    accountCooldown: isString,
    evidenceTimeout: isString, createdTimeout: isString, idleAccountCooldown: isString,
  })),
  egressRotation: isOptional(hasShape({
    enabled: isBoolean, maxAttemptsPerQuarantine: isNumber, minNodeInterval: isString, maxGlobalPerHour: isNumber,
    webhookTimeout: isString, webhookRetries: isNumber, settleDelay: isString, probeTimeout: isString, probeInterval: isString,
  })),
});
const defaultAccountsConfig = (): SettingsConfigDTO["accounts"] => ({
  markBuildForbiddenReauth: false,
  buildForbiddenReauthCodes: ["permission-denied"],
  excludeBuildBotFlaggedFromScheduling: false,
  autoCleanReauthEnabled: false,
  autoCleanReauthInterval: "10m",
  autoCleanReauthMinAge: "1h",
  autoCleanIncludeDisabled: false,
});
const defaultRequestRetryConfig = (): NonNullable<SettingsConfigDTO["requestRetry"]> => ({
  enabled: true, maxAttempts: 2, onExhausted: "fail_closed",
  accountCooldown: "24h",
  evidenceTimeout: "3.5s", createdTimeout: "5s", idleAccountCooldown: "15m",
});
export const defaultEgressRotationConfig = (): NonNullable<SettingsConfigDTO["egressRotation"]> => ({
  enabled: true, maxAttemptsPerQuarantine: 3, minNodeInterval: "3m", maxGlobalPerHour: 6,
  webhookTimeout: "15s", webhookRetries: 2, settleDelay: "20s", probeTimeout: "2m", probeInterval: "5s",
});
function withSettingsDefaults(snapshot: SettingsSnapshotDTO): SettingsSnapshotDTO {
  const accounts = snapshot.config.accounts ?? defaultAccountsConfig();
  const requestRetryRaw = snapshot.config.requestRetry ?? defaultRequestRetryConfig();
  const requestRetry = {
    ...defaultRequestRetryConfig(),
    ...requestRetryRaw,
  };
  const egressRotation = snapshot.config.egressRotation ?? defaultEgressRotationConfig();
  const segmentedSelector = snapshot.config.routing.segmentedSelector ?? { enabled: true, minCandidates: 3000, windowSize: 64 };
  return {
    ...snapshot,
    config: {
      ...snapshot.config,
      providerWeb: {
        ...snapshot.config.providerWeb,
        streamIdleTimeout: snapshot.config.providerWeb.streamIdleTimeout || "1m30s",
      },
      providerConsole: {
        ...snapshot.config.providerConsole,
        streamIdleTimeout: snapshot.config.providerConsole.streamIdleTimeout || "2m",
      },
      audit: {
        ...snapshot.config.audit,
        commitDelayMS: snapshot.config.audit.commitDelayMS ?? 5,
        retentionPeriod: snapshot.config.audit.retentionPeriod ?? `${(snapshot.config.audit.retentionDays ?? 7) * 24}h`,
      },
      routing: {
        ...snapshot.config.routing,
        markBuildChatDeniedAsReauth: snapshot.config.routing.markBuildChatDeniedAsReauth ?? false,
        accountIsolatedConnections: snapshot.config.routing.accountIsolatedConnections ?? false,
        segmentedSelector: {
          enabled: segmentedSelector.enabled ?? true,
          minCandidates: segmentedSelector.minCandidates || 3000,
          windowSize: segmentedSelector.windowSize || 64,
        },
      },
      accounts: {
        markBuildForbiddenReauth: accounts.markBuildForbiddenReauth ?? false,
        buildForbiddenReauthCodes: accounts.buildForbiddenReauthCodes ?? ["permission-denied"],
        excludeBuildBotFlaggedFromScheduling: accounts.excludeBuildBotFlaggedFromScheduling ?? false,
        autoCleanReauthEnabled: accounts.autoCleanReauthEnabled ?? false,
        autoCleanReauthInterval: accounts.autoCleanReauthInterval || "10m",
        autoCleanReauthMinAge: accounts.autoCleanReauthMinAge || "1h",
        autoCleanIncludeDisabled: accounts.autoCleanIncludeDisabled ?? false,
      },
      requestRetry,
      egressRotation,
    },
  };
}
const decodeSettingsSnapshotRaw = createObjectDecoder<SettingsSnapshotDTO>("settings", {
  config: settingsConfigValidator,
  recommendedProviderBuild: hasShape({ clientVersion: isString, userAgent: isString }),
  updatedAt: isString,
  revision: isString,
  restartRequired: isArrayOf(isString),
  appliedRevision: isOptional(isString),
  applyPending: isOptional(isBoolean),
  applyTargets: isOptional(isArrayOf(hasShape({ name: isString, appliedRevision: isString, pending: isBoolean, error: isOptional(isString), lastAttemptAt: isOptional(isString) }))),
  notification: isOptional(hasShape({ revision: isString, state: isOneOf("disabled", "observed", "pending", "published", "failed"), error: isOptional(isString), lastAttemptAt: isOptional(isString) })),
  fileRequestRetry: isOptional(hasShape({
    enabled: isBoolean, maxAttempts: isNumber, onExhausted: isString,
    accountCooldown: isString,
    evidenceTimeout: isString, createdTimeout: isString, idleAccountCooldown: isString,
  })),
});
const decodeSettingsSnapshot = (value: unknown) => withSettingsDefaults(decodeSettingsSnapshotRaw(value));
export function getSettings(signal?: AbortSignal): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", { signal }, decodeSettingsSnapshot);
}

export function updateSettings(revision: string, config: SettingsConfigDTO, signal?: AbortSignal): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", { method: "PUT", body: { revision, config }, signal }, decodeSettingsSnapshot);
}

// resetSettings 移除运行覆盖并推进持久版本：可编辑字段恢复以 config.yaml
// 为默认（后台保存过的覆盖被移除），返回重置后的快照。
export function resetSettings(revision: string, signal?: AbortSignal): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", { method: "DELETE", body: { revision }, signal }, decodeSettingsSnapshot);
}

export function resetSettingsRotation(revision: string, signal?: AbortSignal): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings/egress-rotation/reset", { method: "POST", body: { revision }, signal }, decodeSettingsSnapshot);
}
